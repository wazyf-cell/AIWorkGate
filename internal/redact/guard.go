// 从 magpie internal/redact 移植（逐节等价切分自原单文件 redact.go；
// 拆分只调整文件边界与 import，不改变任何逻辑，见 README 的来源与完整性节）。
package redact

import (
	"regexp"
	"slices"
	"strings"
)

// placeholderRe is a placeholder as Mask writes it.
var placeholderRe = regexp.MustCompile(`\{\{[A-Z][A-Z0-9_]*_[a-z2-7]{8}\}\}`)

// HasPlaceholder says whether s holds text written as Mask writes a
// placeholder, in JSON or not.
func HasPlaceholder(s string) bool {
	return strings.Contains(s, "{{") && placeholderRe.MatchString(s)
}

// notAVariable turns away what stands in for a value: ${PASS}, <password>, ****.
func notAVariable(v string) bool {
	if strings.ContainsAny(v[:1], "$%{<[*") {
		return false
	}
	return strings.Trim(v, "*xX.") != ""
}

// keyValue is a key's or a token's value, not a word or a number: letters
// and digits both, or 16 characters or more that aren't all digits. token 是
// expired, key 是 value and 密钥为 2048 are left.
func keyValue(v string) bool {
	if !notAVariable(v) {
		return false
	}
	letter := strings.IndexFunc(v, func(r rune) bool { return r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' }) >= 0
	return letter && (strings.ContainsAny(v, digits) || len(v) >= 16)
}

// keyAt is keyValue for s[a:b], or six digits or more right after 为, 為,
// 是, ：, :, =, は or 는 that count nothing: a key said in a message is
// often a number (密钥为12231241, token 是 20261025, CyberCaius on
// Discord), while 密钥为 2048 位 and 令牌 300000 毫秒 are a size and a
// time.
func keyAt(s string, a, b int) bool {
	v := s[a:b]
	if pathValue(v) || versionValue(v) {
		// where a key is kept: 私钥 ~/.ssh/id_ed25519, or a release's
		// version: token 是 0.15.0-rc1 (fieldvalue.go)
		return false
	}
	if keyValue(v) {
		return true
	}
	if len(v) < 6 || strings.Trim(v, digits) != "" {
		return false
	}
	before := strings.TrimRight(s[:a], " \t\"'“「\\")
	after := strings.TrimLeft(s[b:], " \t")
	return slices.ContainsFunc(keySeparators, func(c string) bool { return strings.HasSuffix(before, c) }) &&
		!slices.ContainsFunc(quantityUnits, func(u string) bool { return strings.HasPrefix(after, u) })
}

// keySeparators are what stands between a name and a key's value.
var keySeparators = []string{"为", "為", "是", "：", ":", "=", "は", "는"}

// quantityUnits are what a number counts when one follows it: bits, bytes,
// a time, a count.
var quantityUnits = []string{"位", "比特", "字节", "字節", "个", "個", "次", "秒", "毫秒", "分钟", "分鐘", "小时", "小時", "天", "年", "bit", "byte", "ms", "sec", "min", "ビット", "バイト", "비트", "바이트"}

// secretValue is a value that looks like a secret, not code: letters and
// digits both, and not a call, a reference or a placeholder of its own.
func secretValue(v string) bool {
	if !notAVariable(v) || strings.HasPrefix(v, "process.env") || strings.HasPrefix(v, "os.") {
		return false
	}
	return strings.ContainsAny(v, digits) && strings.IndexFunc(v, func(r rune) bool { return r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' }) >= 0
}

// identPath is a name in code, or a field read through one: Uint8Array,
// details.2, opts.token2.
var identPath = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z0-9_]+)*$`)

// notCode turns away a value at s[a:b] that is code naming a secret, not
// the secret: a call (privateKey: toBase64Url(…), secretKey =
// fromBase64Url(…)), or, unquoted after a colon, a type or a read that
// code goes on from (secretKey: Uint8Array;, private_key: details.2,).
// A secret written in code is a string, so in quotes; one after = or at a
// line's end (.env files, logs, YAML) is kept (Kieran Zhang on X, 10-25).
func notCode(s string, a, b int) bool {
	if !identPath.MatchString(s[a:b]) {
		return true
	}
	rest := strings.TrimLeft(s[b:], " \t")
	if strings.HasPrefix(rest, "(") {
		return false
	}
	if a > 0 && (s[a-1] == '"' || s[a-1] == '\'') || !strings.HasSuffix(strings.TrimRight(s[:a], " \t"), ":") {
		return true
	}
	return rest == "" || !strings.ContainsRune(";,)}]|?&<>", rune(rest[0]))
}

// notUserinfo turns away what looks like an email at s[a:b] but is a
// URL's user:password@host or user@host: its @ ends the userinfo, so
// what is before it is a user name or a password and what is after it
// the host. postgres://app:hunter2pass@db.acme-corp.cn:5432/x went as
// app:{{EMAIL_…}}:5432, the password and the host one address, where the
// password is the PASSWORD rule's and the host stays (#3538). An address
// in a URL's path or query (?to=bob@acme.cn) and in mailto: is an
// email still.
func notUserinfo(s string, a, _ int) bool {
	return !inUserinfo(s, a)
}

// emailAt is where an email at s[a:b] can be: not in a URL's userinfo
// (notUserinfo), and with no dot touching it that makes it part of a
// longer name: none before it, and none after it that a letter, digit,
// - or _ follows (a@b.io.12 is no address). A dot after it that ends a
// sentence is the sentence's: "The user's email address is
// jo@quill.dev." masked nothing while the dot was a bound character, and
// the address went to the vendor as written.
func emailAt(s string, a, b int) bool {
	if a > 0 && s[a-1] == '.' {
		return false
	}
	if b+1 < len(s) && s[b] == '.' && strings.IndexByte(tokenCh, s[b+1]) >= 0 {
		return false
	}
	return notUserinfo(s, a, b)
}

// inUserinfo says s[a] is inside a URL's authority, after its scheme://
// and before the host: nothing from there back to the // is a /, ?, #,
// space, quote, < or > or @.
func inUserinfo(s string, a int) bool {
	i := a
	for i > 0 && !strings.ContainsRune(" \t\r\n/?#\"'<>@`", rune(s[i-1])) {
		i--
	}
	if i < 3 || s[i-1] != '/' || s[i-2] != '/' || s[i-3] != ':' {
		return false
	}
	j := i - 3 // the scheme ends at s[j], its colon
	k := j
	for k > 0 && (s[k-1] >= 'a' && s[k-1] <= 'z' || s[k-1] >= 'A' && s[k-1] <= 'Z' || s[k-1] >= '0' && s[k-1] <= '9' || strings.IndexByte("+.-", s[k-1]) >= 0) {
		k--
	}
	for k < j && !(s[k] >= 'a' && s[k] <= 'z' || s[k] >= 'A' && s[k] <= 'Z') {
		k++ // a scheme starts with a letter
	}
	return k < j
}

