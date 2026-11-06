package gateway

// P10-2 接线（task-21）：把 internal/redact 三件套接进转发链路。
//
// 本文件是独立接线文件，不 import internal/config：redact 开关与 key 经
// gateway.Options.Redact（解锁后在 gateway.go 的 Options 加字段）由 app 层注入。
//
// 解锁后要在既有文件加的钩子清单（每处 ≤10 行，回执逐条列出）：
//   1. gateway.go  Options struct  加 `Redact RedactOptions`（On/KeyFile/RouteAware）；
//   2. gateway.go  newGateway      存 `g.redact = opts.Redact`（Gateway 新增该字段）；
//   3. gateway.go  forwardPlugin   L1183 后：`body = g.redact.MaskJSONBody(rewritten, id)`
//      （Route = 目标插件 id）；
//   4. （A 内置上游已退役，原 forwardA 钩子随之删除；裸模型名不再有隐式上游）
//   5. gateway.go  forward         响应写循环（L1317 起）：`w, finish := g.redact.RespWriter(w)`，
//      Write 改走 w、Flush 用 w.Flush()、写循环结束后 `finish()`（= NewWriter.Finish）；
//   6. app 层构造 gateway.Options 处：`Redact: redactOptionsFromConfig(c)` 一行；
//   7. app 启动处：`redact.SetKeyPath(c.Redact.KeyFile)`（缺省 <dataDir>\redact.key）。
//
// 语义：入站 MaskJSON（Secrets+Personal，Route=provider id）；出站 NewWriter 回程还原
// （SSE 半帧持留 / 整包 Finish 一次性还原），客户端看不到 {{KIND_指纹}}；审计走
// redact.Audit（类别/条数/行号/耗时，无原文），由调用方喂给现有日志通道。

import (
	"net/http"
	"strconv"
	"strings"

	"aiworkgate/internal/redact"
)

// RedactOptions 是一份脱敏配置（镜像 internal/config 的 redact 段）。
type RedactOptions struct {
	On         bool   // 总开关；本阶段缺省 false（先可配后默认开）
	KeyFile    string // 占位符指纹 key 的落盘路径；缺省 <dataDir>\redact.key
	RouteAware bool   // Route 感知；缺省 true
	// Kinds 非空时只脱这些 kind（SECRET/EMAIL/PHONE/…）；Categories 非空时只开
	// 这些类别 id（ssn/passport/…）。都为空 = 规则默认表（internal/redact）。
	Kinds      []string
	Categories []string
}

// kindsMap 把 Kinds（kind 名）与 Categories（类别 id）并成一个 Options.Kinds
// 的 category-id 开关表（kind 经 redact.Categories 反查其类别；未知 id 忽略）。
// 空表返回 nil = 用规则默认表。
func (o RedactOptions) kindsMap() map[string]bool {
	var m map[string]bool
	add := func(id string) {
		if id == "" {
			return
		}
		if m == nil {
			m = map[string]bool{}
		}
		m[id] = true
	}
	for _, c := range o.Categories {
		add(strings.ToLower(strings.TrimSpace(c)))
	}
	if len(o.Kinds) > 0 {
		catOf := map[string]string{}
		for _, c := range redact.Categories {
			for _, k := range c.Kinds {
				catOf[k] = c.ID
			}
		}
		for _, k := range o.Kinds {
			add(catOf[strings.ToUpper(strings.TrimSpace(k))])
		}
	}
	return m
}

// Wiring 是一次转发的脱敏上下文：入站 mask + 出站还原 + 审计统计。
type Wiring struct {
	on         bool
	routeAware bool
	kinds      map[string]bool // 非 nil = 只脱这些类别 id

	audit  *redact.Audit
	masked int

	writer *redact.Writer
}

// NewWiring 从配置建一次转发的上下文；on=false 时所有方法都原样透传（零开销）。
func NewWiring(o RedactOptions) *Wiring {
	return &Wiring{on: o.On, routeAware: o.RouteAware, kinds: o.kindsMap()}
}

// Enabled 报告脱敏是否开启。
func (w *Wiring) Enabled() bool { return w != nil && w.on }

// MaskJSONBody 是入站钩子：开启时把 body 走 MaskJSON（Route=route，目标 provider id），
// 失败保持原样（fail-open，记录 masked=0；错误由审计行带出，不打断请求）。
// 返回值是真正要发给上游的字节。
func (w *Wiring) MaskJSONBody(body []byte, route string) []byte {
	if !w.Enabled() || len(body) == 0 {
		return body
	}
	o := redact.Options{Secrets: true, Personal: true}
	if w.kinds != nil {
		o.Kinds = w.kinds
	}
	if w.routeAware && route != "" {
		o.Route = route
	}
	out, audit := redact.MaskJSONAudit(body, o)
	w.audit = audit
	w.masked = audit.Total()
	if len(out) == 0 && len(body) > 0 {
		// 理论上不会发生（MaskJSON 非 JSON 时退化 Mask）；保守退回原体
		return body
	}
	return out
}

// Masked 是本次入站脱掉的值总数（0 = 没开或没脱到）。
func (w *Wiring) Masked() int {
	if w == nil {
		return 0
	}
	return w.masked
}

// Audit 是本次入站的审计（类别/条数/行号/耗时，绝无原文）；nil = 未开。
func (w *Wiring) Audit() *redact.Audit {
	if w == nil {
		return nil
	}
	return w.audit
}

// AuditLine 把审计压成一行日志文本（不含任何被脱敏的值）。Elapsed 以审计记录为准。
func (w *Wiring) AuditLine() string {
	if w == nil || w.audit == nil {
		return ""
	}
	records := w.audit.Records()
	if len(records) == 0 {
		return "redact: no values masked"
	}
	var b strings.Builder
	b.WriteString("redact masked: ")
	for i, r := range records {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(r.Kind)
		b.WriteByte('=')
		b.WriteString(strconv.Itoa(r.Count))
		b.WriteByte('@')
		b.WriteString(strconv.Itoa(r.Line))
	}
	b.WriteString(" elapsed=")
	b.WriteString(w.audit.Elapsed().String())
	return b.String()
}

// RespWriter 是出站钩子：开启时把 w 包成 redact.Writer（回程还原），
// 返回包好的 ResponseWriter 和一个必须在响应结束时调用的 finish 函数
// （= NewWriter.Finish；SSE 半帧持留在 Finish 收尾）。未开时原样返回 w。
func (w *Wiring) RespWriter(rw http.ResponseWriter) (http.ResponseWriter, func()) {
	if !w.Enabled() {
		return rw, nil
	}
	writer := redact.NewWriter(rw)
	w.writer = writer
	return writer, func() { writer.Finish() }
}
