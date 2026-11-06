package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// --- envelope + payload parsing, ported from lobsterai.ts ---

// loginSession is createLobsteraiLoginSession: a uuid plus the "first keyfrom"
// timestamp captured when the login page was opened.
type loginSession struct {
	UUID          string
	FirstKeyfrom  string
	LatestKeyfrom string
}

// strconvNow returns the current epoch milliseconds as a decimal string, which is
// the wire format the keyfrom fields use (the reference does `String(Date.now())`).
func strconvNow() string { return strconv.FormatInt(time.Now().UnixMilli(), 10) }

// envelope is parseLobsteraiEnvelope: {code, msg|message, data}, success is code 0.
type envelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

func parseLobsteraiEnvelope(body []byte) (envelope, bool) {
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return envelope{Code: -1}, false
	}
	if env.Code != 0 {
		return env, false
	}
	return env, true
}

// tokenPayload is parseLobsteraiTokenPayload's fields.
type tokenPayload struct {
	AccessToken   string
	RefreshToken  string
	UserID        string
	YID           string
	AccountUserID string
	Nickname      string
	ExpiresIn     *int
}

// exchangeRejectedError is an upstream business rejection (HTTP 200 with a
// non-zero code) of the submitted auth code: the operator's input is wrong, not
// the transport. Handlers use it to answer 4xx instead of 502.
type exchangeRejectedError struct {
	Code int
	Msg  string
}

func (e *exchangeRejectedError) Error() string {
	msg := strings.TrimSpace(e.Msg)
	if msg == "" {
		msg = fmt.Sprintf("code=%d", e.Code)
	}
	return "lobsterai: " + msg
}

// parseLobsteraiTokenPayload reads the token tuple out of an exchange or refresh
// envelope. Field names come from the reference: accessToken / refreshToken /
// expiresIn at the top level, user.{id,yid,userId,nickname} nested.
func parseLobsteraiTokenPayload(body []byte) (tokenPayload, error) {
	env, ok := parseLobsteraiEnvelope(body)
	if !ok {
		return tokenPayload{}, &exchangeRejectedError{Code: env.Code, Msg: env.Msg}
	}
	var data struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    *int   `json:"expiresIn"`
		User         struct {
			ID       string `json:"id"`
			YID      string `json:"yid"`
			UserID   string `json:"userId"`
			Nickname string `json:"nickname"`
		} `json:"user"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return tokenPayload{}, fmt.Errorf("lobsterai: token payload: %w", err)
	}
	return tokenPayload{
		AccessToken:   data.AccessToken,
		RefreshToken:  data.RefreshToken,
		UserID:        data.User.ID,
		YID:           data.User.YID,
		AccountUserID: data.User.UserID,
		Nickname:      data.User.Nickname,
		ExpiresIn:     data.ExpiresIn,
	}, nil
}

// toCredentials builds a credential from a token payload plus the login session,
// porting buildLobsteraiCredential: the two keyfrom timestamps are frozen here
// and are never updated by a later refresh (the reference matches the Go
// implementation, which reads the stored LatestKeyfrom).
func (p tokenPayload) toCredentials(session loginSession) credentials {
	return credentials{
		AccessToken:   p.AccessToken,
		RefreshToken:  p.RefreshToken,
		FirstKeyfrom:  session.FirstKeyfrom,
		LatestKeyfrom: session.LatestKeyfrom,
		UUID:          session.UUID,
		UserID:        p.UserID,
		YID:           p.YID,
		AccountUserID: p.AccountUserID,
		Nickname:      p.Nickname,
	}
}
