package protocol

import (
	"encoding/json"
	"fmt"

	"github.com/kw-decade/Texvoke/internal/ir"
)

// StopReason 是 Anthropic Messages 的终止原因。
//
// 这里不复用 FinishReason，是因为两套枚举并非一一对应：Anthropic 的
// refusal 与 pause_turn 在 Chat Completions 里没有等价物，硬映射过去
// 会在往返中丢失信息。协议特有的东西就留在协议自己的类型里。
type StopReason string

const (
	StopEndTurn      StopReason = "end_turn"      // 模型自然说完
	StopMaxTokens    StopReason = "max_tokens"    // 达到输出上限被截断
	StopStopSequence StopReason = "stop_sequence" // 命中调用方指定的停止序列
	StopToolUse      StopReason = "tool_use"      // 模型请求调用工具
	StopRefusal      StopReason = "refusal"       // 模型拒绝生成
	StopPauseTurn    StopReason = "pause_turn"    // 长任务暂停，可续期
)

// Valid 报告 s 是否是已知的终止原因。空值永远无效。
func (s StopReason) Valid() bool {
	switch s {
	case StopEndTurn, StopMaxTokens, StopStopSequence, StopToolUse, StopRefusal, StopPauseTurn:
		return true
	default:
		return false
	}
}

// AnthropicUsage 是 Anthropic 的 token 用量统计。
//
// 字段名与 Chat Completions 不同（input/output 而非 prompt/completion），
// 且多出两个缓存相关的计数，因此单独建模而不是复用 Usage。
type AnthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
}

// MessagesRequest 是归一化后的 Anthropic Messages 请求。
type MessagesRequest struct {
	Model string `json:"model"`

	// MaxTokens 在 Anthropic 是必填字段，缺失会被上游直接拒绝，
	// 所以解码时就要挡住，而不是等发出去才失败。
	MaxTokens int `json:"max_tokens"`

	// Messages 里可能含一条 system 消息（由顶层 system 字段转换而来），
	// 且只能有一条——编码回 Anthropic 时它要还原成顶层字段。
	Messages []Message `json:"messages"`

	Tools      []ir.ToolDeclaration `json:"tools,omitempty"`
	ToolChoice ToolChoice           `json:"tool_choice"`

	// ParallelToolCalls 对应 tool_choice.disable_parallel_tool_use 的**反面**。
	// 统一成与 Chat Completions 相同的正向语义，免得下游每次都要想一遍
	// 这个布尔是不是反的。nil 表示客户端没有表态。
	ParallelToolCalls *bool `json:"parallel_tool_calls,omitempty"`

	Stream bool `json:"stream,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

// MessagesResponse 是归一化后的 Anthropic Messages 响应。
type MessagesResponse struct {
	ID    string `json:"id"`
	Model string `json:"model"`

	// Content 是除工具调用之外的内容 block 数组，保留原始形态
	// （text、image、thinking 等一律不动）。
	Content json.RawMessage `json:"content,omitempty"`

	ToolCalls []ir.ToolCallProposal `json:"tool_calls,omitempty"`

	StopReason StopReason `json:"stop_reason"`
	// StopSequence 只在 StopReason 为 stop_sequence 时有值。
	StopSequence string `json:"stop_sequence,omitempty"`

	Usage *AnthropicUsage `json:"usage,omitempty"`
}

// Validate 检查响应各字段是否自洽。
//
// 与 Chat Completions 同理：stop_reason 与工具调用必须双向一致，
// 客户端 SDK 靠 stop_reason == "tool_use" 决定要不要执行工具。
func (r MessagesResponse) Validate() error {
	if r.ID == "" {
		return fmt.Errorf("protocol: 响应缺少 id")
	}
	if r.Model == "" {
		return fmt.Errorf("protocol: 响应缺少 model")
	}
	if !r.StopReason.Valid() {
		return fmt.Errorf("protocol: 响应的 stop_reason 非法：%q", r.StopReason)
	}
	if len(r.ToolCalls) > 0 && r.StopReason != StopToolUse {
		return fmt.Errorf("protocol: 响应带 %d 个工具调用，stop_reason 必须是 %q，实际为 %q",
			len(r.ToolCalls), StopToolUse, r.StopReason)
	}
	if r.StopReason == StopToolUse && len(r.ToolCalls) == 0 {
		return fmt.Errorf("protocol: stop_reason 为 %q 却没有任何工具调用", StopToolUse)
	}
	if r.StopSequence != "" && r.StopReason != StopStopSequence {
		return fmt.Errorf("protocol: 带 stop_sequence 时 stop_reason 必须是 %q，实际为 %q",
			StopStopSequence, r.StopReason)
	}

	seen := make(map[string]bool, len(r.ToolCalls))
	for i, tc := range r.ToolCalls {
		if err := tc.Validate(); err != nil {
			return fmt.Errorf("protocol: 第 %d 个工具调用非法：%w", i, err)
		}
		if seen[tc.CallID] {
			return fmt.Errorf("protocol: 工具调用 id %q 重复", tc.CallID)
		}
		seen[tc.CallID] = true
	}
	return nil
}
