package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/kw-decade/Texvoke/internal/ir"
)

// Anthropic 流式事件类型。规格十章要求事件顺序至少符合：
// message_start → 每个 block 的 content_block_start/delta/stop →
// message_delta → message_stop。
const (
	EventMessageStart      = "message_start"
	EventMessageDelta      = "message_delta"
	EventMessageStop       = "message_stop"
	EventContentBlockStart = "content_block_start"
	EventContentBlockDelta = "content_block_delta"
	EventContentBlockStop  = "content_block_stop"
	EventPing              = "ping"
	EventError             = "error"
)

// partialBlock 是一个尚未累积完的 content block。
type partialBlock struct {
	kind string

	// text 与 thinking 各自累积；工具参数走 jsonBuf。
	text      bytes.Buffer
	thinking  bytes.Buffer
	signature string

	// jsonBuf 累积 input_json_delta 的 partial_json 片段。与 Chat 的
	// arguments 同理：累积期间只是字节，不能当 JSON 校验。
	jsonBuf bytes.Buffer

	toolID   string
	toolName string

	// raw 保留 content_block_start 里那个 block 的原始形态，供 thinking 之类
	// 我们不打算解释的类型原样搬运。
	raw json.RawMessage

	stopped bool
}

// AnthropicStreamAccumulator 把 Anthropic 的流式事件累积成完整响应。
type AnthropicStreamAccumulator struct {
	opts DecodeOptions

	id    string
	model string

	blocks map[int]*partialBlock
	order  []int

	stopReason   StopReason
	stopSequence string
	usage        *AnthropicUsage

	started   bool
	stopped   bool
	totalSize int
}

// NewAnthropicStreamAccumulator 创建一个累积器。
func NewAnthropicStreamAccumulator(opts DecodeOptions) *AnthropicStreamAccumulator {
	return &AnthropicStreamAccumulator{
		opts:   opts,
		blocks: make(map[int]*partialBlock, 2),
	}
}

// Add 喂入一个 SSE 事件。
//
// ping 与注释心跳跳过；未知事件类型同样跳过而不报错——Anthropic 会新增
// 事件类型，遇到没见过的就中断会让 Bridge 在上游升级后突然罢工。
func (a *AnthropicStreamAccumulator) Add(ev Event) error {
	if a.stopped {
		return fmt.Errorf("protocol: 流已收到 %s，不应再有事件", EventMessageStop)
	}
	if ev.IsComment() {
		return nil
	}
	if ev.Type == EventPing {
		return nil
	}

	if len(ev.Data) == 0 {
		return nil
	}
	a.totalSize += len(ev.Data)
	if int64(a.totalSize) > a.opts.maxBytes() {
		return fmt.Errorf("protocol: 流累积 %d 字节超过上限 %d", a.totalSize, a.opts.maxBytes())
	}

	// 事件类型以 data 里的 type 字段为准：event 行可能被中间的代理剥掉，
	// 而 data 里的 type 是 Anthropic 自己写进负载的。
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(ev.Data, &probe); err != nil {
		return fmt.Errorf("protocol: 流事件不是合法 JSON：%w", err)
	}
	kind := probe.Type
	if kind == "" {
		kind = ev.Type
	}

	switch kind {
	case EventMessageStart:
		return a.handleMessageStart(ev.Data)
	case EventContentBlockStart:
		return a.handleBlockStart(ev.Data)
	case EventContentBlockDelta:
		return a.handleBlockDelta(ev.Data)
	case EventContentBlockStop:
		return a.handleBlockStop(ev.Data)
	case EventMessageDelta:
		return a.handleMessageDelta(ev.Data)
	case EventMessageStop:
		a.stopped = true
		return nil
	case EventError:
		// 上游在流中途报错。这与「响应形状非法」是两回事，
		// 必须返回可分类的 *UpstreamError。
		var e struct {
			Error UpstreamError `json:"error"`
		}
		if err := json.Unmarshal(ev.Data, &e); err != nil {
			return &UpstreamError{Message: string(ev.Data)}
		}
		if e.Error.Message == "" {
			e.Error.Message = string(ev.Data)
		}
		return &e.Error
	default:
		return nil
	}
}

