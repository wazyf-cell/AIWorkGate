// 从 magpie internal/redact 移植（逐节等价切分自原单文件 redact.go；
// 拆分只调整文件边界与 import，不改变任何逻辑，见 README 的来源与完整性节）。
package redact

import (
	"regexp"
	"strings"
)

// ---- restoring ------------------------------------------------------------------

// placeholderBody is a placeholder without its braces, as a model may write
// one back: API_KEY_k3v9x2mq, phone_b5puzbfv, PHONE_B5PUZBFV.
var placeholderBody = regexp.MustCompile(`[A-Za-z][A-Za-z0-9_]*_[A-Za-z2-7]{8}`)

// Restore puts the values back in s. escaped is for text that is itself
// JSON (a tool call's arguments), where a value goes in escaped.
//
// A model doesn't always write a placeholder back as it was given: it
// writes {{ PHONE_b5puzbfv }} as a template has it, drops a brace, or the
// braces, or changes the case. Any of those is put back too when what is
// left names a placeholder this magpie made: its kind and the 8 of its
// hash, which no other text has by chance (shensmart on Discord).
func Restore(s string, escaped bool) string { return restore(s, escaped, nil) }

// restore is Restore, counting in put, when it isn't nil, each
// placeholder it put a value back for.
func restore(s string, escaped bool, put map[string]int) string {
	mu.RLock()
	defer mu.RUnlock()
	if len(values) == 0 {
		return s
	}
	s = restoreShapes(s, put)
	if !strings.Contains(s, "_") {
		return s
	}
	var b strings.Builder
	last := 0
	for _, m := range placeholderBody.FindAllStringIndex(s, -1) {
		a, z := m[0], m[1]
		if a < last || a > 0 && isWordByte(s[a-1]) || z < len(s) && isWordByte(s[z]) {
			continue
		}
		p := canonical(s[a:z])
		v, ok := values[p]
		if !ok {
			continue
		}
		a, z = braces(s, a, z)
		if a < last {
			continue
		}
		if put != nil {
			put[p]++
		}
		b.WriteString(s[last:a])
		if escaped {
			v = jsonEscape(v)
		}
		b.WriteString(v)
		last = z
	}
	if last == 0 {
		return s
	}
	b.WriteString(s[last:])
	return b.String()
}

// canonical is the placeholder Mask wrote for body, whatever its case.
func canonical(body string) string {
	cut := len(body) - 8
	return "{{" + strings.ToUpper(body[:cut]) + strings.ToLower(body[cut:]) + "}}"
}

func isWordByte(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_'
}

// braces widens s[a:z], a placeholder's body, over the braces the model
// wrote around it, and the spaces inside them: both of {{ and }}, or one
// of them dropped ({{X} or {X}}), or just {{ or just }}. A single brace
// on one side alone is the text's own ({X as in code) and stays.
func braces(s string, a, z int) (int, int) {
	la, l := a, 0
	for la > 0 && (s[la-1] == ' ' || s[la-1] == '\t') {
		la--
	}
	for l < 2 && la > 0 && s[la-1] == '{' {
		la--
		l++
	}
	rz, r := z, 0
	for rz < len(s) && (s[rz] == ' ' || s[rz] == '\t') {
		rz++
	}
	for r < 2 && rz < len(s) && s[rz] == '}' {
		rz++
		r++
	}
	if l == 2 || l == 1 && r >= 1 {
		a = la
	}
	if r == 2 || r == 1 && l >= 1 {
		z = rz
	}
	return a, z
}

// maxPlaceholder is the longest placeholder Mask writes: {{, a kind (a
// user's rule may name one up to maxKind long), _, the 8 of the hash, }}.
const maxPlaceholder = len("{{") + maxKind + len("_") + 8 + len("}}")

// partialTail is how much of the end of s may be the start of a
// placeholder the next piece of a stream finishes, in any of the ways
// Restore takes one back: {{API_, {{ phone_b5, PHONE_b5puzbfv} — or, with
// no brace before it, a last word that starts as a placeholder this magpie
// made does (PHO, PHONE_b5pu), or is one whose }} may still come.
func partialTail(s string) int {
	if n := braceTail(s); n > 0 {
		return n
	}
	return max(wordTail(s), shapeTail(s))
}

// braceTail is a tail from {, or {{, on: then spaces, a placeholder's body
// or the start of one, then spaces and one } at most.
func braceTail(s string) int {
	i := strings.LastIndexByte(s, '{')
	if i < 0 {
		return 0
	}
	if i > 0 && s[i-1] == '{' {
		i--
	}
	t := s[i:]
	if len(t) >= maxPlaceholder+4 || strings.Contains(t, "}}") {
		return 0
	}
	body := strings.TrimLeft(t, "{")
	body = strings.TrimLeft(body, " \t")
	if body == "" {
		return len(t) // {, {{, {{␠
	}
	if c := body[0]; !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z') {
		return 0
	}
	j := 0
	for j < len(body) && isWordByte(body[j]) {
		j++
	}
	rest := strings.TrimLeft(body[j:], " \t")
	if rest != "" && rest != "}" {
		return 0
	}
	if j < len(body) && placeholderBody.FindString(body[:j]) != body[:j] {
		return 0 // a space or a brace after something that is no placeholder
	}
	return len(t)
}

// wordTail is the last word of s, where it may be a placeholder's body as
// a model writes one without braces: the start of one of the kinds this
// magpie has made placeholders of, up to the 8 of its hash. A word of
// text that only starts like a kind is held for an event and then goes
// on, unchanged.
func wordTail(s string) int {
	i := len(s)
	for i > 0 && isWordByte(s[i-1]) {
		i--
	}
	w := s[i:]
	if w == "" || len(w) > maxKind+9 || !(w[0] >= 'A' && w[0] <= 'Z') {
		return 0
	}
	u := strings.ToUpper(w)
	mu.RLock()
	defer mu.RUnlock()
	for k := range kinds {
		k += "_"
		if strings.HasPrefix(k, u) {
			return len(w)
		}
		if strings.HasPrefix(u, k) && len(u)-len(k) <= 8 && strings.Trim(strings.ToLower(u[len(k):]), "abcdefghijklmnopqrstuvwxyz234567") == "" {
			return len(w)
		}
	}
	return 0
}
