// Package redact keeps secrets on the machine. What the gateway sends a
// vendor has its secrets (API keys, private keys, tokens, passwords in
// connection strings and assignments), and when asked personal data and the
// user's own words, swapped for placeholders like {{API_KEY_k3v9x2mq}}; what
// the vendor says back has them swapped back, streams included, so the agent
// and the user see the real thing while the vendor never does.
//
// A placeholder is the same for the same value every time (a keyed hash of
// it), so a conversation's history reads the same turn after turn — what a
// vendor cached of it stays good, and a thinking block's signature still
// matches the text it was made for — and one placeholder is never two
// values. The values are held in memory only, by placeholder, as requests
// bring them; a restart loses nothing, since the next request brings them
// again. A value once masked is masked wherever a request has it, whatever

package redact

import (
	"regexp"
	"slices"
	"strings"
)

type Options struct {
	Secrets  bool     // API keys, private keys, tokens, passwords
	Personal bool     // emails, phone numbers, ID and bank card numbers
	Words    []string // the user's own: names, codenames, hosts
	// WordsAnyCase matches Words in any case: Nightjar masks NIGHTJAR and
	// nightjar too. Off, a word is matched as written.
	WordsAnyCase bool
	// WordsWhole matches Words only as whole words (acmilan_ on Discord):
	// ABC masks "ABC IS" and 'ABC' but not XABCD, and 123456 masks no part
	// of 91234567, 0.123456 or 123456.78 (wholeWord). Off, a word is found
	// anywhere, inside a longer one too.
	WordsWhole bool
	Rules      []Rule // the user's own rules, masked with the secrets
	// Kinds turns each of the Categories of personal data on or off by its
	// id, where the user chose; one not in it is as its Category.On says.
	// None of them is masked while Personal is off.
	Kinds map[string]bool
	// Model, where set, finds more in the texts to mask (model.go)
	Model Model
	// Route names where a request masked for a vendor may go (the gateway
	// sets it: the providers a model, its fallbacks or a routing group's
	// members are served by). A text sent on it before goes on it again
	// as it went (sentSpans); on another route it is masked afresh.
	Route string
	// MinScore is the score the user has Model's finds need (Settings →
	// Privacy's slider), which Model's own finds are held to already. A
	// value only the model found, masked before at a lower score, isn't
	// masked again as known under it (knownSpans).
	MinScore float64
	// NamesByModel says Model finds Chinese names itself (AstrLink
	// Guard): beside it only a nickname (小王, 老李) is found by the
	// words around it (zhNameSpans)
	NamesByModel bool
	// Shape sends a key as a made-up key of its form, not a placeholder
	// (shape.go)
	Shape bool

	// Actions is what is done with a value of a kind (a Hit's Kind) other
	// than masking it, where the user chose: Warn sends it as written,
	// Block masks it and the request is held back (the gateway's). A kind
	// not in it is masked.
	Actions map[string]string
	// Allow are values the user lets go as written, whichever finder finds
	// them: each a whole value, in any case.
	Allow []string

	// Seen, where set, is told of each value masked, warned of or blocked
	Seen func(Hit)

	// found are the spans Model found, by text, for one request's texts
	found map[string][]Found
	// sent is masking for a vendor (Mask, MaskJSON): a text masked
	// before on Route has the model's finds and the known values it had
	// then (sentSpans)
	sent bool
}

// covers says o masks what a rule of category cat finds: "" are the
// secrets, catPersonal the personal data always masked with it, any other
// one of the Categories.
func (o Options) covers(cat string) bool {
	switch cat {
	case "":
		return o.Secrets
	case catPersonal:
		return o.Personal
	}
	if !o.Personal {
		return false
	}
	if on, ok := o.Kinds[cat]; ok {
		return on
	}
	return categoryOn[cat]
}