func (a *AnthropicStreamAccumulator) handleMessageStart(data []byte) error {
	var ev struct {
		Message struct {
			ID    string          `json:"id"`
			Model string          `json:"model"`
			Role  string          `json:"role"`
			Usage *AnthropicUsage `json:"usage"`
		} `json:"message"`
	}
	if err := json.Unmarshal(data, &ev); err != nil {
		return fmt.Errorf("protocol: message_start 格式非法：%w", err)
	}
	if a.started {
		return fmt.Errorf("protocol: 流中出现了两次 %s", EventMessageStart)
	}
	if ev.Message.Role != "" && Role(ev.Message.Role) != RoleAssistant {
		return fmt.Errorf("protocol: message_start 的角色为 %q，期望 assistant", ev.Message.Role)
	}
	a.started = true
	a.id = ev.Message.ID
	a.model = ev.Message.Model
	if ev.Message.Usage != nil {
		a.usage = ev.Message.Usage
	}
	return nil
}

func (a *AnthropicStreamAccumulator) handleBlockStart(data []byte) error {
	var ev struct {
		Index        *int `json:"index"`
		ContentBlock struct {
			Type      string          `json:"type"`
			ID        string          `json:"id"`
			Name      string          `json:"name"`
			Text      string          `json:"text"`
			Thinking  string          `json:"thinking"`
			Signature string          `json:"signature"`
			Input     json.RawMessage `json:"input"`
		} `json:"content_block"`
		Raw json.RawMessage `json:"-"`
	}
	if err := json.Unmarshal(data, &ev); err != nil {
		return fmt.Errorf("protocol: content_block_start 格式非法：%w", err)
	}
	if ev.Index == nil {
		return fmt.Errorf("protocol: content_block_start 缺少 index，后续增量将无法归位")
	}
	if _, exists := a.blocks[*ev.Index]; exists {
		return fmt.Errorf("protocol: index %d 的 content block 重复开始", *ev.Index)
	}

	b := &partialBlock{
		kind:      ev.ContentBlock.Type,
		toolID:    ev.ContentBlock.ID,
		toolName:  ev.ContentBlock.Name,
		signature: ev.ContentBlock.Signature,
	}
	// start 事件里可能已经带了初始内容，不能丢。
	b.text.WriteString(ev.ContentBlock.Text)
	b.thinking.WriteString(ev.ContentBlock.Thinking)

	if b.kind == "tool_use" && b.toolID == "" {
		return fmt.Errorf("protocol: index %d 的 tool_use block 缺少 id", *ev.Index)
	}

	// 保留原始 block 形态，供我们不解释的类型原样搬运。
	var wrapper struct {
		ContentBlock json.RawMessage `json:"content_block"`
	}
	if err := json.Unmarshal(data, &wrapper); err == nil {
		b.raw = wrapper.ContentBlock
	}

	a.blocks[*ev.Index] = b
	a.order = append(a.order, *ev.Index)
	return nil
}

func (a *AnthropicStreamAccumulator) handleBlockDelta(data []byte) error {
	var ev struct {
		Index *int `json:"index"`
		Delta struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			PartialJSON string `json:"partial_json"`
			Thinking    string `json:"thinking"`
			Signature   string `json:"signature"`
		} `json:"delta"`
	}
	if err := json.Unmarshal(data, &ev); err != nil {
		return fmt.Errorf("protocol: content_block_delta 格式非法：%w", err)
	}
	if ev.Index == nil {
		return fmt.Errorf("protocol: content_block_delta 缺少 index，无法归位到具体 block")
	}
	b, ok := a.blocks[*ev.Index]
	if !ok {
		return fmt.Errorf("protocol: index %d 的增量没有对应的 content_block_start", *ev.Index)
	}
	if b.stopped {
		return fmt.Errorf("protocol: index %d 的 block 已结束，不应再有增量", *ev.Index)
	}

	switch ev.Delta.Type {
	case "text_delta":
		b.text.WriteString(ev.Delta.Text)
	case "input_json_delta":
		// 工具参数的片段。与 Chat 的 arguments 同理：累积期间只是字节。
		b.jsonBuf.WriteString(ev.Delta.PartialJSON)
	case "thinking_delta":
		b.thinking.WriteString(ev.Delta.Thinking)
	case "signature_delta":
		b.signature += ev.Delta.Signature
	default:
		// 未知的 delta 类型跳过，理由同未知事件。
	}
	return nil
}

