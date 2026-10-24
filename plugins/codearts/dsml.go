package main

import (
	"regexp"
	"strings"
)

// DeepSeek v4 flash/pro do not accept OpenAI `tools`; the reference adapter
// injects a DSML system prompt and parses DSML tool calls out of the text
// stream. These literals must match the upstream model exactly.
const (
	dsmlCallsOpen  = "<\uFF5CDSML\uFF5Ctool_calls>"
	dsmlCallsClose = "</\uFF5CDSML\uFF5Ctool_calls>"
	dsmlInvokeOpen = "<\uFF5CDSML\uFF5Cinvoke"
	dsmlInvokeEnd  = "</\uFF5CDSML\uFF5Cinvoke>"
	dsmlParamOpen  = "<\uFF5CDSML\uFF5Cparameter"
	dsmlParamEnd   = "</\uFF5CDSML\uFF5Cparameter>"
	thoughtOpen    = "<thought>"
	thoughtClose   = "</thought>"
)

var (
	dsmlNameAttr  = regexp.MustCompile(`name\s*=\s*"([^"]*)"`)
	dsmlStringArg = regexp.MustCompile(`string\s*=\s*"true"`)
	intScalar     = regexp.MustCompile(`^-?\d+$`)
	floatScalar   = regexp.MustCompile(`^-?\d+\.\d+$`)
)

type dsmlCall struct {
	Name      string
	Arguments string
}

type dsmlExtractor struct {
	buf   strings.Builder
	state int // 0 normal, 1 in-thought, 2 in-dsml
}

func (d *dsmlExtractor) feed(chunk string) (text, reasoning string, calls []dsmlCall) {
	d.buf.WriteString(chunk)
	for {
		switch d.state {
		case 0:
			current := d.buf.String()
			thoughtIdx := strings.Index(current, thoughtOpen)
			dsmlIdx := strings.Index(current, dsmlCallsOpen)
			openIdx, nextState := -1, 1
			if thoughtIdx != -1 && (dsmlIdx == -1 || thoughtIdx < dsmlIdx) {
				openIdx, nextState = thoughtIdx, 1
			} else if dsmlIdx != -1 {
				openIdx, nextState = dsmlIdx, 2
			}
			if openIdx == -1 {
				keep := longestOpenPrefixTail(current, thoughtOpen, dsmlCallsOpen)
				if keep == 0 {
					text += current
					d.reset()
				} else if len(current) > keep {
					text += current[:len(current)-keep]
					d.set(current[len(current)-keep:])
				}
				return
			}
			if openIdx > 0 {
				text += current[:openIdx]
			}
			rest := current[openIdx:]
			openLen := len(thoughtOpen)
			if nextState == 2 {
				openLen = len(dsmlCallsOpen)
			}
			d.set(rest[openLen:])
			d.state = nextState
		case 1:
			current := d.buf.String()
			idx := strings.Index(current, thoughtClose)
			if idx == -1 {
				keep := longestOpenPrefixTail(current, thoughtClose)
				if keep == 0 {
					reasoning += current
					d.reset()
				} else if len(current) > keep {
					reasoning += current[:len(current)-keep]
					d.set(current[len(current)-keep:])
				}
				return
			}
			reasoning += current[:idx]
			d.set(current[idx+len(thoughtClose):])
			d.state = 0
		default:
			current := d.buf.String()
			idx := strings.Index(current, dsmlCallsClose)
			if idx == -1 {
				return
			}
			block := current[:idx+len(dsmlCallsClose)]
			parsed, ok := parseDsmlToolCalls(block)
			if !ok {
				text += block
			} else {
				calls = append(calls, parsed...)
			}
			d.set(current[idx+len(dsmlCallsClose):])
			d.state = 0
		}
	}
}

func (d *dsmlExtractor) flush() (text, reasoning string) {
	remaining := d.buf.String()
	d.reset()
	switch d.state {
	case 1:
		d.state = 0
		return "", remaining
	case 2:
		d.state = 0
		return dsmlCallsOpen + remaining, ""
	default:
		d.state = 0
		return remaining, ""
	}
}

func (d *dsmlExtractor) set(s string) {
	d.buf.Reset()
	d.buf.WriteString(s)
}

func (d *dsmlExtractor) reset() { d.set("") }

// longestOpenPrefixTail holds back a trailing fragment that could be the start
// of one of the openers, so a split chunk never leaks a partial tag as text.
func longestOpenPrefixTail(buffer string, prefixes ...string) int {
	keep := 0
	maxCheck := len(buffer)
	maxPrefix := 0
	for _, p := range prefixes {
		if len(p) > maxPrefix {
			maxPrefix = len(p)
		}
	}
	if maxCheck > maxPrefix {
		maxCheck = maxPrefix
	}
	for i := 1; i <= maxCheck; i++ {
		tail := buffer[len(buffer)-i:]
		for _, p := range prefixes {
			if strings.HasPrefix(p, tail) {
				keep = i
				break
			}
		}
	}
	return keep
}