// rule finds one kind of value. The match is group 1 when the pattern has
// one, else all of it; bound are the characters that may not touch it on
// either side (a key inside a longer word is no key); ok checks it further.
type rule struct {
	kind    string
	re      *regexp.Regexp
	markers []string // one of these is in any text it can match
	// fold are markers too, looked for in the text in lower case, for a
	// pattern that matches its words in any case (Passport, PASSPORT)
	fold  []string
	bound string
	ok    func(string) bool
	// at checks the value at s[a:b] by what is around it: a dotted quad
	// right after "version" is no address
	at func(s string, a, b int) bool
	// fix, where set, checks the value in place of ok and may shorten it:
	// an IBAN printed in groups of four, followed by a word in capitals
	fix func(s string, a, b int) (int, int, bool)
	// back and ahead, where ahead is set, are how far before and after
	// one of fold a match can be: the pattern is only run there, as one
	// that takes its words in any case is slow over all of a long text
	back, ahead int
	// cat is what turns the rule on (Options.covers): "" the secrets
	cat string
	// field is a rule that takes a value after a field's name and = or :,
	// a space allowed between them: a value that is the next field's
	// assignment is not this one's (fieldMatches)
	field bool
	// yours is a rule the user wrote (customRules)
	yours bool
	// anyGroup takes the first group that matched, for a pattern with a
	// group in each of its alternatives
	anyGroup bool
	// groups, where set, are the kinds of a match's groups, each masked as
	// a value of its own (the user and the host of user@host), and groupOK
	// checks each
	groups  []string
	groupOK []func(string) bool
}

const (
	alnum   = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	tokenCh = alnum + "_-"
	digits  = "0123456789"
)

