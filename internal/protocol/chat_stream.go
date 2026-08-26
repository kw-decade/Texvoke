package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/kw-decade/Texvoke/internal/ir"
)

// StreamDoneMarker 是 Chat Completions 流的终止标记。
//
// 它不是 JSON，而是字面量 [DONE]。把它当 JSON 解析会失败，
// 所以必须在解析之前先认出来。
const StreamDoneMarker = "[DONE]"

// partialToolCall 是一次尚未累积完的工具调用。
//
// 参数以任意边界切碎到达——上游可能把 {"city":"SF"} 拆成 {"ci / ty":" / SF"}——
// 所以累积期间它只是一串字节，不能当 JSON 校验。等流结束后再整体解析，
// 解析失败就是显式的 parse_error。
type partialToolCall struct {
	id   string
	name string
	args bytes.Buffer
}

// ChatStreamAccumulator 把 chat.completion.chunk 序列累积成一个完整响应。
//
// 规格十章要求「流式工具参数 delta 要能被客户端正确累积」，这里做的是它的
// 镜像：读上游的增量并还原。累积按 tool_calls 的 index 分组，因为并行调用的
// 增量是交错到达的，靠出现顺序猜会把两次调用的参数拼到一起。
type ChatStreamAccumulator struct {
	opts DecodeOptions

	id      string
	model   string
	created int64

	content bytes.Buffer
	refusal bytes.Buffer

	calls map[int]*partialToolCall
	// order 记录 index 首次出现的顺序。map 遍历顺序随机，直接遍历会让
	// 同一份流每次还原出不同的调用顺序。
	order []int

	finishReason FinishReason
	usage        *Usage

	done      bool
	sawChunk  bool
	totalSize int
}

// NewChatStreamAccumulator 创建一个累积器。
func NewChatStreamAccumulator(opts DecodeOptions) *ChatStreamAccumulator {
	return &ChatStreamAccumulator{
		opts:  opts,
		calls: make(map[int]*partialToolCall, 2),
	}
}