func (a *AnthropicStreamAccumulator) handleBlockStop(data []byte) error {
	var ev struct {
		Index *int `json:"index"`
	}
	if err := json.Unmarshal(data, &ev); err != nil {
		return fmt.Errorf("protocol: content_block_stop 格式非法：%w", err)
	}
	if ev.Index == nil {
		return fmt.Errorf("protocol: content_block_stop 缺少 index")
	}
	b, ok := a.blocks[*ev.Index]
	if !ok {
		return fmt.Errorf("protocol: index %d 的结束事件没有对应的开始事件", *ev.Index)
	}
	b.stopped = true
	return nil
}

func (a *AnthropicStreamAccumulator) handleMessageDelta(data []byte) error {
	var ev struct {
		Delta struct {
			StopReason   *string `json:"stop_reason"`
			StopSequence *string `json:"stop_sequence"`
		} `json:"delta"`
		Usage *AnthropicUsage `json:"usage"`
	}
	if err := json.Unmarshal(data, &ev); err != nil {
		return fmt.Errorf("protocol: message_delta 格式非法：%w", err)
	}
	if ev.Delta.StopReason != nil && *ev.Delta.StopReason != "" {
		a.stopReason = StopReason(*ev.Delta.StopReason)
	}
	if ev.Delta.StopSequence != nil {
		a.stopSequence = *ev.Delta.StopSequence
	}
	if ev.Usage != nil {
		// message_delta 的 usage 只带 output_tokens，input_tokens 在
		// message_start 里给过。合并而不是覆盖，否则输入侧计数会被清零。
		if a.usage == nil {
			a.usage = ev.Usage
		} else {
			if ev.Usage.OutputTokens > 0 {
				a.usage.OutputTokens = ev.Usage.OutputTokens
			}
			if ev.Usage.InputTokens > 0 {
				a.usage.InputTokens = ev.Usage.InputTokens
			}
			if ev.Usage.CacheReadInputTokens > 0 {
				a.usage.CacheReadInputTokens = ev.Usage.CacheReadInputTokens
			}
			if ev.Usage.CacheCreationInputTokens > 0 {
				a.usage.CacheCreationInputTokens = ev.Usage.CacheCreationInputTokens
			}
		}
	}
	return nil
}

// Done 报告是否已收到 message_stop。
func (a *AnthropicStreamAccumulator) Done() bool { return a.stopped }

// Result 把累积结果组装成完整响应。
func (a *AnthropicStreamAccumulator) Result() (*MessagesResponse, error) {
	if !a.started {
		return nil, fmt.Errorf("protocol: 流中没有 %s", EventMessageStart)
	}
	if !a.stopped && a.stopReason == "" {
		return nil, fmt.Errorf("protocol: 流未正常结束，既无 %s 也无 stop_reason", EventMessageStop)
	}

	resp := &MessagesResponse{
		ID:           a.id,
		Model:        a.model,
		StopReason:   a.stopReason,
		StopSequence: a.stopSequence,
		Usage:        a.usage,
	}

	idx := make([]int, len(a.order))
	copy(idx, a.order)
	sort.Ints(idx)

	now := a.opts.now()
	var contentBlocks []json.RawMessage

	for _, i := range idx {
		b := a.blocks[i]
		switch b.kind {
		case "tool_use":
			args, err := decodeAccumulatedArguments(b.jsonBuf.Bytes())
			if err != nil {
				return nil, fmt.Errorf("protocol: index %d（id=%s）的%w", i, b.toolID, err)
			}
			p := ir.ToolCallProposal{
				SessionID: a.opts.SessionID,
				RequestID: a.opts.RequestID,
				CallID:    b.toolID,
				Tool: ir.ToolID{
					Namespace: ir.NamespaceClient,
					Name:      b.toolName,
					Version:   ir.VersionDeclared,
				},
				Arguments:          args,
				Source:             ir.SourceNative,
				RawCandidateDigest: ir.DigestRawCandidate(b.jsonBuf.Bytes()),
				CreatedAt:          now,
			}
			if err := p.Validate(); err != nil {
				return nil, fmt.Errorf("protocol: index %d 的调用非法：%w", i, err)
			}
			resp.ToolCalls = append(resp.ToolCalls, p)

		case "text":
			block, err := json.Marshal(map[string]string{"type": "text", "text": b.text.String()})
			if err != nil {
				return nil, fmt.Errorf("protocol: 编码 index %d 的 text block 失败：%w", i, err)
			}
			contentBlocks = append(contentBlocks, block)

		case "thinking":
			// 隐藏推理内容原样重组，不解释也不改写。签名必须一起带上——
			// 丢了签名的 thinking 会被 Anthropic 拒绝。
			m := map[string]string{"type": "thinking", "thinking": b.thinking.String()}
			if b.signature != "" {
				m["signature"] = b.signature
			}
			block, err := json.Marshal(m)
			if err != nil {
				return nil, fmt.Errorf("protocol: 编码 index %d 的 thinking block 失败：%w", i, err)
			}
			contentBlocks = append(contentBlocks, block)

		default:
			// 其余类型（redacted_thinking、server_tool_use 等）用 start 事件里
			// 的原始形态搬运。拿不到原始形态就报错，不猜测。
			if len(b.raw) == 0 {
				return nil, fmt.Errorf("protocol: index %d 的 %q block 无法还原", i, b.kind)
			}
			contentBlocks = append(contentBlocks, b.raw)
		}
	}

	if len(contentBlocks) > 0 {
		content, err := json.Marshal(contentBlocks)
		if err != nil {
			return nil, fmt.Errorf("protocol: 编码 content 失败：%w", err)
		}
		resp.Content = content
	}

	if err := resp.Validate(); err != nil {
		return nil, err
	}
	return resp, nil
}

