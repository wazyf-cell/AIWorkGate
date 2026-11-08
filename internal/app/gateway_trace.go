// 问题6A 地基（task-48）：webui 的 trace 回调实现。
//
// 信封 {"events":[…],"last_seq":N}：events 是本次返回的增量/回放，
// last_seq 是网关当前最后事件 seq——前端把游标推进到它，下一次 poll 带
// after=last_seq。网关未就绪（处理器未挂）时返回空页，不报错。
package app

import (
	"context"
	"time"

	"aiworkgate/internal/gateway"
)

// gatewayTrace 消费者接口：当前 /v1 处理器是 *gateway.Gateway 时成立。
type gatewayTraceProvider interface {
	TraceRecent(limit int) []gateway.TraceEvent
	TraceLastSeq() int64
	TracePoll(ctx context.Context, after int64, wait bool, timeout time.Duration) []gateway.TraceEvent
}

// traceProviderOf 取当前网关的 trace 提供者；nil = 未就绪（空页语义）。
func (a *App) traceProviderOf() gatewayTraceProvider {
	h := a.currentGateway()
	if h == nil {
		return nil
	}
	provider, ok := h.(gatewayTraceProvider)
	if !ok {
		return nil
	}
	return provider
}

func traceEnvelope(events []gateway.TraceEvent, lastSeq int64) map[string]any {
	if events == nil {
		events = []gateway.TraceEvent{}
	}
	return map[string]any{"events": events, "last_seq": lastSeq}
}

// gatewayTraceRecent implements GET /api/gateway/trace/recent?limit=N.
func (a *App) gatewayTraceRecent(limit int) (any, error) {
	provider := a.traceProviderOf()
	if provider == nil {
		return traceEnvelope([]gateway.TraceEvent{}, 0), nil
	}
	return traceEnvelope(provider.TraceRecent(limit), provider.TraceLastSeq()), nil
}

// gatewayTracePoll implements GET /api/gateway/trace?after=<seq>&wait=1.
// webui 回调不带请求 context：挂起上限（25s）在网关内部封顶，客户端断连最多
// 滞留一个挂起窗口，可接受。
func (a *App) gatewayTracePoll(after int64, wait bool) (any, error) {
	provider := a.traceProviderOf()
	if provider == nil {
		return traceEnvelope([]gateway.TraceEvent{}, 0), nil
	}
	var timeout time.Duration
	if !wait {
		timeout = -1 // 立即返回增量
	}
	events := provider.TracePoll(context.Background(), after, wait, timeout)
	return traceEnvelope(events, provider.TraceLastSeq()), nil
}
