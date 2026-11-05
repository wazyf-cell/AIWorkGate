// 从 magpie internal/redact 移植（逐节等价切分自原单文件 redact.go；
// 拆分只调整文件边界与 import，不改变任何逻辑，见 README 的来源与完整性节）。
package redact

import (
	"regexp"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

type span struct {
	start, end int
	kind       string
	by         string  // which finder found it: one of the By constants
	score      float64 // the model's score for one it found, else 0
	// bare: a whole bare value of JSON text (inStrings), whose
	// placeholder goes in quoted
	bare bool
}

// Which finder found a value (Hit.By): magpie's rules, the user's own
// rules, the user's words, the local model (with the Chinese names it
// brings, which run only with it), or a value masked before (known.go).
const (
	ByRule  = "rule"
	ByYours = "yours"
	ByWord  = "word"
	ByModel = "model"
	ByKnown = "known"
)

// What is done with a value of a kind (Options.Actions) other than
// masking it.
const (
	Warn  = "warn"  // sent as written, and recorded
	Block = "block" // masked, and the request held back
)

// Hit is one value found: its kind, the finder that found it, the
// placeholder it went as (or would have, for one sent as written), what
// was done with it ("" masked, Warn, Block) and its Hint. It never holds
// the value itself.
type Hit struct {
	Kind, By, Placeholder, Action string
	Head                          string
	Len                           int
}

// Value is the value behind placeholder p, while this run of magpie
// remembers it.
func Value(p string) (string, bool) {
	mu.RLock()
	defer mu.RUnlock()
	v, ok := values[p]
	return v, ok
}

// Hint is as much of v as may be kept where v itself may not: its first
// character (the first few of a key's, which name its kind: sk-, ghp_)
// and how many characters it has.
func Hint(kind, v string) (head string, n int) {
	n = utf8.RuneCountInString(v)
	if n < 2 {
		return "", n
	}
	k := 1
	if _, personal := kindCats[kind]; !personal && kind != "TERM" && n >= 16 {
		// a key's prefix, up to where its random part starts
		k = 0
		for _, c := range v {
			if k >= 4 || !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ-_", c) {
				break
			}
			k++
			if c == '-' || c == '_' {
				break
			}
		}
		k = max(k, 1)
	}
	r := []rune(v)
	return string(r[:k]), n
}

// anyCases are the patterns that find a user's word in any case, by word.
var anyCases sync.Map

// anyCase finds w as written but in any case (Unicode's simple folding, so
// a word whose case changes its length in bytes is still found whole).
func anyCase(w string) *regexp.Regexp {
	if re, ok := anyCases.Load(w); ok {
		return re.(*regexp.Regexp)
	}
	re := regexp.MustCompile(`(?i)` + regexp.QuoteMeta(w))
	anyCases.Store(w, re)
	return re
}

// wordRune says r is a letter, a digit or _ of a script that puts spaces
// between words. Chinese and Japanese don't, so a word of theirs is found
// inside their text, and a Latin word right beside a Chinese letter
// ("用ABC做") is still whole.
func wordRune(r rune) bool {
	if r == '_' {
		return true
	}
	if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
		return false
	}
	return !unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana)
}

// wholeWord says s[a:b], a word the user asked to mask, is a word of its
// own in s: where its first or last letter is a wordRune, the letter
// beside it in s isn't one. A number is one with its decimals, so a digit
// at its edge also has no "." and digit beside it: 123456 is not whole in
// 0.123456 or 123456.78, while "v1." ends a sentence.
func wholeWord(s string, a, b int) bool {
	first, _ := utf8.DecodeRuneInString(s[a:b])
	last, _ := utf8.DecodeLastRuneInString(s[a:b])
	if wordRune(first) && a > 0 {
		r, n := utf8.DecodeLastRuneInString(s[:a])
		if wordRune(r) {
			return false
		}
		if unicode.IsDigit(first) && r == '.' {
			if p, _ := utf8.DecodeLastRuneInString(s[:a-n]); unicode.IsDigit(p) {
				return false
			}
		}
	}
	if wordRune(last) && b < len(s) {
		r, n := utf8.DecodeRuneInString(s[b:])
		if wordRune(r) {
			return false
		}
		if unicode.IsDigit(last) && r == '.' {
			if f, _ := utf8.DecodeRuneInString(s[b+n:]); unicode.IsDigit(f) {
				return false
			}
		}
	}
	return true
}

// Mask swaps what o covers in s for placeholders, and says how many.
func Mask(s string, o Options) (string, int) {
	o.sent = true
	return mask(s, withModel(o, []string{s}), o.put(), false)
}

// Preview is Mask with nothing remembered: each value goes as the
// placeholder, or with Options.Shape the made-up key, it would have (a
// user name its made-up name, an IP address its public-ip- name), which
// isn't kept for Restore or as a value seen before (known.go).
func Preview(s string, o Options) string {
	put := tag
	if o.Shape {
		put = func(kind, v string) string {
			name, k := splitName(v)
			if f, ok := shapeOf(kind, k); ok {
				return name + f
			}
			return tag(kind, v)
		}
	}
	tagOrShape := put
	put = func(kind, v string) string {
		if kind == "USERNAME" {
			return standInOf(v) // a user name's made-up name (shape.go)
		}
		if kind == "IP" {
			if at, _, ok := ipStandIns(v); ok {
				return at(0) // an address's public-ip- name (shape.go)
			}
		}
		return tagOrShape(kind, v)
	}
	out, _ := mask(s, withModel(o, []string{s}), put, false)
	return out
}
