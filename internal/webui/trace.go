// 问题6A 地基（task-48）：GET /api/gateway/trace（长轮询）与
// GET /api/gateway/trace/recent（首屏回放）。
//
// 两个端点都是现有 session 鉴权之下的管理面只读端点；宿主回调实现挂起与
// 增量语义（internal/app），本文件只做查询参数解析与信封。
//
// 响应形状（task-49 前端契约）：
//	{"events":[TraceEvent…],"last_seq":<int64>}
//	last_seq 是网关当前最后发出的事件 seq——前端把游标推进到它，
//	下一次 poll 带 after=last_seq；events 为空 = 挂起超时无新事件。
package webui

import (
	"net/http"
	"strconv"
	"strings"
)

func (s *server) handleGatewayTrace(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet) {
		return
	}
	if s.opts.GatewayTracePoll == nil || s.opts.GatewayTraceRecent == nil {
		s.unavailable(w)
		return
	}
	query := r.URL.Query()
	if strings.HasSuffix(r.URL.Path, "/recent") {
		limit, err := strconv.Atoi(query.Get("limit"))
		if err != nil || limit < 0 {
			limit = 0 // 0 = 全部保留历史
		}
		s.writeGatewayTrace(w, func() (any, error) { return s.opts.GatewayTraceRecent(limit) })
		return
	}
	after, _ := strconv.ParseInt(query.Get("after"), 10, 64)
	if after < 0 {
		after = 0
	}
	wait := query.Get("wait") == "1" || strings.EqualFold(query.Get("wait"), "true")
	s.writeGatewayTrace(w, func() (any, error) { return s.opts.GatewayTracePoll(after, wait) })
}

// writeGatewayTrace 统一信封：回调出错 500；成功写宿主组装好的
// {"events":[…],"last_seq":N}。
func (s *server) writeGatewayTrace(w http.ResponseWriter, call func() (any, error)) {
	payload, err := call()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "gateway trace: "+err.Error(), "internal_error")
		return
	}
	if payload == nil {
		payload = map[string]any{"events": []any{}, "last_seq": 0}
	}
	writeJSON(w, http.StatusOK, payload)
}
