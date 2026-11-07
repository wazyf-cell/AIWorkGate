package gateway

// D-56 单连接多协议分槽（plans/2026-11-05 §D-56）。
//
// 一条自定义连接此前只有一个 (Protocol, BaseURL)：入站的 messages/responses
// 面先被翻译成 chat，再按同一个协议打出去。分槽允许同一条连接为三种入站面
// 声明各自的出站协议与地址，例如 messages 槽指向 Anthropic 兼容端点、chat 槽
// 指向 OpenAI 兼容端点，客户端两种面都打到同一个模型名上。
//
// 取舍（定稿并写进实现）：
//   - 槽是"按面覆盖"而非"每面必须齐全"：某一面没有槽（协议或地址任一缺失）
//     回落到旧单协议字段 Protocol/BaseURL，即该连接的默认槽。这样旧字段永远
//     向后兼容，新增一个槽不需要把另外两面抄写一遍；也不会因为漏配一面而把
//     请求打死——回落后的行为就是分槽功能上线前的行为。
//   - 只有配置本身非法才报错：槽键不是三个入站面之一、槽协议不是
//     openai/anthropic/gemini、或槽与旧字段都没有地址。这类错误转发前直接
//     失败（502 + 明确文案），不静默改写。
//   - 槽只影响出站转发（URL、协议转换、鉴权头）。连接发现（GET /models）与
//     显式连通性测试仍走默认协议/地址：模型列表语义上属于连接而非某个面。
//   - 宿主侧的存储列、管理 API 与 UI 回填不属 gateway；本文件只消费 provider
//     （Options.CustomModels）给出的槽值，测试直接构造 CustomModel。

import (
	"fmt"
	"strings"

	"aiworkgate/internal/model"
)

// Custom protocol slot faces (D-56). The keys of CustomModel.Protocols /
// CustomModel.ProtocolPaths. /v1/completions is translated through the chat
// face and shares its slot.
const (
	CustomFaceChat      = "chat"
	CustomFaceResponses = "responses"
	CustomFaceMessages  = "messages"
)

// customSlot is the resolved outbound protocol and base URL of one inbound
// face. Both fields are already normalized (NormalizeCustomProtocolName).
type customSlot struct {
	Protocol string
	BaseURL  string
}

// customFaceForPath maps an inbound gateway path onto its D-56 slot face. The
// legacy /v1/completions envelope is translated to a chat body upstream, so it
// rides the chat slot.
func customFaceForPath(path string) string {
	switch path {
	case "/v1/messages":
		return CustomFaceMessages
	case "/v1/responses":
		return CustomFaceResponses
	default:
		return CustomFaceChat
	}
}

// customSlotFaces is the accepted slot key set; a slot map carrying any other
// key is rejected as invalid configuration.
var customSlotFaces = map[string]bool{
	CustomFaceChat:      true,
	CustomFaceResponses: true,
	CustomFaceMessages:  true,
}

// customSlotFor resolves one inbound face onto its outbound slot. Resolution
// order per field: the face's slot value, else the connection's legacy default
// (Protocol / BaseURL). See the file comment for the fallback trade-off.
func customSlotFor(cm CustomModel, face string) (customSlot, error) {
	switch strings.ToLower(strings.TrimSpace(face)) {
	case CustomFaceChat, CustomFaceResponses, CustomFaceMessages:
	default:
		return customSlot{}, fmt.Errorf("自定义模型 %s：未知入站面 %q", cm.Name, face)
	}
	for _, slots := range []map[string]string{cm.Protocols, cm.ProtocolPaths} {
		for key := range slots {
			if !customSlotFaces[strings.ToLower(strings.TrimSpace(key))] {
				return customSlot{}, fmt.Errorf("自定义模型 %s：协议槽键 %q 无效（只支持 chat/responses/messages）", cm.Name, key)
			}
		}
	}
	protocol := strings.TrimSpace(cm.Protocols[face])
	baseURL := strings.TrimSpace(cm.ProtocolPaths[face])
	if protocol == "" {
		protocol = strings.TrimSpace(cm.Protocol)
	}
	if baseURL == "" {
		baseURL = strings.TrimSpace(cm.BaseURL)
	}
	if _, ok := model.NormalizeProtocol(protocol); !ok {
		return customSlot{}, fmt.Errorf("自定义模型 %s：%s 槽协议 %q 无效（openai/anthropic/gemini）", cm.Name, face, protocol)
	}
	if baseURL == "" {
		return customSlot{}, fmt.Errorf("自定义模型 %s：%s 槽缺少 API 地址", cm.Name, face)
	}
	return customSlot{
		Protocol: NormalizeCustomProtocolName(protocol),
		BaseURL:  baseURL,
	}, nil
}
