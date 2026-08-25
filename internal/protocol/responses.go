package protocol

import (
	"encoding/json"
	"fmt"

	"github.com/kw-decade/Texvoke/internal/ir"
)

// ResponseStatus 是 Responses API 的响应状态。
//
// 与 Chat Completions 的 finish_reason 不同：那是「这一次生成为什么停下」，
// 这是「这个 response 对象当前处于什么状态」。一个 in_progress 的响应
// 之后还会变，把两者混成一个枚举会丢掉这层含义。
type ResponseStatus string

const (
	ResponseCompleted  ResponseStatus = "completed"
	ResponseInProgress ResponseStatus = "in_progress"
	ResponseIncomplete ResponseStatus = "incomplete"
	ResponseFailed     ResponseStatus = "failed"
	ResponseCancelled  ResponseStatus = "cancelled"
	ResponseQueued     ResponseStatus = "queued"
)

// Valid 报告 s 是否是已知状态。空值永远无效。
func (s ResponseStatus) Valid() bool {
	switch s {
	case ResponseCompleted, ResponseInProgress, ResponseIncomplete,
		ResponseFailed, ResponseCancelled, ResponseQueued:
		return true
	default:
		return false
	}
}

// Terminal 报告该状态是否已经定型，不会再变。
func (s ResponseStatus) Terminal() bool {
	switch s {
	case ResponseCompleted, ResponseIncomplete, ResponseFailed, ResponseCancelled:
		return true
	default:
		return false
	}
}

// ResponsesUsage 是 Responses API 的 token 用量。字段名与 Chat Completions
// 不同（input/output 而非 prompt/completion），单独建模避免映射时出错。
type ResponsesUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// ResponsesRequest 是归一化后的 OpenAI Responses 请求。
//
// 注意这里没有 Instructions 字段：顶层 instructions 在解码时转成序列首位的
// system 消息，编码时统一写回 input 里的 message item。OpenAI 文档说明
// instructions 等价于在 input 开头放一条 system 消息，因此这样处理语义等价，
// 而且换来了往返幂等——保留两个位置就必须记录「它原本在哪」，那是一个
// 除了让往返测试通过之外没有任何用处的字段。
type ResponsesRequest struct {
	Model string `json:"model"`

	// Input 是归一化后的消息序列，含由 instructions 转换而来的 system 消息。
	Input []Message `json:"input"`

	Tools      []ir.ToolDeclaration `json:"tools,omitempty"`
	ToolChoice ToolChoice           `json:"tool_choice"`

	ParallelToolCalls *bool `json:"parallel_tool_calls,omitempty"`

	// MaxOutputTokens 为 0 表示客户端没有指定（Responses 里它是可选的，
	// 与 Anthropic 的必填 max_tokens 不同）。
	MaxOutputTokens int `json:"max_output_tokens,omitempty"`

	// PreviousResponseID 把这次请求接到上一次响应之后，由上游负责取回
	// 之前的上下文。它是服务端状态的句柄，必须原样透传——改写或丢弃会让
	// 上游看到一段与客户端预期不同的历史。
	PreviousResponseID string `json:"previous_response_id,omitempty"`

	// Store 控制上游是否留存这次响应。nil 表示客户端没有表态。
	Store *bool `json:"store,omitempty"`

	Stream bool `json:"stream,omitempty"`

	// SkippedItemTypes 记录 input 里被跳过的 item 类型（去重后）。
	//
	// 跳过不认识的扩展 item 是为了不把整轮对话挡在门外，但跳过本身必须
	// 留下痕迹——静默丢弃客户端发来的数据，正是以后最难查的那类问题。
	SkippedItemTypes []string `json:"skipped_item_types,omitempty"`

	// SkippedTools 记录被跳过的工具名（目前只有 namespace 类型会落进来）。
	//
	// 与 SkippedItemTypes 分开：一个说的是「有段结构我不认识」，另一个说的是
	// 「有个工具你声明了但模型用不上」。后者会直接影响模型能做什么，接入方
	// 必须能看见——降级要么可见，要么就是个查不出来的 bug。
	SkippedTools []string `json:"skipped_tools,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

// ResponsesResponse 是归一化后的 Responses 响应。
type ResponsesResponse struct {
	ID        string         `json:"id"`
	Model     string         `json:"model"`
	CreatedAt int64          `json:"created_at"`
	Status    ResponseStatus `json:"status"`

	// Content 是 message item 里的 content block 数组，保留原始形态。
	Content json.RawMessage `json:"content,omitempty"`

	// MessageItemID 是承载 Content 的那个 message item 的 ID。
	// 与工具调用的 ProtocolItemID 同理：流式事件与最终 output 必须用同一个
	// ID，客户端才能把增量拼回同一个 item。
	MessageItemID string `json:"message_item_id,omitempty"`

	ToolCalls []ir.ToolCallProposal `json:"tool_calls,omitempty"`

	// IncompleteReason 只在 Status 为 incomplete 时有值（如 max_output_tokens）。
	IncompleteReason string `json:"incomplete_reason,omitempty"`

	Usage *ResponsesUsage `json:"usage,omitempty"`
}

// Validate 检查响应各字段是否自洽。
func (r ResponsesResponse) Validate() error {
	if r.ID == "" {
		return fmt.Errorf("protocol: 响应缺少 id")
	}
	if r.Model == "" {
		return fmt.Errorf("protocol: 响应缺少 model")
	}
	if !r.Status.Valid() {
		return fmt.Errorf("protocol: 响应的 status 非法：%q", r.Status)
	}
	if r.IncompleteReason != "" && r.Status != ResponseIncomplete {
		return fmt.Errorf("protocol: 带 incomplete_reason 时 status 必须是 %q，实际为 %q",
			ResponseIncomplete, r.Status)
	}

	seenCall := make(map[string]bool, len(r.ToolCalls))
	seenItem := make(map[string]bool, len(r.ToolCalls))
	for i, tc := range r.ToolCalls {
		if err := tc.Validate(); err != nil {
			return fmt.Errorf("protocol: 第 %d 个工具调用非法：%w", i, err)
		}
		if seenCall[tc.CallID] {
			return fmt.Errorf("protocol: 工具调用的 call_id %q 重复", tc.CallID)
		}
		seenCall[tc.CallID] = true

		// item id 同样不能重复：客户端按 item_id 把流式增量拼回对应的 item，
		// 两个 item 共用一个 id 会让增量拼到错误的调用上。
		if tc.ProtocolItemID != "" {
			if seenItem[tc.ProtocolItemID] {
				return fmt.Errorf("protocol: 工具调用的 item id %q 重复", tc.ProtocolItemID)
			}
			seenItem[tc.ProtocolItemID] = true
		}
	}
	return nil
}
