// 六维二期：文件/记忆启发式桶（task-67/74，默认关）。
//
// 判据（docs/plans/2026-11-08-六维treemap设计要点.md §2 草案，全部四项）：
//   - 路径串（task-67）：会话文本块里的 Windows 盘符路径、带目录前缀的 POSIX
//     路径。URL（`://` 前缀）排除；裸文件名（main.go）不命中（误判太宽）。
//   - 记忆（task-67）：块级判定——文本含 `<memory` 标签或开头出现
//     CLAUDE.md/AGENTS.md 标记。无标记漏归回会话，无伤。
//   - 代码围栏（task-74）：```/~~~ 围栏块与路径串**强关联**（块前 1~2 行内或
//     闭栏后首行命中路径正则）时整块归 files；无关联不猜测。
//   - `<file path="…">` 约定标签（task-74）：整块归 files，path 记入 item。
//
// 关闭时（默认）启发式完全不参与：六桶结果与一期逐字节一致。
package gateway

import (
	"math"
	"regexp"
	"strings"
)

// ctxSectionsHeuristics 是启发式开启时的固定桶序（files 在工具结果之后）。
var ctxSectionsHeuristics = []string{
	"system", "tools", "tool_results", "files", "conversation", "memory", "other",
}

// ctxFileWindowsRe matches drive-letter paths: `C:\Users\zyf\main.go`. The
// segment class excludes spaces (paths in prose rarely carry them, and a space
// would make the greedy run swallow the next POSIX path on the same line).
var ctxFileWindowsRe = regexp.MustCompile(`[A-Za-z]:\\(?:[\w@.-]+\\)*[\w@.-]{1,120}`)

// ctxFilePosixRe matches POSIX paths with at least one directory segment and a
// file name with an extension: `/Users/zyf/src/main.go`, `src/api.ts` is NOT
// matched (no leading slash — too loose in prose).
var ctxFilePosixRe = regexp.MustCompile(`/(?:[\w@.-]+/)+[\w@.-]{1,120}\.[A-Za-z]{1,8}`)

// ctxMemoryTagRe / ctxMemoryMarkerRe are the block-level memory markers.
var (
	ctxMemoryTagRe    = regexp.MustCompile(`<memory[\s>/]`)
	ctxMemoryMarkerRe = regexp.MustCompile(`(?:^|\n)\s*(?:#{1,3}\s*)?(?:CLAUDE|AGENTS)\.md\b`)
)

// ctxMemoryMarkerHead caps where the marker must appear for a block-level hit
// (memory blocks are injected at the head of their text).
const ctxMemoryMarkerHead = 120

// ctxFileSegment is one phase-2 file-bound piece of a text block: its token
// estimate and the drill-down label (a path or a `<file path>` attribute).
type ctxFileSegment struct {
	tokens float64
	label  string
}

// ctxFileTagRe matches the `<file path="...">…</file>` convention block
// (task-74). The path attribute becomes the drill-down label.
var ctxFileTagRe = regexp.MustCompile(`(?s)<file\s+path="([^"]{1,200})"\s*>(.*?)</file\s*>`)

// ctxArtifactBlockRe matches the `<artifact …>…</artifact>` convention block
// (task-80). Group 1 = the optional attribute string, group 2 = content.
// The attribute string is scanned for path/title to build the drill-down label.
var ctxArtifactBlockRe = regexp.MustCompile(`(?s)<artifact(\s[^>]*)?>(.*?)</artifact\s*>`)

// ctxArtifactPathRe / ctxArtifactTitleRe extract the drill-down label from
// the artifact tag's attribute string: path wins, title is the fallback.
var (
	ctxArtifactPathRe  = regexp.MustCompile(`\bpath="([^"]{1,200})"`)
	ctxArtifactTitleRe = regexp.MustCompile(`\btitle="([^"]{1,200})"`)
)

// ctxArtifactLabel picks the drill-down label from an artifact tag's
// attribute string: path > title > the literal `<artifact>` fallback.
func ctxArtifactLabel(attrs string) string {
	if m := ctxArtifactPathRe.FindStringSubmatch(attrs); m != nil {
		return m[1]
	}
	if m := ctxArtifactTitleRe.FindStringSubmatch(attrs); m != nil {
		return m[1]
	}
	return "<artifact>"
}

