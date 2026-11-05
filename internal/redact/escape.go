// 从 magpie internal/redact 移植（逐节等价切分自原单文件 redact.go；
// 拆分只调整文件边界与 import，不改变任何逻辑，见 README 的来源与完整性节）。
package redact

import (
	"encoding/json"
	"slices"
	"strings"
)

// unescapedView is s, JSON text, as long as s and read as the vendor reads
// it once unescaped, as far as the finders tell: an escape that stands for
// a newline, a tab or another control character has its letter read as
// one (\n as \ and a newline), and a \u escape's u and digits as line
// breaks, so neither starts or ends a word. A backslash, a quote and a
// slash escaped stay as written: the rules read \" as a quote and C:\\Users
// as a Windows path.
func unescapedView(s string) string {
	if strings.IndexByte(s, '\\') < 0 {
		return s
	}
	b := []byte(s)
	for i := 0; i+1 < len(b); i++ {
		if b[i] != '\\' {
			continue
		}
		i++
		switch b[i] {
		case 'n', 'b', 'f':
			b[i] = '\n'
		case 'r':
			b[i] = '\r'
		case 't':
			b[i] = '\t'
		case 'u':
			for j := i; j < i+5 && j < len(b); j++ {
				b[j] = '\n'
			}
			i += 4
		}
	}
	return string(b)
}

// unescapedPut is put given each value unescaped, as the vendor and the
// agent read it: a private key's \n are newlines. A value that isn't a
// JSON string's text goes as it is.
func unescapedPut(put func(kind, v string) string) func(kind, v string) string {
	return func(kind, v string) string {
		if strings.IndexByte(v, '\\') >= 0 {
			var u string
			if json.Unmarshal([]byte(`"`+v+`"`), &u) == nil {
				v = u
			}
		}
		return put(kind, v)
	}
}

// outsideEscapes is found with no value starting or ending inside one of
// the escapes of s, JSON text: after an escape's backslash, as the n of
// \n, or among a \u00e9's digits. Masked there, it would leave a
// backslash before the placeholder or cut one off from its letter, and s
// would be JSON no longer (#3022). A value is cut back to the escapes'
// edges instead, so what it holds is still masked: the zoe@quill.dev of a
// model's "nzoe@quill.dev" read in \nzoe@quill.dev, a user's rule that
// reads up to the backslash of \". One left with nothing goes. Every
// finder's values go through here: the rules', the user's words, the
// model's and those known before.
func outsideEscapes(s string, found []span) []span {
	if len(found) == 0 || strings.IndexByte(s, '\\') < 0 {
		return found
	}
	// esc[i], for i inside an escape past its backslash: where the escape
	// starts and ends, plus one each so a zero is outside one
	type edges struct{ from, to int }
	esc := make([]edges, len(s)+1)
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' {
			continue
		}
		n := 2
		if i+1 < len(s) && s[i+1] == 'u' {
			n = 6
		}
		end := min(i+n, len(s))
		for j := i + 1; j < end; j++ {
			esc[j] = edges{i + 1, end + 1}
		}
		i = end - 1
	}
	out := found[:0]
	for _, f := range found {
		if e := esc[f.start]; e.to > 0 {
			f.start = e.to - 1
		}
		if e := esc[f.end]; e.from > 0 {
			f.end = e.from - 1
		}
		if f.start < f.end {
			out = append(out, f)
		}
	}
	return out
}

// inStrings is found, for s JSON text (a tool call's arguments), with
// each value inside the one string it starts in: cut at that string's
// closing quote, or started at the first string inside it. A value that
// is a whole bare value (a number under "password":) is kept, and its
// placeholder goes in quoted (span.bare); one in nothing else, across a
// key's quotes, a colon or a brace, goes. Masked over the quote, the
// arguments were JSON no longer: the local model reads a password's ! and
// the "} after it as one token, and {"cmd":"export DB_PASSWORD=Hunt3r!"}
// went as {"cmd":"export DB_PASSWORD={{SECRET_…}}}; a user's rule
// tok-\S+ took the "} too. The vendor turned away every turn that
// replayed the call, and Codex reconnected over and over
// (xiaojiang091499 on X). Every finder's values go through here, as
// through outsideEscapes. Text that isn't JSON is left as it is.
func inStrings(s string, found []span) []span {
	if len(found) == 0 || !json.Valid([]byte(s)) {
		return found
	}
	// in[i] is 1 + which string literal byte i is the text of, 0 for one
	// outside every string (its quotes among them)
	in := make([]int32, len(s))
	n := int32(0)
	for i := 0; i < len(s); i++ {
		if s[i] != '"' {
			continue
		}
		n++
		j := i + 1
		for ; j < len(s) && s[j] != '"'; j++ {
			in[j] = n
			if s[j] == '\\' && j+1 < len(s) {
				j++
				in[j] = n
			}
		}
		i = j
	}
	out := found[:0]
	for _, f := range found {
		a := f.start
		for a < f.end && in[a] == 0 {
			a++
		}
		if a == f.end {
			if bareValue(s, f.start, f.end) {
				f.bare = true
				out = append(out, f)
			}
			continue
		}
		b := a
		for b < f.end && in[b] == in[a] {
			b++
		}
		f.start, f.end = a, b
		out = append(out, f)
	}
	return out
}

