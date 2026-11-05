// 从 magpie internal/redact 移植（逐节等价切分自原单文件 redact.go；
// 拆分只调整文件边界与 import，不改变任何逻辑，见 README 的来源与完整性节）。
package redact

import (
	"slices"
	"sort"
	"strings"
)

// by is the finder r is, for Hit.By.
func (r rule) by() string {
	if r.yours {
		return ByYours
	}
	return ByRule
}

// matches are r's matches in s, lower being lowerASCII(s), as
// FindAllStringSubmatchIndex has them: in all of s, or, for a rule with
// ahead set, only around its fold markers.
func (r rule) matches(s, lower string) [][]int {
	if r.ahead == 0 {
		return r.findAll(s)
	}
	var wins [][2]int
	for _, f := range r.fold {
		for i := 0; ; {
			j := strings.Index(lower[i:], f)
			if j < 0 {
				break
			}
			at := i + j
			i = at + len(f)
			// from the line's start, or from where a word starts no further
			// back than back: what the pattern takes for the text's start is
			// where a line or a word starts in s too. The line's start is
			// looked for no further back than that: a text of one long
			// line, as a request body is, isn't read back to its start for
			// each marker.
			start := max(0, at-r.back)
			if nl := strings.LastIndexByte(s[start:at], '\n'); nl >= 0 {
				start += nl + 1
			} else if start > 0 {
				for start < at && (strings.IndexByte(alnum+"_.-", s[start-1]) >= 0 || s[start]&0xc0 == 0x80) {
					start++
				}
				if start == at && strings.IndexByte(alnum+"_", s[at-1]) >= 0 {
					continue // inside a longer word
				}
			}
			wins = append(wins, [2]int{start, min(len(s), i+r.ahead)})
		}
	}
	sort.Slice(wins, func(i, j int) bool { return wins[i][0] < wins[j][0] })
	var out [][]int
	for k := 0; k < len(wins); k++ {
		w := wins[k]
		for k+1 < len(wins) && wins[k+1][0] <= w[1] {
			k++
			w[1] = max(w[1], wins[k][1])
		}
		for _, m := range r.findAll(s[w[0]:w[1]]) {
			for x := range m {
				if m[x] >= 0 {
					m[x] += w[0]
				}
			}
			out = append(out, m)
		}
	}
	return out
}

// findAll are r's matches in s: each of them, or, for a rule that reads
// a field's value after its name (field), none whose value is the next
// field (fieldMatches).
func (r rule) findAll(s string) [][]int {
	if r.field {
		return fieldMatches(r.re, s)
	}
	return r.re.FindAllStringSubmatchIndex(s, -1)
}

// lowerASCII is s with its ASCII letters in lower case, as long as s, so a
// place in one is the same place in the other (a byte of a character
// outside ASCII is never an ASCII letter). It is s itself, not a copy,
// where s has no capital.
func lowerASCII(s string) string {
	i := 0
	for i < len(s) && !('A' <= s[i] && s[i] <= 'Z') {
		i++
	}
	if i == len(s) {
		return s
	}
	b := []byte(s)
	for ; i < len(b); i++ {
		if c := b[i]; 'A' <= c && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

// takes says s[a:b] is a value r masks: nothing it may not touch beside
// it, and its checks pass.
func (r rule) takes(s string, a, b int, ok func(string) bool) bool {
	if b <= a {
		return false
	}
	if r.bound != "" && (a > 0 && strings.IndexByte(r.bound, s[a-1]) >= 0 || b < len(s) && strings.IndexByte(r.bound, s[b]) >= 0) {
		return false
	}
	if ok != nil && !ok(s[a:b]) {
		return false
	}
	return r.at == nil || r.at(s, a, b)
}

// apply swaps each of found in s for what put makes of its value, the
// first and longest where they overlap, nothing inside a placeholder. A
// value o allows is left as it is, and one of a kind o warns of is left
// and told. o.Seen, where set, is told of each.
func apply(s string, found []span, put func(kind, v string) string, o Options) (string, int) {
	if len(found) == 0 {
		return s, 0
	}
	// nothing inside a placeholder already there, or a made-up key
	found = outsideShapes(s, found)
	if strings.Contains(s, "{{") {
		for _, p := range placeholderRe.FindAllStringIndex(s, -1) {
			found = slices.DeleteFunc(found, func(f span) bool { return f.start < p[1] && f.end > p[0] })
		}
		if len(found) == 0 {
			return s, 0
		}
	}
	// the first to start, and of those the longest, wins where they overlap;
	// of two the same, the rule found first, so a value's placeholder is the
	// same every time (an API key in API_KEY=… is an API_KEY, not a SECRET)
	sort.SliceStable(found, func(i, j int) bool {
		if found[i].start != found[j].start {
			return found[i].start < found[j].start
		}
		return found[i].end > found[j].end
	})
	var b strings.Builder
	last, next, n := 0, 0, 0 // next: where the next value may start
	for _, f := range found {
		if f.start < next {
			continue
		}
		next = f.end
		v := s[f.start:f.end]
		if o.allows(v) {
			continue
		}
		act := o.Actions[f.kind]
		if act != Warn && act != Block {
			act = ""
		}
		h := Hit{Kind: f.kind, By: f.by, Action: act}
		h.Head, h.Len = Hint(f.kind, v)
		if act == Warn {
			if o.Seen != nil {
				h.Placeholder = tag(f.kind, v)
				o.Seen(h)
			}
			continue
		}
		b.WriteString(s[last:f.start])
		h.Placeholder = put(f.kind, v)
		if f.bare {
			b.WriteString(`"` + h.Placeholder + `"`)
		} else {
			b.WriteString(h.Placeholder)
		}
		if o.Seen != nil {
			o.Seen(h)
		}
		last, n = f.end, n+1
	}
	b.WriteString(s[last:])
	return b.String(), n
}

// allows says the user lets v go as written (Options.Allow).
func (o Options) allows(v string) bool {
	for _, a := range o.Allow {
		if strings.EqualFold(strings.TrimSpace(a), v) {
			return true
		}
	}
	return false
}

func containsAny(s string, subs []string) bool {
	for _, x := range subs {
		if strings.Contains(s, x) {
			return true
		}
	}
	return false
}
