// 六维采样门控与落库钩子（task-62）。sticky 门控（trace.go:5-7 同模式）：
// SetCtxSampling 开启后 forward 路径在改写前的 body 上分析一次并挂 rec；
// 收束时 emit 把 (composition, upstream prompt) 交给宿主 sink 落独立表。
// 关闭时每请求只付一次原子读。
package gateway

import (
	"sync/atomic"
)

// ctxSink 是宿主注入的落库回调（InstallCtxSink 包级安装，gateway 不 import store）。
var (
	ctxSinkMu   atomic.Value // func(CtxEmit)
	ctxSampling atomic.Bool
)

// InstallCtxSink wires the host's persistence callback. Nil disables the sink
// (sampling may still run for tests that read rec.ctxComp directly).
func InstallCtxSink(sink func(CtxEmit)) {
	if sink == nil {
		ctxSinkMu.Store(func(CtxEmit) {})
		return
	}
	ctxSinkMu.Store(sink)
}

// SetCtxSampling flips the sticky gate live (admin switch, next request on).
func (g *Gateway) SetCtxSampling(on bool) {
	if g == nil {
		return
	}
	ctxSampling.Store(on)
}

// SetCtxSamplingEnabled is the host-facing package-level switch (the gate is
// process-global, like InstallCtxSink): the admin endpoint calls this directly.
func SetCtxSamplingEnabled(on bool) { ctxSampling.Store(on) }

// CtxSamplingEnabled reports the current gate state for the admin endpoint.
func CtxSamplingEnabled() bool { return ctxSampling.Load() }

// ctxSamplingOn reports whether the gate is open; callers pay one atomic read
// per request when it is closed.
func ctxSamplingOn() bool { return ctxSampling.Load() }

// sampleCtxComposition analyzes the pre-rewrite body when the gate is open.
// It is read-only: the returned body is the input unchanged, so this hook
// composes freely with the tool-alias / redact rewrites that follow.
func (g *Gateway) sampleCtxComposition(rec *reqRecord, body []byte) []byte {
	if !ctxSamplingOn() || rec == nil || len(body) == 0 {
		return body
	}
	if comp, err := AnalyzeCtxComposition(body); err == nil {
		rec.ctxComp = &comp
	}
	return body
}

// emitCtxComposition is called from emit(): it allocates the local estimates
// onto the upstream prompt total and hands the record to the sink.
func emitCtxComposition(rec *reqRecord, nowUnix int64) {
	if rec == nil || rec.ctxComp == nil {
		return
	}
	raw := ctxSinkMu.Load()
	sink, _ := raw.(func(CtxEmit))
	if sink == nil {
		return
	}
	comp := *rec.ctxComp
	// 比例分摊（task-62 契约）：上游报了 prompt 总量就把估算份额换算到上游口径，
	// 双口径都落库（est 是本地口径，allocated 是分摊后口径）。
	if rec.prompt > 0 {
		comp.Sections = AllocatePromptTokens(comp, rec.prompt)
	}
	sink(CtxEmit{
		ReqID:          rec.reqID,
		TS:             nowUnix,
		Comp:           comp,
		UpstreamPrompt: rec.prompt,
		UsageKnown:     rec.usageKnown,
		Reason:         "sampling_enabled",
	})
}