// hiddenInURL says the password at s[a:b], in a URL's user:password@,
// is hidden by masking it: it is not also written as it is elsewhere in
// the same URL, as its user name, a part of its host or of its path. A
// test database's URL often gives one word for all three:
// postgresql://geodatafusion:geodatafusion@127.0.0.1:55432/geodatafusion
// is the PostGIS test of the geodatafusion repo. Its password was masked
// while the URL still had it twice as written, and once masked the word
// was known (knownSpans): the project's folders, its crate, its .toml
// and cargo test -p geodatafusion went to the vendor as a made-up word
// for the rest of the run, as a password (xuanwo on X, 10-29: "很多项目名
// 被识别成了 password", 26M masked in a day). postgres:postgres,
// root:root and guest:guest are the same: anyone who reads the user
// reads the password. A password not written elsewhere in its URL is
// masked as before, whatever it looks like: app:geodatafusion@ masks it
// there and, as known, wherever it is next.
func hiddenInURL(s string, a, b int) bool {
	pw := s[a:b]
	i := strings.LastIndex(s[:a], "//")
	if i < 0 || s[a-1] != ':' {
		return true
	}
	if s[i+2:a-1] == pw {
		return false // the user name
	}
	// the host and the path, to the URL's end
	end := b + 1
	for end < len(s) && !strings.ContainsRune(" \t\r\n\"'<>`\\)]}", rune(s[end])) {
		end++
	}
	if b+1 > end {
		return true
	}
	for _, part := range strings.FieldsFunc(s[b+1:end], func(r rune) bool { return strings.ContainsRune("/:.?#&=;,@", r) }) {
		if part == pw {
			return false
		}
	}
	return true
}

