package main

import (
	"errors"
	"net"
	"net/http"
	"sync"
	"time"
)

type loginField struct {
	Name        string `json:"name"`
	Label       string `json:"label"`
	Type        string `json:"type"`
	Required    bool   `json:"required,omitempty"`
	Placeholder string `json:"placeholder,omitempty"`
	Help        string `json:"help,omitempty"`
}

type loginSession struct {
	ID           string
	Method       string
	Status       string
	AccountID    string
	Error        string
	URL          string
	Instructions string
	Fields       []loginField
	CreatedAt    int64
	ExpiresAt    int64

	port      int
	verifier  string
	challenge string
	ticket    string
	key       *dpopKey
	proxyURL  string
	// Identity the credential endpoint expects for this ticket: the portal
	// channel uses snap_AIIDE, the IDE plugin channel snap_jetbrains.
	ticketPluginName    string
	ticketPluginVersion string
	listener            net.Listener
	server              *http.Server
	closeOnce           sync.Once
	mu                  sync.Mutex
}

func (s *loginSession) closeListener() {
	s.closeOnce.Do(func() {
		if s.server != nil {
			_ = s.server.Close()
		}
		if s.listener != nil {
			_ = s.listener.Close()
		}
	})
}

func (s *loginSession) snapshot() (status, accountID, errMsg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Status, s.AccountID, s.Error
}

func (p *Plugin) scheduleSessionCleanup(id string) {
	time.AfterFunc(10*time.Minute, func() {
		p.sessionsMu.Lock()
		delete(p.sessions, id)
		p.sessionsMu.Unlock()
		p.dropTransport(id)
	})
}

// liveLoginSession returns an open session for the method, if any.
func (p *Plugin) liveLoginSession(method string) *loginSession {
	now := p.now()
	p.sessionsMu.Lock()
	defer p.sessionsMu.Unlock()
	var best *loginSession
	for _, s := range p.sessions {
		if s.Method != method {
			continue
		}
		s.mu.Lock()
		status := s.Status
		s.mu.Unlock()
		if (status != "pending" && status != "need_input") || now.Unix() > s.ExpiresAt {
			continue
		}
		if best == nil || s.CreatedAt > best.CreatedAt {
			best = s
		}
	}
	return best
}

func (p *Plugin) failSession(s *loginSession, msg string) {
	s.mu.Lock()
	if s.Status == "success" || s.Status == "failed" {
		s.mu.Unlock()
		return
	}
	s.Status = "failed"
	s.Error = msg
	s.mu.Unlock()
	// Delay the close so an in-flight callback/submit response can flush.
	time.AfterFunc(250*time.Millisecond, s.closeListener)
	p.scheduleSessionCleanup(s.ID)
	p.logLine("warn", "login session failed", map[string]any{"session": s.ID, "reason": msg})
}

var errLoginCancelled = errors.New("codearts: login session was cancelled")

func (p *Plugin) completeLogin(s *loginSession, cred Credential) (string, error) {
	// The ticket poller keeps running after DELETE /accounts/login/{sid}; a
	// credential that arrives later must not create an account the user
	// already walked away from.
	if !p.sessionLive(s.ID) {
		return "", errLoginCancelled
	}
	acct, err := p.upsertAccount(cred, p.now())
	if err != nil {
		p.failSession(s, err.Error())
		return "", err
	}
	s.mu.Lock()
	s.Status = "success"
	s.AccountID = acct.ID
	s.Error = ""
	s.mu.Unlock()
	// The OAuth callback still has a 307 to flush; closing the listener from
	// inside its handler would turn the browser response into an EOF.
	time.AfterFunc(250*time.Millisecond, s.closeListener)
	p.scheduleSessionCleanup(s.ID)
	p.logLine("info", "login succeeded", map[string]any{"session": s.ID, "account": acct.ID})
	return acct.ID, nil
}

func (p *Plugin) upsertAccount(cred Credential, now time.Time) (*Account, error) {
	if !cred.usable() {
		return nil, errors.New("登录响应缺少 AK/SK/security_token")
	}
	id := stableAccountID(cred)
	p.mu.Lock()
	existing := p.accountByIDLocked(id)
	if existing == nil {
		a := &Account{
			ID:         id,
			Label:      accountLabel(cred),
			Enabled:    true,
			AddedAt:    now.Unix(),
			Credential: cred,
			Identity:   newIdentity(now.Unix()),
		}
		p.accounts = append(p.accounts, a)
		p.mu.Unlock()
		if err := p.saveAccounts(); err != nil {
			return nil, err
		}
		return a, nil
	}
	existing.Credential = cred
	if label := accountLabel(cred); label != "" {
		existing.Label = label
	}
	existing.Invalid = false
	existing.LastError = ""
	if existing.Identity.SessionID == "" {
		existing.Identity = newIdentity(now.Unix())
	}
	out := deepCopyAccount(existing)
	p.mu.Unlock()
	if err := p.saveAccounts(); err != nil {
		return nil, err
	}
	return out, nil
}

func (p *Plugin) cancelAllSessions() {
	p.sessionsMu.Lock()
	sessions := make([]*loginSession, 0, len(p.sessions))
	for id, s := range p.sessions {
		sessions = append(sessions, s)
		delete(p.sessions, id)
	}
	p.sessionsMu.Unlock()
	for _, s := range sessions {
		s.closeListener()
	}
}