// ctxFenceBlockRe matches one code fence (``` or ~~~) with its body, up to the
// nearest closing fence of either marker. The non-greedy body keeps nested-ish
// inputs from swallowing the whole message; imperfect fences degrade to the
// plain path scan below, never to a wrong files assignment. (The backtick runs
// are string-concatenated: a raw literal cannot contain them.)
var ctxFenceBlockRe = regexp.MustCompile("(?s)(?:^|\n)(" + "```" + "|~~~)[ \t]*[^\n]*\n(.*?)\n(" + "```" + "|~~~)(?:\n|$)")

// ctxFenceLinesBack bounds how many lines before a fence may carry the path
// that associates it (task-74: strong association, 1~2 lines).
const ctxFenceLinesBack = 2

// ctxFenceAssociatedPath checks the fence's strong-association rule: a path hit
// in the 1~2 lines before the opening fence or in the first line after the
// closing fence. Returns the longest associated path, "" when unassociated.
func ctxFenceAssociatedPath(text string, openStart, closeEnd int) string {
	// 前 1~2 行：从 openStart 向前截两行。
	beforeStart := openStart
	for lines := 0; lines < ctxFenceLinesBack && beforeStart > 0; {
		beforeStart--
		if beforeStart == 0 || text[beforeStart-1] == '\n' {
			lines++
		}
	}
	before := text[beforeStart:openStart]
	tokens, best := ctxFileTokens(before)
	if tokens > 0 && best != "" {
		return best
	}
	// 围栏后首行。
	afterStart := closeEnd
	if afterStart < len(text) && text[afterStart] == '\n' {
		afterStart++
	}
	afterEnd := strings.IndexByte(text[afterStart:], '\n')
	if afterEnd < 0 {
		afterEnd = len(text)
	} else {
		afterEnd += afterStart
	}
	tokens, best = ctxFileTokens(text[afterStart:afterEnd])
	if tokens > 0 && best != "" {
		return best
	}
	return ""
}

// ctxFileSegments extracts the file-bound pieces of one text block under the
// phase-2 heuristics, in priority order:
//  1. `<file path="…">` convention blocks — wholesale, label = path attribute;
//  2. code fences strongly associated with a path (path hit in the 1~2 lines
//     before the fence or the first line after it) — wholesale, label = path;
//  3. leftover text: bare path-shaped substrings (task-67).
//
// Consumed segments are cut out of the returned rest so the caller prices only
// what stays behind (token conservation: segment estimates are additive).
func ctxFileSegments(text string) (segments []ctxFileSegment, rest string) {
	rest = text
	// 1. `<file path>` 约定标签整块。
	for {
		loc := ctxFileTagRe.FindStringSubmatchIndex(rest)
		if loc == nil {
			break
		}
		path := rest[loc[2]:loc[3]]
		content := rest[loc[4]:loc[5]]
		segments = append(segments, ctxFileSegment{
			tokens: estimateTextTokens(content),
			label:  path,
		})
		rest = rest[:loc[0]] + " " + rest[loc[1]:]
	}
	// 1b. `<artifact>` 约定标签整块（task-80）：label = path > title > `<artifact>`。
	for {
		loc := ctxArtifactBlockRe.FindStringSubmatchIndex(rest)
		if loc == nil {
			break
		}
		attrs := ""
		if loc[2] >= 0 {
			attrs = rest[loc[2]:loc[3]]
		}
		content := rest[loc[4]:loc[5]]
		segments = append(segments, ctxFileSegment{
			tokens: estimateTextTokens(content),
			label:  ctxArtifactLabel(attrs),
		})
		rest = rest[:loc[0]] + " " + rest[loc[1]:]
	}
	// 2. 与路径强关联的代码围栏。
	for {
		loc := ctxFenceBlockRe.FindStringSubmatchIndex(rest)
		if loc == nil {
			break
		}
		if path := ctxFenceAssociatedPath(rest, loc[0], loc[1]); path != "" {
			segments = append(segments, ctxFileSegment{
				tokens: estimateTextTokens(rest[loc[2]:loc[3]]),
				label:  path,
			})
			rest = rest[:loc[0]] + " " + rest[loc[1]:]
			continue
		}
		// 无关联的围栏不消费：跳过它的开栏，避免同一段反复命中。为简单起见
		// 把开栏两字符替换成空白，围栏体留在 rest 里按普通文本计价。
		rest = rest[:loc[0]] + "  " + rest[loc[0]+2:]
	}
	// 3. 剩余文本的裸路径串（task-67 口径）；命中串从 rest 剔除为等长空白，
	// 避免 files 与 conversation 双计（等长 ASCII/路径字符同权重，估算守恒；
	// URL 命中虽被 ctxFileTokens 跳过，等长替换也不改变其 conversation 份额）。
	if tokens, best := ctxFileTokens(rest); tokens > 0 && best != "" {
		segments = append(segments, ctxFileSegment{tokens: tokens, label: best})
		for _, re := range []*regexp.Regexp{ctxFileWindowsRe, ctxFilePosixRe} {
			rest = re.ReplaceAllStringFunc(rest, func(hit string) string {
				return strings.Repeat(" ", len([]rune(hit)))
			})
		}
	}
	return segments, rest
}