var rules = append([]rule{
	{kind: "PRIVATE_KEY", re: regexp.MustCompile(`-----BEGIN (?:[A-Z0-9]+ )*PRIVATE KEY(?: BLOCK)?-----[\s\S]+?-----END (?:[A-Z0-9]+ )*PRIVATE KEY(?: BLOCK)?-----`), markers: []string{"PRIVATE KEY"}},
	{kind: "API_KEY", re: regexp.MustCompile(`sk-(?:ant-|proj-|or-|svcacct-|admin-)?[A-Za-z0-9_-]{20,}`), markers: []string{"sk-"}, bound: tokenCh, ok: notASlug},
	// a gateway's or a relay's own: oc_sk_…, or_sk_… (#195)
	{kind: "API_KEY", re: regexp.MustCompile(`[a-z]{2,4}_sk_[A-Za-z0-9_-]{20,}`), markers: []string{"_sk_"}, bound: tokenCh, ok: secretValue},
	// Volcano Ark and the relays in front of it issue keys with an ark-
	// prefix (#2946)
	{kind: "API_KEY", re: regexp.MustCompile(`ark-[A-Za-z0-9][A-Za-z0-9_-]{15,}`), markers: []string{"ark-"}, bound: tokenCh, ok: secretValue},
	{kind: "API_KEY", re: regexp.MustCompile(`(?:gh[pousr]_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{40,}|glpat-[A-Za-z0-9_-]{20,})`), markers: []string{"gh", "github_pat_", "glpat-"}, bound: tokenCh},
	{kind: "API_KEY", re: regexp.MustCompile(`AIza[0-9A-Za-z_-]{35}`), markers: []string{"AIza"}, bound: tokenCh},
	{kind: "API_KEY", re: regexp.MustCompile(`xox[abposr]-[0-9A-Za-z-]{10,}`), markers: []string{"xox"}, bound: tokenCh},
	{kind: "API_KEY", re: regexp.MustCompile(`(?:sk|rk)_live_[0-9A-Za-z]{20,}`), markers: []string{"_live_"}, bound: tokenCh},
	{kind: "API_KEY", re: regexp.MustCompile(`(?:AKIA|ASIA)[0-9A-Z]{16}`), markers: []string{"AKIA", "ASIA"}, bound: alnum},
	{kind: "API_KEY", re: regexp.MustCompile(`(?:hf_[A-Za-z0-9]{30,}|gsk_[A-Za-z0-9]{40,}|xai-[A-Za-z0-9]{40,}|npm_[A-Za-z0-9]{36}|pypi-[A-Za-z0-9_-]{50,}|dop_v1_[a-f0-9]{64}|SG\.[A-Za-z0-9_-]{22}\.[A-Za-z0-9_-]{43})`),
		markers: []string{"hf_", "gsk_", "xai-", "npm_", "pypi-", "dop_v1_", "SG."}, bound: tokenCh},
	{kind: "TOKEN", re: regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`), markers: []string{"eyJ"}, bound: tokenCh},
	// the password of user:password@host, or of :password@host as a Redis
	// URL has it with no user
	{kind: "PASSWORD", re: regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^\s:@/?#"'<>]*:([^\s@/?#"'<>]{3,})@`), markers: []string{"://"}, ok: notAVariable, at: hiddenInURL},
	// password = …, API_KEY: "…", as .env files and configs have them
	{kind: "SECRET", re: regexp.MustCompile(`(?i)[A-Za-z0-9_.-]*(?:` + secretNames.pattern() + `)[A-Za-z0-9_.-]*` + quote + `[ \t]*[:=][ \t]*` + quote + `(` + secretCh + `{8,})`),
		markers: secretNames.markers(), ok: secretValue, at: fieldAt, field: true},
	// an env var's name says what it holds in the upper case: a
	// SERVICE_X_KEY, a DB_PASS (#2946). The names above have a word for the
	// secret in them; these only end in one, and only in the upper case is
	// that a secret's name — in the lower case a name that ends in key is
	// code's: a sort key, a cache key, a public key (notAPlainKey turns the
	// upper-case ones of those away too).
	{kind: "SECRET", re: regexp.MustCompile(`(?:^|[^A-Za-z0-9_.-])[A-Za-z0-9_.-]*_(?:KEY|PASS)` + quote + `[ \t]*[:=][ \t]*` + quote + `(` + secretCh + `{8,})`),
		markers: fieldNames{{"key", "key"}, {"pass", "pass"}}.markers(), ok: secretValue, at: namedSecretAt, field: true},
	// an Authorization or Cookie header's value is a secret whatever it
	// looks like: an opaque bearer token, a session value (#2946)
	{kind: "SECRET", re: regexp.MustCompile(`(?im)^[ \t]*(?:authorization|cookie)[ \t]*:[ \t]*(\S[^\r\n]{7,})`),
		markers: fieldNames{{"authorization", "authorization"}, {"cookie", "cookie"}}.markers(), ok: headerValue},
	// a field called key and nothing more, as providers.json has one: "key":
	// "oc_sk_…" (#195). Only a long value of letters and digits both, since
	// code calls anything a key: a map's "key": "value", sort_key, keyboard.
	{kind: "SECRET", re: regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9_.-])` + quote + `key` + quote + `[ \t]*[:=][ \t]*` + quote + `(` + secretCh + `{16,})`),
		markers: fieldNames{{"key", "key"}}.markers(), ok: secretValue, at: fieldAt, field: true},
	// a password after its name in Chinese, Japanese or Korean, as people
	// write one into a message: 密码：786kih, 密碼是 hunter2, パスワード: … .
	// Unlike password = in a config, nothing in code is called 密码, so
	// any value of four or more is taken, digits only too (CyberCaius on
	// Discord); a sentence's closing stop is no part of it.
	{kind: "PASSWORD", re: cjkNamed(cjkPasswordNames), fold: lowerAll(cjkPasswordNames), back: 64, ahead: 100, ok: notAVariable, field: true},
	// a key, a secret or a token after its name the same way: 密钥为 …,
	// 令牌是 …, 金鑰： … . A key of digits only is taken after 为, 是, ：
	// or = when it has six or more and counts nothing, so 密钥为12231241
	// is masked and 密钥为 2048 位 is left.
	{kind: "SECRET", re: cjkNamed(cjkKeyNames), fold: lowerAll(cjkKeyNames), back: 64, ahead: 100, at: keyAt, field: true},
	// an English name for one before 为, 為, 是 or ：, as a Chinese message
	// has it: apikey 为 …, token 是 …, API Key：… (CyberCaius on Discord).
	// password = … and API_KEY: … in a config are the SECRET rule's above.
	{kind: "SECRET", re: regexp.MustCompile(`(?i)(?:` + secretNames.pattern() + `|pwd|api[ ]key|access[ ]key|secret[ ]key|key|\bpin(?:[ ]?code)?)` + quote +
		`[ \t]*(?:[为為是][ \t]*[:：]?|：)[ \t]*(?:\\?["'“「])?(` + secretCh + `{3,}[A-Za-z0-9_\-/+=~!@#%^&*])`),
		fold: cjkCopulas, back: 24, ahead: 260, at: keyAt, field: true},

	{kind: "EMAIL", re: regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,}`), markers: []string{"@"}, bound: alnum + "_%+-", ok: realEmail, at: emailAt, cat: catPersonal},
	{kind: "ID_CARD", re: regexp.MustCompile(`[1-9]\d{5}(?:18|19|20)\d{2}(?:0[1-9]|1[0-2])(?:0[1-9]|[12]\d|3[01])\d{3}[\dXx]`), bound: alnum, ok: chineseID, cat: catPersonal},
	{kind: "PHONE", re: regexp.MustCompile(`(?:\+?86[- ]?)?1[3-9]\d{9}`), bound: alnum, at: phoneAt, cat: catPersonal},
	{kind: "BANK_CARD", re: regexp.MustCompile(`[3-6]\d{3}(?:[ -]?\d{4}){2,3}(?:[ -]?\d{1,3})?`), bound: alnum, ok: luhn, cat: catPersonal},
}, moreRules...)

// cjkPasswordNames are what a password is called in Chinese, Japanese and
// Korean.
var cjkPasswordNames = []string{"密码", "密碼", "口令", "PIN码", "PIN碼", "PIN 码", "安全码", "安全碼", "パスワード", "暗証番号", "비밀번호", "암호", "비번"}

// cjkKeyNames are what a key, a secret or a token is called in Chinese,
// Simplified and Traditional, Japanese and Korean.
var cjkKeyNames = []string{
	"密钥", "密鑰", "秘钥", "秘鑰", "私钥", "私鑰", "金钥", "金鑰", "令牌", "权杖", "權杖", "授权码", "授權碼",
	"秘密鍵", "トークン", "シークレット", "APIキー", "アクセスキー",
	"토큰", "비밀키", "API 키", "API키",
}

// cjkCopulas are what stands between a name and its value in a Chinese
// message: 为, 為, 是, a full-width colon.
var cjkCopulas = []string{"为", "為", "是", "："}

// lowerAll is names as fold has them, in lower case.
func lowerAll(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = strings.ToLower(n)
	}
	return out
}

// cjkNamed is a value after one of names: after ：, :, =, 为, 為, 是, は
// or 는, quoted or not, or after nothing but a space.
func cjkNamed(names []string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)(?:` + strings.Join(names, "|") + `)[ \t]*(?:[:：=][ \t]*|[为為是][ \t]*[:：]?[ \t]*|は[ \t]*|는[ \t]*)?(?:\\?["'“「])?(` + secretCh + `{3,63}[A-Za-z0-9_\-/+=~!@#%^&*])`)
}