// bareValue says s[a:b], JSON text outside its strings, is all of one
// bare value: an object's (after :) or an array's (after [ or ,), with
// the , } or ] that ends it after it.
func bareValue(s string, a, b int) bool {
	if a >= b || strings.ContainsAny(s[a:b], " \t\r\n{}[]\",:") {
		return false
	}
	i := a - 1
	for i >= 0 && isSpace(s[i]) {
		i--
	}
	k := b
	for k < len(s) && isSpace(s[k]) {
		k++
	}
	return i >= 0 && strings.IndexByte(":[,", s[i]) >= 0 && (k == len(s) || strings.IndexByte(",}]", s[k]) >= 0)
}

// noNumbers is found without what is a piece of a decimal number of s:
// digits right after the point of 5.13800138000, or right before the
// point of 13800138000.5. A number is judged whole, and the fraction of
// a height, a price or a coordinate is not a phone or a card number
// because its digits happen to check (willz on Discord). Nor is a number
// that stands bare as a JSON value: "timestamp": 17929036837 in a session
// file is a count or a time, and a phone, a card or an ID number in JSON
// is a string (id.null on Discord). Only personal data's kinds, whoever
// found them (a rule, the model, a value known before): a secret is
// masked wherever it is, and what the user chose (their words, their
// rules) stays theirs.
func noNumbers(s string, found []span) []span {
	return slices.DeleteFunc(found, func(f span) bool {
		_, personal := kindCats[f.kind]
		return personal && f.by != ByWord && f.by != ByYours && (inDecimal(s, f.start, f.end) || jsonNumber(s, f.start, f.end))
	})
}

// jsonNumber says s[a:b] is all of a bare JSON number, by where it sits:
// digits only, an object's value ("key": …) or an element of an array of
// numbers ([…, …]), and nothing after it but the , } or ] that ends a
// value. "phone": "13800138000" is a string, and phone:13800138000 and
// "call 13800138000, thanks" are text, so each is still masked.
func jsonNumber(s string, a, b int) bool {
	if a >= b || strings.Trim(s[a:b], digits) != "" {
		return false
	}
	space := func(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }
	k := b
	for k < len(s) && space(s[k]) {
		k++
	}
	if k < len(s) && strings.IndexByte(",}]", s[k]) < 0 {
		return false
	}
	i := a - 1
	for i >= 0 && space(s[i]) {
		i--
	}
	if i < 0 {
		return false
	}
	switch s[i] {
	case ':':
		for i--; i >= 0 && space(s[i]); i-- {
		}
		return i >= 0 && s[i] == '"'
	case '[', ',':
		// back over the numbers before it to the array's [
		for ; i >= 0; i-- {
			if s[i] == '[' {
				return true
			}
			if strings.IndexByte(digits+",.-+eE", s[i]) < 0 && !space(s[i]) {
				return false
			}
		}
	}
	return false
}

// inDecimal says s[a:b] starts with a digit that is a decimal's fraction
// (a digit and a point before it) or ends with a digit that is its whole
// part (a point and a digit after it).
func inDecimal(s string, a, b int) bool {
	isDigit := func(i int) bool { return i >= 0 && i < len(s) && s[i] >= '0' && s[i] <= '9' }
	return isDigit(a) && a >= 2 && s[a-1] == '.' && isDigit(a-2) ||
		isDigit(b-1) && b+1 < len(s) && s[b] == '.' && isDigit(b+1)
}