// EncodeAnthropicStream 把一个完整响应渲染成 Anthropic 的事件序列。
//
// 事件顺序严格遵守规格十章：message_start → 每个 block 的
// content_block_start/delta/stop → message_delta → message_stop。
// 顺序错了客户端 SDK 会直接报协议错误。
func EncodeAnthropicStream(enc *SSEEncoder, r MessagesResponse) error {
	if err := r.Validate(); err != nil {
		return err
	}
	if err := EncodeAnthropicMessageStart(enc, r, startUsageOf(r)); err != nil {
		return err
	}
	return encodeAnthropicBodyAndTail(enc, r, 0)
}

// EncodeAnthropicMessageStart 渲染真流式的起始事件（message_start）。
// 独立出来是因为真流式下它在第一个文本增量之前发出，与收尾相隔整个
// 生成期。startUsage 允许调用方传 nil（取零值）。
func EncodeAnthropicMessageStart(enc *SSEEncoder, r MessagesResponse, startUsage *AnthropicUsage) error {
	if startUsage == nil {
		startUsage = &AnthropicUsage{}
	}
	b, err := json.Marshal(map[string]any{
		"type": EventMessageStart,
		"message": map[string]any{
			"id":            r.ID,
			"type":          "message",
			"role":          "assistant",
			"model":         r.Model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         startUsage,
		},
	})
	if err != nil {
		return fmt.Errorf("protocol: 编码 %s 事件失败：%w", EventMessageStart, err)
	}
	return enc.Write(Event{Type: EventMessageStart, Data: b})
}

// EncodeAnthropicStreamTail 渲染真流式的收尾段：content_block_stop →
// tool_use 块 → message_delta → message_stop。skipText 为真时跳过文本
// block 重发（增量已实时到达），但 **content_block_stop 必须发**——
// start/stop 配对是 Anthropic 的硬约束，漏掉它客户端 SDK 直接报错。
func EncodeAnthropicStreamTail(enc *SSEEncoder, r MessagesResponse, skipText bool) error {
	if err := r.Validate(); err != nil {
		return err
	}
	if !skipText {
		return EncodeAnthropicStream(enc, r)
	}
	// 文本增量已按 index 0 发出，收尾从它的 stop 开始。
	return encodeAnthropicTextStopAndTail(enc, r)
}

// encodeAnthropicTextStopAndTail 发出文本 block 的 stop 与后续全部内容。
func encodeAnthropicTextStopAndTail(enc *SSEEncoder, r MessagesResponse) error {
	write := func(evType string, v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Errorf("protocol: 编码 %s 事件失败：%w", evType, err)
		}
		return enc.Write(Event{Type: evType, Data: b})
	}
	hasText := len(r.Content) > 0 && string(r.Content) != "null"
	index := 0
	if hasText {
		if err := write(EventContentBlockStop, map[string]any{
			"type": EventContentBlockStop, "index": 0,
		}); err != nil {
			return err
		}
		index = 1
	}
	return encodeAnthropicToolCallsAndEnd(enc, r, write, index)
}

// startUsageOf 从响应里提取 message_start 用的 usage。
func startUsageOf(r MessagesResponse) *AnthropicUsage {
	if r.Usage == nil {
		return &AnthropicUsage{}
	}
	return &AnthropicUsage{
		InputTokens:              r.Usage.InputTokens,
		CacheReadInputTokens:     r.Usage.CacheReadInputTokens,
		CacheCreationInputTokens: r.Usage.CacheCreationInputTokens,
	}
}