// Add 喂入一个 SSE 事件。
//
// 注释心跳与空事件会被跳过：它们证明连接还活着，但不携带内容。
func (a *ChatStreamAccumulator) Add(ev Event) error {
	if a.done {
		return fmt.Errorf("protocol: 流已收到 %s，不应再有事件", StreamDoneMarker)
	}
	if ev.IsComment() || ev.Data == nil {
		return nil
	}

	data := bytes.TrimSpace(ev.Data)
	if len(data) == 0 {
		return nil
	}
	if string(data) == StreamDoneMarker {
		a.done = true
		return nil
	}

	a.totalSize += len(data)
	if int64(a.totalSize) > a.opts.maxBytes() {
		return fmt.Errorf("protocol: 流累积 %d 字节超过上限 %d", a.totalSize, a.opts.maxBytes())
	}

	var chunk struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Created int64  `json:"created"`
		Choices []struct {
			Index int `json:"index"`
			Delta struct {
				Role      string          `json:"role"`
				Content   *string         `json:"content"`
				Refusal   *string         `json:"refusal"`
				ToolCalls json.RawMessage `json:"tool_calls"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Usage *Usage `json:"usage"`
	}
	if err := json.Unmarshal(data, &chunk); err != nil {
		return fmt.Errorf("protocol: 流事件不是合法的 chunk：%w", err)
	}
	a.sawChunk = true

	// 顶层标识只认第一次。中途变化说明上游把两次响应混进了一条流，
	// 或者中间的代理串了会话——两者都不该静默接受。
	if chunk.ID != "" {
		if a.id != "" && a.id != chunk.ID {
			return fmt.Errorf("protocol: 流中途更换了响应 id：%q → %q", a.id, chunk.ID)
		}
		a.id = chunk.ID
	}
	if chunk.Model != "" {
		a.model = chunk.Model
	}
	if chunk.Created != 0 {
		a.created = chunk.Created
	}
	if chunk.Usage != nil {
		a.usage = chunk.Usage
	}

	if len(chunk.Choices) == 0 {
		// 只带 usage 的尾包是合法的（stream_options.include_usage）。
		return nil
	}
	if len(chunk.Choices) > 1 {
		return fmt.Errorf("protocol: 流事件含 %d 个 choice，工具调用场景下多候选语义不明确", len(chunk.Choices))
	}

	c := chunk.Choices[0]
	if c.Delta.Content != nil {
		a.content.WriteString(*c.Delta.Content)
	}
	if c.Delta.Refusal != nil {
		a.refusal.WriteString(*c.Delta.Refusal)
	}
	if c.FinishReason != nil && *c.FinishReason != "" {
		a.finishReason = FinishReason(*c.FinishReason)
	}

	return a.addToolCallDeltas(c.Delta.ToolCalls)
}

// addToolCallDeltas 按 index 把工具调用的增量并进对应的累积槽。
func (a *ChatStreamAccumulator) addToolCallDeltas(raw json.RawMessage) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var deltas []struct {
		Index    *int   `json:"index"`
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &deltas); err != nil {
		return fmt.Errorf("protocol: tool_calls 增量格式非法：%w", err)
	}

	for _, d := range deltas {
		// index 是关联增量的唯一依据。缺了它就无法判断这段参数属于哪次调用，
		// 按出现顺序猜会把并行调用的参数拼到一起。
		if d.Index == nil {
			return fmt.Errorf("protocol: tool_calls 增量缺少 index，无法关联到具体调用")
		}
		if d.Type != "" && d.Type != "function" {
			return fmt.Errorf("protocol: tool_calls 增量的 type 为 %q，只支持 function", d.Type)
		}

		pc, ok := a.calls[*d.Index]
		if !ok {
			pc = &partialToolCall{}
			a.calls[*d.Index] = pc
			a.order = append(a.order, *d.Index)
		}
		if d.ID != "" {
			if pc.id != "" && pc.id != d.ID {
				return fmt.Errorf("protocol: index %d 的调用 id 中途变化：%q → %q", *d.Index, pc.id, d.ID)
			}
			pc.id = d.ID
		}
		if d.Function.Name != "" {
			// 工具名理论上也可能被切开，但实践中上游总是一次给全。
			// 拼接而不是覆盖，两种情形都能应付。
			pc.name += d.Function.Name
		}
		pc.args.WriteString(d.Function.Arguments)

		a.totalSize += len(d.Function.Arguments)
		if int64(a.totalSize) > a.opts.maxBytes() {
			return fmt.Errorf("protocol: 流累积 %d 字节超过上限 %d", a.totalSize, a.opts.maxBytes())
		}
	}
	return nil
}

// Done 报告是否已收到终止标记。
func (a *ChatStreamAccumulator) Done() bool { return a.done }

// PartialText 返回断流前已经累积到的正文文本。
//
// 用途：上游中途断开时，已生成的文本不该跟着一起扔。调用方拿它做降级
// 透传——半截回答也比让用户等完一整轮重生成强。只在流未正常结束时
// 调用才有意义；正常结束时用 Result。
func (a *ChatStreamAccumulator) PartialText() string { return a.content.String() }

// Result 把累积结果组装成完整响应。
//
// 参数直到这一步才被解析：累积期间它只是一串可能被切碎的字节。解析失败
// 返回显式错误，绝不生成一个「能用」的兜底提案——规格三章的硬要求。
func (a *ChatStreamAccumulator) Result() (*ChatResponse, error) {
	if !a.sawChunk {
		return nil, fmt.Errorf("protocol: 流中没有任何 chunk")
	}
	// 既没有 [DONE] 也没有 finish_reason，说明流断在了中途。这与正常结束
	// 是两回事：前者的内容可能只有一半，当成完整响应用会静默丢数据。
	if !a.done && a.finishReason == "" {
		return nil, fmt.Errorf("protocol: 流未正常结束，既无 %s 也无 finish_reason", StreamDoneMarker)
	}

	resp := &ChatResponse{
		ID:           a.id,
		Model:        a.model,
		Created:      a.created,
		Refusal:      a.refusal.String(),
		FinishReason: a.finishReason,
		Usage:        a.usage,
	}
	if a.content.Len() > 0 {
		b, err := json.Marshal(a.content.String())
		if err != nil {
			return nil, fmt.Errorf("protocol: 编码累积的文本失败：%w", err)
		}
		resp.Content = b
	}

	// 按 index 升序还原调用顺序，而不是按首次出现顺序。上游可能先发
	// index 1 的第一片再发 index 0 的，客户端看到的应当是 index 定义的顺序。
	idx := make([]int, len(a.order))
	copy(idx, a.order)
	sort.Ints(idx)

	now := a.opts.now()
	for _, i := range idx {
		pc := a.calls[i]
		if pc.id == "" {
			return nil, fmt.Errorf("protocol: index %d 的调用累积结束时仍缺少 id", i)
		}
		args, err := decodeAccumulatedArguments(pc.args.Bytes())
		if err != nil {
			return nil, fmt.Errorf("protocol: index %d（id=%s）的%w", i, pc.id, err)
		}
		p := ir.ToolCallProposal{
			SessionID: a.opts.SessionID,
			RequestID: a.opts.RequestID,
			CallID:    pc.id,
			Tool: ir.ToolID{
				Namespace: ir.NamespaceClient,
				Name:      pc.name,
				Version:   ir.VersionDeclared,
			},
			Arguments:          args,
			Source:             ir.SourceNative,
			RawCandidateDigest: ir.DigestRawCandidate(pc.args.Bytes()),
			CreatedAt:          now,
		}
		if err := p.Validate(); err != nil {
			return nil, fmt.Errorf("protocol: index %d 的调用非法：%w", i, err)
		}
		resp.ToolCalls = append(resp.ToolCalls, p)
	}

	if err := resp.Validate(); err != nil {
		return nil, err
	}
	return resp, nil
}

// decodeAccumulatedArguments 解析累积完成的参数字节。
//
// 与非流式路径的区别：这里拿到的已经是解开引号后的原始 JSON，不需要再剥
// 一层字符串。空值按「无参数」处理成 {}——上游在只发了 name 却没发任何
// 参数片段时会留下空缓冲，那对无参工具是正常的。
func decodeAccumulatedArguments(raw []byte) (json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return json.RawMessage(`{}`), nil
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("参数累积完成后不是 JSON 对象：%w", err)
	}
	return compactJSON(json.RawMessage(raw))
}

// EncodeChatStream 把一个完整响应渲染成 chat.completion.chunk 序列。
//
// 用于虚拟协议模式：Runtime 从纯文本上游解析出调用后，要模拟成客户端
// 期待的流式形态。拆分粒度按 item 而不是按字节——伪造逐字的打字机效果
// 既无必要，也会让客户端误以为上游真的在流式生成。
func EncodeChatStream(enc *SSEEncoder, r ChatResponse) error {
	if err := r.Validate(); err != nil {
		return err
	}

	head := func(delta map[string]any, finish any) map[string]any {
		return newChatChunk(r, delta, finish)
	}

	write := func(v map[string]any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Errorf("protocol: 编码 chunk 失败：%w", err)
		}
		return enc.Write(Event{Data: b})
	}

	// 首包只带 role，这是 OpenAI 的既定形态，客户端据此知道消息开始了。
	if err := write(head(map[string]any{"role": "assistant"}, nil)); err != nil {
		return err
	}

	if len(r.Content) > 0 && string(r.Content) != "null" {
		var s string
		if err := json.Unmarshal(r.Content, &s); err != nil {
			return fmt.Errorf("protocol: 流式渲染要求 content 是字符串：%w", err)
		}
		if s != "" {
			if err := write(head(map[string]any{"content": s}, nil)); err != nil {
				return err
			}
		}
	}
	if r.Refusal != "" {
		if err := write(head(map[string]any{"refusal": r.Refusal}, nil)); err != nil {
			return err
		}
	}

	return encodeChatTail(enc, r)
}

// EncodeChatStreamTail 渲染真流式的收尾段：tool_calls → finish_reason →
// usage → [DONE]。skipText 为真时跳过 content/refusal 重发——那些增量已经
// 实时到达，重复会让客户端看到两份正文。
//
// 事件形状与 EncodeChatStream 的对应段落逐一相同（同一个 head 构造器），
// 分叉即 bug。
func EncodeChatStreamTail(enc *SSEEncoder, r ChatResponse, skipText bool) error {
	if err := r.Validate(); err != nil {
		return err
	}
	if !skipText {
		// 未流式过：完整渲染（含首包与文本）。
		return EncodeChatStream(enc, r)
	}
	return encodeChatTail(enc, r)
}

// encodeChatTail 是两条路径共用的 Chat 收尾段。
func encodeChatTail(enc *SSEEncoder, r ChatResponse) error {
	write := func(v map[string]any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Errorf("protocol: 编码 chunk 失败：%w", err)
		}
		return enc.Write(Event{Data: b})
	}

	if err := rejectFreeform("chat", r.ToolCalls); err != nil {
		return err
	}
	for i, c := range r.ToolCalls {
		delta := map[string]any{
			"tool_calls": []any{map[string]any{
				"index": i,
				"id":    c.CallID,
				"type":  "function",
				"function": map[string]any{
					"name":      c.Tool.Name,
					"arguments": string(c.Arguments),
				},
			}},
		}
		if err := write(newChatChunk(r, delta, nil)); err != nil {
			return err
		}
	}

	if err := write(newChatChunk(r, map[string]any{}, string(r.FinishReason))); err != nil {
		return err
	}
	if r.Usage != nil {
		final := newChatChunk(r, map[string]any{}, string(r.FinishReason))
		final["usage"] = r.Usage
		final["choices"] = []any{}
		if err := write(final); err != nil {
			return err
		}
	}
	return enc.Write(Event{Data: []byte(StreamDoneMarker)})
}

// newChatChunk 构造一个带公共头的 chunk。头字段（id/object/created/model）
// 在同一条流里必须恒定。
func newChatChunk(r ChatResponse, delta map[string]any, finish any) map[string]any {
	return map[string]any{
		"id":      r.ID,
		"object":  "chat.completion.chunk",
		"created": r.Created,
		"model":   r.Model,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         delta,
			"finish_reason": finish,
		}},
	}
}
