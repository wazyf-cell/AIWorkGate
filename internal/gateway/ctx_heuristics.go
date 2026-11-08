// 六维二期：文件/记忆启发式桶（task-67，默认关）。
//
// 判据（docs/plans/2026-11-08-六维treemap设计要点.md §2 草案）：
//   - 文件：会话文本块里的路径串——Windows 盘符路径、带目录前缀的 POSIX 路径
//     （判据草案的 /.../x.ext）。URL（`://` 前缀）排除；裸文件名（main.go）
//     不命中（误判太宽，设计草案也只要求带目录前缀）。
//   - 记忆：块级判定——文本含 `<memory` 标签或开头出现 CLAUDE.md/AGENTS.md
//     标记（约定客户端的记忆块通常整块注入）。无标记漏归回会话，无伤。
//
// 关闭时（默认）启发式完全不参与：六桶结果与一期逐字节一致。
package gateway

import (
	"regexp"
	"strings"
)

// ctxSectionsHeuristics 是启发式开启时的固定桶序（files 在工具结果之后）。
var ctxSectionsHeuristics = []string{
	"system", "tools", "tool_results", "files", "conversation", "memory", "other",
}

// ctxFileWindowsRe matches drive-letter paths: `C:\Users\zyf\main.go`.
var ctxFileWindowsRe = regexp.MustCompile(`[A-Za-z]:\\(?:[\w.@ -]+\\)*[\w.@-]{1,120}`)

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

// ctxClassifyText splits one text block into (files, memory, bestPath) under
// the phase-2 heuristics: a memory-marker block goes wholesale to memory;
// otherwise path-shaped substrings are cut out for files. Sum(rule) never
// exceeds estimateTextTokens(text), so the three-way split conserves tokens.
func ctxClassifyText(text string) (fileTokens, memoryTokens float64, bestPath string) {
	if ctxMemoryBlockHit(text) {
		return 0, estimateTextTokens(text), ""
	}
	fileTokens, bestPath = ctxFileTokens(text)
	return fileTokens, 0, bestPath
}

// ctxSplitTextHeuristics walks one message's text-bearing blocks (string
// content, block.text spellings; tool_result and image blocks are excluded —
// their tokens live in tool_results / the image flat rate) and returns the
// files/memory split with the longest path hit as the drill-down label.
func ctxSplitTextHeuristics(message map[string]any) (files, memory float64, bestPath string) {
	for _, key := range []string{"content", "parts"} {
		value, ok := message[key]
		if !ok {
			continue
		}
		switch blocks := value.(type) {
		case string:
			f, m, b := ctxClassifyText(blocks)
			files += f
			memory += m
			if len(b) > len(bestPath) {
				bestPath = b
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
				f, m, b := ctxClassifyText(text)
				files += f
				memory += m
				if len(b) > len(bestPath) {
					bestPath = b
				}
			}
		}
	}
	return files, memory, bestPath
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