// ctxClassifyText splits one text block into (files, memory, rest) under the
// phase-2 heuristics: a memory-marker block goes wholesale to memory; otherwise
// the file segments (task-74 fence/file-tag rules plus bare paths) are cut out
// and whatever remains stays in conversation. Sum(rule) conserves tokens.
func ctxClassifyText(text string) (fileTokens float64, memoryTokens float64, rest string, segments []ctxFileSegment) {
	if ctxMemoryBlockHit(text) {
		return 0, estimateTextTokens(text), "", nil
	}
	segments, rest = ctxFileSegments(text)
	for _, segment := range segments {
		fileTokens += segment.tokens
	}
	return fileTokens, 0, rest, segments
}

// ctxSplitTextHeuristics walks one message's text-bearing blocks (string
// content, block.text spellings; tool_result and image blocks are excluded —
// their tokens live in tool_results / the image flat rate) and returns the
// files/memory split with one drill-down row per file segment.
func ctxSplitTextHeuristics(message map[string]any) (files, memory float64, items []CtxItem) {
	for _, key := range []string{"content", "parts"} {
		value, ok := message[key]
		if !ok {
			continue
		}
		switch blocks := value.(type) {
		case string:
			f, m, _, segments := ctxClassifyText(blocks)
			files += f
			memory += m
			for _, segment := range segments {
				items = append(items, CtxItem{Bucket: "files", Label: ctxLabel("file", segment.label), Tokens: int(math.Ceil(segment.tokens))})
			}
		case []any:
			for _, rawBlock := range blocks {
				block, _ := rawBlock.(map[string]any)
				if block == nil {
					continue
				}
				if strings.ToLower(jsonStringValue(block["type"])) == "tool_result" {
					continue // tool_results owns those tokens
				}
				text, ok := block["text"].(string)
				if !ok {
					continue
				}
				f, m, _, segments := ctxClassifyText(text)
				files += f
				memory += m
				for _, segment := range segments {
					items = append(items, CtxItem{Bucket: "files", Label: ctxLabel("file", segment.label), Tokens: int(math.Ceil(segment.tokens))})
				}
			}
		}
	}
	return files, memory, items
}

// ctxFileTokens finds file-path substrings inside one text block and returns
// their estimated tokens plus the longest hit (drill-down label). URL internals
// (`://`) are skipped so links do not count as files.
func ctxFileTokens(text string) (float64, string) {
	total := 0.0
	best := ""
	bestLen := 0
	for _, re := range []*regexp.Regexp{ctxFileWindowsRe, ctxFilePosixRe} {
		for _, loc := range re.FindAllStringIndex(text, 16) {
			// Skip matches that are part of a URL scheme: walk back over the
			// leading slashes and refuse anything preceded by `:` (`://`).
			start := loc[0]
			for start > 0 && text[start-1] == '/' {
				start--
			}
			if start > 0 && text[start-1] == ':' {
				continue
			}
			hit := text[loc[0]:loc[1]]
			tokens := estimateTextTokens(hit)
			total += tokens
			if len(hit) > bestLen {
				best, bestLen = hit, len(hit)
			}
		}
	}
	return total, best
}

// ctxMemoryBlockHit reports whether a text block is a memory block by the
// convention markers (tag anywhere, or CLAUDE.md/AGENTS.md marker near the head).
func ctxMemoryBlockHit(text string) bool {
	if ctxMemoryTagRe.MatchString(text) {
		return true
	}
	head := text
	if len([]rune(head)) > ctxMemoryMarkerHead {
		head = string([]rune(head)[:ctxMemoryMarkerHead])
	}
	return ctxMemoryMarkerRe.MatchString(head)
}