// encodeAnthropicBodyAndTail 渲染 message_start 之后的全部内容：
// 文本 block → tool_use 块 → message_delta → message_stop。startIndex 是
// 文本 block 的位置（完整渲染恒为 0）。
func encodeAnthropicBodyAndTail(enc *SSEEncoder, r MessagesResponse, startIndex int) error {
	write := func(evType string, v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Errorf("protocol: 编码 %s 事件失败：%w", evType, err)
		}
		return enc.Write(Event{Type: evType, Data: b})
	}
	index := startIndex

	// 文本 block 在前，与非流式渲染保持一致的顺序。
	if len(r.Content) > 0 && string(r.Content) != "null" {
		var blocks []json.RawMessage
		if err := json.Unmarshal(r.Content, &blocks); err != nil {
			return fmt.Errorf("protocol: 流式渲染要求 content 是 block 数组：%w", err)
		}
		for _, raw := range blocks {
			var probe struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if err := json.Unmarshal(raw, &probe); err != nil {
				return fmt.Errorf("protocol: content block 格式非法：%w", err)
			}

			if err := write(EventContentBlockStart, map[string]any{
				"type": EventContentBlockStart, "index": index,
				"content_block": emptyShellOf(raw, probe.Type),
			}); err != nil {
				return err
			}
			if probe.Type == "text" && probe.Text != "" {
				if err := write(EventContentBlockDelta, map[string]any{
					"type": EventContentBlockDelta, "index": index,
					"delta": map[string]any{"type": "text_delta", "text": probe.Text},
				}); err != nil {
					return err
				}
			}
			if err := write(EventContentBlockStop, map[string]any{
				"type": EventContentBlockStop, "index": index,
			}); err != nil {
				return err
			}
			index++
		}
	}

	return encodeAnthropicToolCallsAndEnd(enc, r, write, index)
}

// encodeAnthropicToolCallsAndEnd 渲染 tool_use 块与结束事件。
//
// 两条路径共用（完整渲染与真流式收尾），index 由调用方给出——真流式下
// 文本 block 已经占了 0 号位。
func encodeAnthropicToolCallsAndEnd(enc *SSEEncoder, r MessagesResponse,
	write func(string, any) error, index int) error {

	if err := rejectFreeform("anthropic", r.ToolCalls); err != nil {
		return err
	}
	for _, c := range r.ToolCalls {
		if err := write(EventContentBlockStart, map[string]any{
			"type": EventContentBlockStart, "index": index,
			"content_block": map[string]any{
				"type": "tool_use", "id": c.CallID, "name": c.Tool.Name,
				"input": map[string]any{},
			},
		}); err != nil {
			return err
		}
		// 参数整体作为一个片段发出。切成多片只是为了模仿上游的节奏，
		// 对正确性没有帮助，反而多出可能出错的边界。
		if err := write(EventContentBlockDelta, map[string]any{
			"type": EventContentBlockDelta, "index": index,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": string(c.Arguments)},
		}); err != nil {
			return err
		}
		if err := write(EventContentBlockStop, map[string]any{
			"type": EventContentBlockStop, "index": index,
		}); err != nil {
			return err
		}
		index++
	}

	delta := map[string]any{"stop_reason": string(r.StopReason), "stop_sequence": nil}
	if r.StopSequence != "" {
		delta["stop_sequence"] = r.StopSequence
	}
	msgDelta := map[string]any{"type": EventMessageDelta, "delta": delta}
	if r.Usage != nil {
		msgDelta["usage"] = map[string]any{"output_tokens": r.Usage.OutputTokens}
	}
	if err := write(EventMessageDelta, msgDelta); err != nil {
		return err
	}

	return write(EventMessageStop, map[string]any{"type": EventMessageStop})
}

// emptyShellOf 返回一个 block 的「空壳」，供 content_block_start 使用。
//
// Anthropic 的 start 事件带的是空内容的壳，真正的内容走后续 delta。
// 对我们不解释的类型（thinking、redacted_thinking 等）原样送出：
// 它们没有增量形态，拆开反而无法还原。
func emptyShellOf(raw json.RawMessage, kind string) any {
	if kind == "text" {
		return map[string]any{"type": "text", "text": ""}
	}
	return raw
}