// realEmail is whether v, matched as an address, is someone's. git@ is
// the user every git host is reached by over SSH (git@github.com:o/r.git,
// git@gitee.com, a company's own git@git.corp.com), no one's mailbox, and
// was masked so a remote no longer read as one (jumy on Discord).
// notASlug is false for words in lower case joined by hyphens, as a
// page's address names an article about SK hynix
// (cnbc.com/…/sk-hynix-third-quarter-profit.html), which went to the model
// as a key. A key's characters are random: it has a capital, or a run of
// 16 or more with no hyphen (DeepSeek's sk- and 32 hex digits, OpenRouter's
// sk-or-v1- and 64), or hex digits alone, as a UUID's groups (sk- and a
// UUID in lower case) are, which no word but a few like "face" is.
func notASlug(v string) bool {
	if strings.ContainsFunc(v, func(r rune) bool { return r >= 'A' && r <= 'Z' }) {
		return true
	}
	if !strings.ContainsFunc(strings.TrimPrefix(v, "sk-"), func(r rune) bool {
		return r != '-' && r != '_' && (r < '0' || r > '9') && (r < 'a' || r > 'f')
	}) {
		return true
	}
	for _, w := range strings.FieldsFunc(v, func(r rune) bool { return r == '-' || r == '_' }) {
		if len(w) >= 16 {
			return true
		}
	}
	return false
}

// headerValue says the rest of an Authorization or Cookie line is a
// header's value, not prose that begins with the word ("Authorization:
// the user must sign in first"): a credential, after a scheme word or
// alone (Bearer …, Basic …), or a cookie's name=value pairs. One that
// stands for a variable (${TOKEN}, <token>, ****) is left alone.
func headerValue(v string) bool {
	v = strings.TrimSpace(v)
	if v == "" || !notAVariable(v) {
		return false
	}
	notLetter := func(r rune) bool { return !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z') }
	f := strings.Fields(v)
	if len(f) == 1 || len(f) == 2 && strings.IndexFunc(f[0], notLetter) < 0 {
		// a word alone is prose's: a credential has a digit or a sign in it
		cred := f[len(f)-1]
		return len(cred) >= 8 && notAVariable(cred) && strings.IndexFunc(cred, notLetter) >= 0
	}
	for _, kv := range strings.Split(v, ";") {
		if kv = strings.TrimSpace(kv); kv == "" {
			continue
		}
		name, _, ok := strings.Cut(kv, "=")
		if !ok || name == "" || strings.ContainsAny(name, " \t") {
			return false
		}
	}
	return strings.Contains(v, "=")
}

// notAPlainKey turns away the upper-case name before the value at s[:a]
// when it only ends in key and holds no secret: a sort key, a cache key, a
// prompt cache key, a public key (#2946).
func notAPlainKey(s string, a, b int) bool {
	i := a
	for i > 0 && strings.IndexByte(`"' `+"\t:=", s[i-1]) >= 0 {
		i--
	}
	end := i
	for i > 0 && strings.IndexByte(alnum+"_.-", s[i-1]) >= 0 {
		i--
	}
	name := strings.ToLower(s[i:end])
	return !strings.HasSuffix(name, "sort_key") && !strings.HasSuffix(name, "cache_key") &&
		!strings.HasSuffix(name, "prompt_cache_key") && !strings.HasSuffix(name, "public_key")
}

// notPlainKeyOrCode keeps both boundaries on a name that only ends in _KEY
// or _PASS: plain keys stay as written, and so does code naming a secret.
func notPlainKeyOrCode(s string, a, b int) bool {
	return notAPlainKey(s, a, b) && notCode(s, a, b)
}

// namedSecretAt is notPlainKeyOrCode with the value-side reading the SECRET
// rules' fieldAt gained later: a const TOKEN_KEY = "access_token_v2" is
// code naming a token, and its value reads as a version, so both stay.
func namedSecretAt(s string, a, b int) bool {
	return notPlainKeyOrCode(s, a, b) && fieldAt(s, a, b)
}
