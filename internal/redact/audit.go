package redact

// 审计（P10 必做 ⑤ 的规则层出口）：只记「类别 / 条数 / 行号 / 耗时」，
// **绝不记原文**——AuditRecord 里没有任何字段能携带被脱敏的值、占位符或值的前缀。
//
// 实现不碰 magpie 引擎：复用 Options.Seen 回调收类别与条数（每个被脱的值都会
// 触发一次 Seen），行号则由脱敏输出里的占位符位置推算——占位符与原文逐点对应、
// 换行结构不变，所以「输出中占位符前的换行数+1」就是原文里的行号。
//
// gateway 接线卡（下一张）从 Records() 取数据写日志即可，本包不落盘、不打印。

import (
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// AuditRecord 是审计的一行：类别、该类被脱了几个值、第一个值出现在第几行、耗时。
// Line 是 1 基的行号；Elapsed 是整个 Mask/MaskJSON 调用花的时间。
type AuditRecord struct {
	Kind    string
	Count   int
	Line    int
	Elapsed time.Duration
}

// Audit 收集一次脱敏的统计。零值不可用，用 MaskAudit / MaskJSONAudit 拿。
type Audit struct {
	start   time.Time
	elapsed time.Duration

	mu        sync.Mutex
	counts    map[string]int // kind → 条数
	firstLine map[string]int // kind → 首次出现的行号
	total     int            // 全部被脱的值数
}

func newAudit() *Audit {
	return &Audit{
		start:     time.Now(),
		counts:    map[string]int{},
		firstLine: map[string]int{},
	}
}

// placeholderKindsRe 抓输出里的占位符并读出它的类别。
var placeholderKindsRe = regexp.MustCompile(`\{\{([A-Z]{2,}[A-Z0-9_]*)_[a-z2-7]{8}\}\}`)

// MaskAudit 是 Mask 的审计版本：返回值照常，另带回一份只有统计的审计。
// 传入的 o.Seen 若已有值会先被调用（本函数在它之后补记审计）。
func MaskAudit(s string, o Options) (string, *Audit) {
	a := newAudit()
	seen := o.Seen
	o.Seen = func(h Hit) {
		if seen != nil {
			seen(h)
		}
		if h.Action == "" || h.Action == Block {
			a.mu.Lock()
			a.counts[h.Kind]++
			a.total++
			a.mu.Unlock()
		}
	}
	out, _ := Mask(s, o)
	a.finish(out)
	return out, a
}

// MaskJSONAudit 是 MaskJSON 的审计版本。
func MaskJSONAudit(body []byte, o Options) ([]byte, *Audit) {
	a := newAudit()
	seen := o.Seen
	o.Seen = func(h Hit) {
		if seen != nil {
			seen(h)
		}
		if h.Action == "" || h.Action == Block {
			a.mu.Lock()
			a.counts[h.Kind]++
			a.total++
			a.mu.Unlock()
		}
	}
	out, _ := MaskJSON(body, o)
	a.finish(string(out))
	return out, a
}

// finish 记录耗时并推算每个类别首次出现的行号（1 基）。
func (a *Audit) finish(out string) {
	a.elapsed = time.Since(a.start)
	for _, m := range placeholderKindsRe.FindAllStringSubmatchIndex(out, -1) {
		if m[2] < 0 {
			continue
		}
		kind := out[m[2]:m[3]]
		if _, ok := a.firstLine[kind]; ok {
			continue
		}
		a.firstLine[kind] = 1 + strings.Count(out[:m[0]], "\n")
	}
}

// Total 是被脱的值总数（各类别之和）。
func (a *Audit) Total() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.total
}

// Elapsed 是整个脱敏调用的耗时。
func (a *Audit) Elapsed() time.Duration { return a.elapsed }

// Record 是某一类别的一行审计。
type Record = AuditRecord

// Records 返回全部审计行，按类别名排序。行里没有值、没有占位符、没有值的前缀。
func (a *Audit) Records() []AuditRecord {
	a.mu.Lock()
	defer a.mu.Unlock()
	ks := make([]string, 0, len(a.counts))
	for k := range a.counts {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	out := make([]AuditRecord, 0, len(ks))
	for _, k := range ks {
		out = append(out, AuditRecord{
			Kind:    k,
			Count:   a.counts[k],
			Line:    a.firstLine[k],
			Elapsed: a.elapsed,
		})
	}
	return out
}
