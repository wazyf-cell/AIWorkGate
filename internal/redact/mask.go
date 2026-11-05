// 从 magpie internal/redact 移植（逐节等价切分自原单文件 redact.go；
// 拆分只调整文件边界与 import，不改变任何逻辑，见 README 的来源与完整性节）。
package redact

import (
	"slices"
	"strings"
	"unicode/utf8"
)

// mask swaps what o covers in s for what put makes of each value.
//
// escaped says s holds JSON text of its own (asJSON: a tool call's
// arguments), which the vendor reads after one more unescaping. There the
// finders read s as the vendor does (unescapedView): the \n before
// @mcp.tool is a newline, not an n that starts an address, and the \n
// before jo@quill.dev is no letter of it either. No value starts or ends
// inside an escape (outsideEscapes), and what is kept to put back is the
// value as it reads unescaped, so Restore, which escapes it again in
// arguments, gives the agent what it wrote. Read as plain text, the n of
// a \n went as the first letter of n@mcp.tool, the backslash was left
// before the placeholder (\{{EMAIL_…}}), the arguments were JSON no
// longer, and Ollama Cloud turned away every turn that replayed the call
// with 400 "invalid tool call arguments" (#3022).
func mask(raw string, o Options, put func(kind, v string) string, escaped bool) (string, int) {
	if len(raw) < 3 {
		return raw, 0
	}
	s := raw // what the finders read: as long as raw, a place in one is the same in the other
	if escaped {
		s = unescapedView(raw)
		put = unescapedPut(put)
	}
	var found []span
	for _, w := range o.Words {
		if w = strings.TrimSpace(w); len(w) < 2 {
			continue
		}
		// a date is matched as that date, however it is written (dateword.go)
		if spans, ok := dateSpans(s, w); ok {
			found = append(found, spans...)
			continue
		}
		// next finds w at or after i; one inside a longer word, where the
		// user asked for whole words, is passed over a letter at a time, so
		// "XABC ABC" still finds the second
		next := func(i int) (int, int) {
			if o.WordsAnyCase {
				if m := anyCase(w).FindStringIndex(s[i:]); m != nil {
					return i + m[0], i + m[1]
				}
				return -1, -1
			}
			if j := strings.Index(s[i:], w); j >= 0 {
				return i + j, i + j + len(w)
			}
			return -1, -1
		}
		for i := 0; i <= len(s); {
			a, b := next(i)
			if a < 0 {
				break
			}
			if o.WordsWhole && !wholeWord(s, a, b) {
				_, n := utf8.DecodeRuneInString(s[a:])
				i = a + max(n, 1)
				continue
			}
			found = append(found, span{a, b, "TERM", ByWord, 0, false})
			i = max(b, a+1)
		}
	}
	found = append(found, ruleSpans(s, o)...)
	// after the rules', so where a rule finds the same value it keeps the
	// kind it has always had
	if o.sent {
		// the model was asked about raw
		now := modelSpans(raw, o)
		kept := sentSpans(sentModel, raw, o, func() []span { return now })
		learnLate(raw, now, kept, o, escaped)
		found = append(found, kept...)
		found = append(found, sentSpans(sentKnown, s, o, func() []span { return knownSpans(s, o) })...)
	} else {
		found = append(found, modelSpans(raw, o)...) // the model was asked about raw
		found = append(found, knownSpans(s, o)...)
	}
	if escaped {
		found = inStrings(raw, outsideEscapes(raw, found))
	}
	found = noNumbers(s, noIDs(s, found))
	if o.sent {
		noteScores(raw, found) // what is masked for a vendor, not a preview
	}
	return apply(raw, found, put, o)
}

// ruleSpans are what the rules, magpie's and the user's own, find in s,
// the text the finders read.
func ruleSpans(s string, o Options) []span {
	return memoRules(s, o, func() []span {
		if len(s) >= ruleMemoMin {
			ruleRuns.Add(1)
		}
		var found []span
		all := rules
		if o.Secrets && len(o.Rules) > 0 {
			// the user's first: where one of theirs and one of magpie's find
			// the same value, it goes by the name they gave it
			all = append(customRules(o.Rules), rules...)
		}
		lower := ""
		// split are matches a rule took apart into values of their own
		// (rkato@box17 as a user and a host): what another rule found from
		// the same place in them, an email, goes
		var split []span
		for _, r := range all {
			if !o.covers(r.cat) {
				continue
			}
			if len(r.markers) > 0 && !containsAny(s, r.markers) {
				continue
			}
			if len(r.fold) > 0 {
				if lower == "" {
					lower = lowerASCII(s)
				}
				if !containsAny(lower, r.fold) {
					continue
				}
			}
			for _, m := range r.matches(s, lower) {
				if len(r.groups) > 0 {
					took := false
					for g, kind := range r.groups {
						if a, b := m[2*g+2], m[2*g+3]; a >= 0 && r.takes(s, a, b, r.groupOK[g]) {
							found = append(found, span{a, b, kind, r.by(), 0, false})
							took = true
						}
					}
					if took {
						split = append(split, span{m[2], m[1], "", "", 0, false})
					}
					continue
				}
				a, b := m[0], m[1]
				for g := 1; 2*g+1 < len(m); g++ {
					if m[2*g] >= 0 {
						a, b = m[2*g], m[2*g+1]
						break
					}
					if !r.anyGroup {
						break
					}
				}
				ok := r.ok
				if r.fix != nil {
					var good bool
					if a, b, good = r.fix(s, a, b); !good {
						continue
					}
					ok = nil
				}
				if r.takes(s, a, b, ok) {
					found = append(found, span{a, b, r.kind, r.by(), 0, false})
				}
			}
		}
		for _, sp := range split {
			found = slices.DeleteFunc(found, func(f span) bool {
				return f.kind == "EMAIL" && f.start == sp.start && f.end <= sp.end
			})
		}
		return found
	})
}

// learnLate makes known what the model finds in raw now that it didn't
// the first time raw was sent on o.Route (now, beside kept): raw goes as
// it went, but a value in it the model has read since is masked in new
// text, as it would be had raw been masked with it (known.go). Nothing
// is told (o.Seen): it isn't masked here.
func learnLate(raw string, now, kept []span, o Options, escaped bool) {
	var late []span
	for _, f := range now {
		if !slices.ContainsFunc(kept, func(k span) bool { return k.start == f.start && k.end == f.end && k.kind == f.kind }) {
			late = append(late, f)
		}
	}
	if len(late) == 0 {
		return
	}
	if escaped {
		late = outsideEscapes(raw, late)
	}
	noteScores(raw, late)
	o.Seen = nil
	put := o.put()
	if escaped {
		put = unescapedPut(put)
	}
	apply(raw, late, put, o)
}
