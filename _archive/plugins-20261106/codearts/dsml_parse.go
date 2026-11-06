package main

import (
	"encoding/json"
	"strconv"
	"strings"
)

func parseDsmlToolCalls(block string) ([]dsmlCall, bool) {
	inner := block
	if strings.HasPrefix(inner, dsmlCallsOpen) {
		inner = inner[len(dsmlCallsOpen):]
	}
	if strings.HasSuffix(inner, dsmlCallsClose) {
		inner = inner[:len(inner)-len(dsmlCallsClose)]
	}
	var calls []dsmlCall
	cursor := 0
	for {
		start := strings.Index(inner[cursor:], dsmlInvokeOpen)
		if start == -1 {
			break
		}
		start += cursor
		openEnd := strings.Index(inner[start:], ">")
		if openEnd == -1 {
			break
		}
		openEnd += start
		closeStart := strings.Index(inner[openEnd+1:], dsmlInvokeEnd)
		if closeStart == -1 {
			break
		}
		closeStart += openEnd + 1
		invoke := inner[start : closeStart+len(dsmlInvokeEnd)]
		call, ok := parseDsmlInvoke(invoke)
		if !ok {
			return nil, false
		}
		calls = append(calls, call)
		cursor = closeStart + len(dsmlInvokeEnd)
	}
	return calls, true
}

func parseDsmlInvoke(block string) (dsmlCall, bool) {
	m := dsmlNameAttr.FindStringSubmatch(block)
	if m == nil {
		return dsmlCall{}, false
	}
	params := map[string]any{}
	cursor := 0
	for {
		start := strings.Index(block[cursor:], dsmlParamOpen)
		if start == -1 {
			break
		}
		start += cursor
		openEnd := strings.Index(block[start:], ">")
		if openEnd == -1 {
			break
		}
		openEnd += start
		openTag := block[start : openEnd+1]
		nameMatch := dsmlNameAttr.FindStringSubmatch(openTag)
		if nameMatch == nil {
			cursor = openEnd + 1
			continue
		}
		closeStart := strings.Index(block[openEnd+1:], dsmlParamEnd)
		if closeStart == -1 {
			break
		}
		closeStart += openEnd + 1
		value := block[openEnd+1 : closeStart]
		if dsmlStringArg.MatchString(openTag) {
			params[nameMatch[1]] = tryParseScalar(value)
		} else if parsed, err := parseJSONValue(value); err == nil {
			if s, isString := parsed.(string); isString {
				params[nameMatch[1]] = tryParseScalar(s)
			} else {
				params[nameMatch[1]] = parsed
			}
		} else {
			params[nameMatch[1]] = value
		}
		cursor = closeStart + len(dsmlParamEnd)
	}
	args, err := json.Marshal(params)
	if err != nil {
		return dsmlCall{}, false
	}
	return dsmlCall{Name: m[1], Arguments: string(args)}, true
}

func parseJSONValue(value string) (any, error) {
	dec := json.NewDecoder(strings.NewReader(value))
	dec.UseNumber()
	var out any
	if err := dec.Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

func tryParseScalar(value string) any {
	if value == "" {
		return ""
	}
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return value
	}
	switch trimmed {
	case "null":
		return nil
	case "true":
		return true
	case "false":
		return false
	}
	if len(trimmed) >= 2 && strings.HasPrefix(trimmed, `"`) && strings.HasSuffix(trimmed, `"`) {
		var decoded string
		if err := json.Unmarshal([]byte(trimmed), &decoded); err == nil {
			return tryParseScalar(decoded)
		}
	}
	if intScalar.MatchString(trimmed) {
		if n, err := strconv.ParseInt(trimmed, 10, 64); err == nil {
			return n
		}
	}
	if floatScalar.MatchString(trimmed) {
		if f, err := strconv.ParseFloat(trimmed, 64); err == nil {
			return f
		}
	}
	if strings.HasPrefix(trimmed, "[") || strings.HasPrefix(trimmed, "{") {
		if parsed, err := parseJSONValue(trimmed); err == nil {
			return parsed
		}
	}
	return value
}