// fieldNames are what a field that holds a secret is called: a pattern for
// each name, and text that any match of it has in it, so a rule's markers
// and the names its pattern takes come from one list and can't drift apart.
type fieldNames []struct{ re, marker string }

// secretNames are what a field holding a secret has in its name:
// DB_PASSWORD, apiKey, x-api-key, client_secret.
var secretNames = fieldNames{
	{"password", "pass"}, {"passwd", "pass"},
	{"secret", "secret"},
	{"token", "token"},
	{`api[_-]?key`, "key"}, {`access[_-]?key`, "key"}, {`private[_-]?key`, "key"},
	{"credential", "credential"},
}

func (f fieldNames) pattern() string {
	res := make([]string, len(f))
	for i, n := range f {
		res[i] = n.re
	}
	return strings.Join(res, "|")
}

// markers are each name's marker as a name is written in lower case, in
// upper case and capitalised: password, PASSWORD, Password, apiKey.
func (f fieldNames) markers() []string {
	var out []string
	for _, n := range f {
		for _, m := range []string{n.marker, strings.ToUpper(n.marker), strings.ToUpper(n.marker[:1]) + n.marker[1:]} {
			if !slices.Contains(out, m) {
				out = append(out, m)
			}
		}
	}
	return out
}

const (
	// quote is a field's or a value's quote, if it has one: " or ', or \"
	// in JSON written inside a string
	quote = `(?:\\?["'])?`
	// secretCh are the characters of a secret's value
	secretCh = `[A-Za-z0-9_\-./+=~!@#%^&*]`
)
