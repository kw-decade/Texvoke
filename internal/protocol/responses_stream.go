package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/kw-decade/Texvoke/internal/ir"
)

// Responses 流式事件类型。
const (
	EventResponseCreated       = "response.created"
	EventResponseInProgress    = "response.in_progress"
	EventResponseCompleted     = "response.completed"
	EventResponseIncomplete    = "response.incomplete"
	EventResponseFailed        = "response.failed"
	EventOutputItemAdded       = "response.output_item.added"
	EventOutputItemDone        = "response.output_item.done"
	EventOutputTextDelta       = "response.output_text.delta"
	EventFunctionArgsDelta     = "response.function_call_arguments.delta"
	EventFunctionArgsDone      = "response.function_call_arguments.done"
	EventResponseErrorEventTyp = "error"
)

// partialResponseItem 是一个尚未累积完的 output item。
type partialResponseItem struct {
	itemID string
	kind   string

	callID string
	name   string
	args   bytes.Buffer
	text   bytes.Buffer

	// content 保留 message item 在 done 事件里给出的完整 content，
	// 优先于逐字累积的 text——前者是上游的权威版本。
	content json.RawMessage

	done bool
}

// ResponsesStreamAccumulator 把 Responses 的流式事件累积成完整响应。
//
// 这个协议有个别处没有的特点：output_item.done 与 response.completed 会重发
// 完整的 item。规格十章要求「最终 response.output 必须与流中已发送的 item
// 对应」，所以这里不只是累积，还要拿完整版校验增量版——两者不一致意味着
// 上游有缺陷，或中间有代理在改写流，任何一种都不该静默接受。
type ResponsesStreamAccumulator struct {
	opts DecodeOptions

	responseID       string
	model            string
	createdAt        int64
	status           ResponseStatus
	incompleteReason string
	usage            *ResponsesUsage

	items map[int]*partialResponseItem
	order []int

	terminal  bool
	totalSize int
}

// NewResponsesStreamAccumulator 创建一个累积器。
func NewResponsesStreamAccumulator(opts DecodeOptions) *ResponsesStreamAccumulator {
	return &ResponsesStreamAccumulator{
		opts:  opts,
		items: make(map[int]*partialResponseItem, 2),
	}
}

// Add 喂入一个 SSE 事件。
func (a *ResponsesStreamAccumulator) Add(ev Event) error {
	if a.terminal {
		return fmt.Errorf("protocol: 流已进入终态 %q，不应再有事件", a.status)
	}
	if ev.IsComment() || len(ev.Data) == 0 {
		return nil
	}

	a.totalSize += len(ev.Data)
	if int64(a.totalSize) > a.opts.maxBytes() {
		return fmt.Errorf("protocol: 流累积 %d 字节超过上限 %d", a.totalSize, a.opts.maxBytes())
	}

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
	case EventResponseCreated, EventResponseInProgress,
		EventResponseCompleted, EventResponseIncomplete, EventResponseFailed:
		return a.handleResponseEnvelope(kind, ev.Data)
	case EventOutputItemAdded:
		return a.handleItemAdded(ev.Data)
	case EventOutputItemDone:
		return a.handleItemDone(ev.Data)
	case EventOutputTextDelta:
		return a.handleTextDelta(ev.Data)
	case EventFunctionArgsDelta:
		return a.handleArgsDelta(ev.Data)
	case EventFunctionArgsDone:
		return a.handleArgsDone(ev.Data)
	case EventResponseErrorEventTyp:
		var e struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		}
		if err := json.Unmarshal(ev.Data, &e); err != nil {
			return &UpstreamError{Message: string(ev.Data)}
		}
		if e.Message == "" {
			e.Message = string(ev.Data)
		}
		return &UpstreamError{Message: e.Message, Code: e.Code}
	default:
		// 未知事件跳过：Responses 的事件类型很多且仍在扩充，
		// 遇到没见过的就中断会让 Bridge 在上游升级后突然罢工。
		return nil
	}
}

func (a *ResponsesStreamAccumulator) handleResponseEnvelope(kind string, data []byte) error {
	var ev struct {
		Response struct {
			ID                string          `json:"id"`
			Model             string          `json:"model"`
			CreatedAt         int64           `json:"created_at"`
			Status            string          `json:"status"`
			Output            json.RawMessage `json:"output"`
			Usage             *ResponsesUsage `json:"usage"`
			IncompleteDetails *struct {
				Reason string `json:"reason"`
			} `json:"incomplete_details"`
		} `json:"response"`
	}
	if err := json.Unmarshal(data, &ev); err != nil {
		return fmt.Errorf("protocol: %s 格式非法：%w", kind, err)
	}

	r := ev.Response
	// 响应 id 中途变化说明上游把两次响应混进了一条流，或代理串了会话。
	if r.ID != "" {
		if a.responseID != "" && a.responseID != r.ID {
			return fmt.Errorf("protocol: 流中途更换了响应 id：%q → %q", a.responseID, r.ID)
		}
		a.responseID = r.ID
	}
	if r.Model != "" {
		a.model = r.Model
	}
	if r.CreatedAt != 0 {
		a.createdAt = r.CreatedAt
	}
	if r.Status != "" {
		a.status = ResponseStatus(r.Status)
	}
	if r.Usage != nil {
		a.usage = r.Usage
	}
	if r.IncompleteDetails != nil {
		a.incompleteReason = r.IncompleteDetails.Reason
	}

	switch kind {
	case EventResponseCompleted, EventResponseIncomplete, EventResponseFailed:
		a.terminal = true
		// 终态事件带完整的 output，用它校验流中累积的结果。
		if len(r.Output) > 0 {
			return a.verifyFinalOutput(r.Output)
		}
	}
	return nil
}

// verifyFinalOutput 拿终态事件里的完整 output 校验增量累积的结果。
//
// 规格十章：「最终 response.output 必须与流中已发送的 item 对应」。
// 不一致意味着上游有缺陷或中间有代理在改写流——静默采信任何一边都可能
// 让客户端拿到一份与它看到的增量不符的最终结果。
func (a *ResponsesStreamAccumulator) verifyFinalOutput(output json.RawMessage) error {
	var finals []struct {
		ID        string `json:"id"`
		Type      string `json:"type"`
		CallID    string `json:"call_id"`
		Arguments string `json:"arguments"`
	}
	if err := json.Unmarshal(output, &finals); err != nil {
		return fmt.Errorf("protocol: 终态事件的 output 格式非法：%w", err)
	}

	// 只校验流中确实出现过的 item。上游可能在终态里补上一些从未以增量
	// 形式发过的 item（例如被过滤掉的内容），那不是我们要拦的情形。
	byID := make(map[string]*partialResponseItem, len(a.items))
	for _, it := range a.items {
		if it.itemID != "" {
			byID[it.itemID] = it
		}
	}

	for _, f := range finals {
		it, ok := byID[f.ID]
		if !ok {
			continue
		}
		if f.Type != "" && it.kind != "" && f.Type != it.kind {
			return fmt.Errorf("protocol: item %s 的类型在终态里是 %q，流中是 %q", f.ID, f.Type, it.kind)
		}
		if f.CallID != "" && it.callID != "" && f.CallID != it.callID {
			return fmt.Errorf("protocol: item %s 的 call_id 在终态里是 %q，流中是 %q", f.ID, f.CallID, it.callID)
		}
		if f.Type == "function_call" && f.Arguments != "" {
			if got := it.args.String(); got != "" && got != f.Arguments {
				return fmt.Errorf("protocol: item %s 的参数与增量累积结果不一致\n终态：%s\n累积：%s",
					f.ID, f.Arguments, got)
			}
		}
	}
	return nil
}

func (a *ResponsesStreamAccumulator) handleItemAdded(data []byte) error {
	var ev struct {
		OutputIndex *int `json:"output_index"`
		Item        struct {
			ID     string `json:"id"`
			Type   string `json:"type"`
			CallID string `json:"call_id"`
			Name   string `json:"name"`
		} `json:"item"`
	}
	if err := json.Unmarshal(data, &ev); err != nil {
		return fmt.Errorf("protocol: %s 格式非法：%w", EventOutputItemAdded, err)
	}
	if ev.OutputIndex == nil {
		return fmt.Errorf("protocol: %s 缺少 output_index，后续增量将无法归位", EventOutputItemAdded)
	}
	if _, exists := a.items[*ev.OutputIndex]; exists {
		return fmt.Errorf("protocol: output_index %d 的 item 重复添加", *ev.OutputIndex)
	}

	a.items[*ev.OutputIndex] = &partialResponseItem{
		itemID: ev.Item.ID,
		kind:   ev.Item.Type,
		callID: ev.Item.CallID,
		name:   ev.Item.Name,
	}
	a.order = append(a.order, *ev.OutputIndex)
	return nil
}

func (a *ResponsesStreamAccumulator) handleItemDone(data []byte) error {
	var ev struct {
		OutputIndex *int `json:"output_index"`
		Item        struct {
			ID        string          `json:"id"`
			Type      string          `json:"type"`
			CallID    string          `json:"call_id"`
			Name      string          `json:"name"`
			Arguments string          `json:"arguments"`
			Content   json.RawMessage `json:"content"`
		} `json:"item"`
	}
	if err := json.Unmarshal(data, &ev); err != nil {
		return fmt.Errorf("protocol: %s 格式非法：%w", EventOutputItemDone, err)
	}
	if ev.OutputIndex == nil {
		return fmt.Errorf("protocol: %s 缺少 output_index", EventOutputItemDone)
	}
	it, ok := a.items[*ev.OutputIndex]
	if !ok {
		return fmt.Errorf("protocol: output_index %d 的结束事件没有对应的 added 事件", *ev.OutputIndex)
	}

	// done 事件带完整 item，逐项核对流中累积的版本。
	if ev.Item.ID != "" && it.itemID != "" && ev.Item.ID != it.itemID {
		return fmt.Errorf("protocol: output_index %d 的 item id 变化：%q → %q", *ev.OutputIndex, it.itemID, ev.Item.ID)
	}
	if ev.Item.CallID != "" && it.callID != "" && ev.Item.CallID != it.callID {
		return fmt.Errorf("protocol: output_index %d 的 call_id 变化：%q → %q", *ev.OutputIndex, it.callID, ev.Item.CallID)
	}
	if ev.Item.Arguments != "" && it.args.Len() > 0 && ev.Item.Arguments != it.args.String() {
		return fmt.Errorf("protocol: output_index %d 的参数与增量累积结果不一致\n完整：%s\n累积：%s",
			*ev.OutputIndex, ev.Item.Arguments, it.args.String())
	}

	// 补上流中没有以增量形式给过的部分。
	if it.itemID == "" {
		it.itemID = ev.Item.ID
	}
	if it.kind == "" {
		it.kind = ev.Item.Type
	}
	if it.callID == "" {
		it.callID = ev.Item.CallID
	}
	if it.name == "" {
		it.name = ev.Item.Name
	}
	if it.args.Len() == 0 && ev.Item.Arguments != "" {
		it.args.WriteString(ev.Item.Arguments)
	}
	if len(ev.Item.Content) > 0 {
		it.content = ev.Item.Content
	}
	it.done = true
	return nil
}

func (a *ResponsesStreamAccumulator) handleTextDelta(data []byte) error {
	var ev struct {
		OutputIndex *int   `json:"output_index"`
		ItemID      string `json:"item_id"`
		Delta       string `json:"delta"`
	}
	if err := json.Unmarshal(data, &ev); err != nil {
		return fmt.Errorf("protocol: %s 格式非法：%w", EventOutputTextDelta, err)
	}
	it, err := a.itemFor(ev.OutputIndex, ev.ItemID, EventOutputTextDelta)
	if err != nil {
		return err
	}
	it.text.WriteString(ev.Delta)
	return nil
}

func (a *ResponsesStreamAccumulator) handleArgsDelta(data []byte) error {
	var ev struct {
		OutputIndex *int   `json:"output_index"`
		ItemID      string `json:"item_id"`
		Delta       string `json:"delta"`
	}
	if err := json.Unmarshal(data, &ev); err != nil {
		return fmt.Errorf("protocol: %s 格式非法：%w", EventFunctionArgsDelta, err)
	}
	it, err := a.itemFor(ev.OutputIndex, ev.ItemID, EventFunctionArgsDelta)
	if err != nil {
		return err
	}
	it.args.WriteString(ev.Delta)
	return nil
}

func (a *ResponsesStreamAccumulator) handleArgsDone(data []byte) error {
	var ev struct {
		OutputIndex *int   `json:"output_index"`
		ItemID      string `json:"item_id"`
		Arguments   string `json:"arguments"`
	}
	if err := json.Unmarshal(data, &ev); err != nil {
		return fmt.Errorf("protocol: %s 格式非法：%w", EventFunctionArgsDone, err)
	}
	it, err := a.itemFor(ev.OutputIndex, ev.ItemID, EventFunctionArgsDone)
	if err != nil {
		return err
	}
	// done 事件给出完整参数，与累积结果核对。
	if ev.Arguments != "" && it.args.Len() > 0 && ev.Arguments != it.args.String() {
		return fmt.Errorf("protocol: item %s 的参数与增量累积结果不一致\n完整：%s\n累积：%s",
			it.itemID, ev.Arguments, it.args.String())
	}
	if it.args.Len() == 0 && ev.Arguments != "" {
		it.args.WriteString(ev.Arguments)
	}
	return nil
}

// itemFor 按 output_index 定位 item，缺省时退回按 item_id 查找。
//
// 两个定位键都保留，是因为规格十章要求 item_id 与 output_index 都稳定一致；
// 任一个能用就不该因为另一个缺失而失败。
func (a *ResponsesStreamAccumulator) itemFor(outputIndex *int, itemID, evKind string) (*partialResponseItem, error) {
	if outputIndex != nil {
		it, ok := a.items[*outputIndex]
		if !ok {
			return nil, fmt.Errorf("protocol: output_index %d 的 %s 没有对应的 added 事件", *outputIndex, evKind)
		}
		if itemID != "" && it.itemID != "" && itemID != it.itemID {
			return nil, fmt.Errorf("protocol: output_index %d 的 item_id 不匹配：%q ≠ %q", *outputIndex, itemID, it.itemID)
		}
		return it, nil
	}
	if itemID != "" {
		for _, it := range a.items {
			if it.itemID == itemID {
				return it, nil
			}
		}
		return nil, fmt.Errorf("protocol: item_id %q 的 %s 没有对应的 added 事件", itemID, evKind)
	}
	return nil, fmt.Errorf("protocol: %s 既无 output_index 也无 item_id，无法归位", evKind)
}

// Done 报告流是否已进入终态。
func (a *ResponsesStreamAccumulator) Done() bool { return a.terminal }

// Result 把累积结果组装成完整响应。
func (a *ResponsesStreamAccumulator) Result() (*ResponsesResponse, error) {
	if a.responseID == "" {
		return nil, fmt.Errorf("protocol: 流中没有任何 response 事件")
	}
	if !a.terminal {
		return nil, fmt.Errorf("protocol: 流未进入终态，最后状态为 %q", a.status)
	}

	resp := &ResponsesResponse{
		ID:               a.responseID,
		Model:            a.model,
		CreatedAt:        a.createdAt,
		Status:           a.status,
		IncompleteReason: a.incompleteReason,
		Usage:            a.usage,
	}

	idx := make([]int, len(a.order))
	copy(idx, a.order)
	sort.Ints(idx)

	now := a.opts.now()
	for _, i := range idx {
		it := a.items[i]
		switch it.kind {
		case "function_call":
			if it.callID == "" {
				return nil, fmt.Errorf("protocol: output_index %d 的调用缺少 call_id", i)
			}
			args, err := decodeAccumulatedArguments(it.args.Bytes())
			if err != nil {
				return nil, fmt.Errorf("protocol: output_index %d（call_id=%s）的%w", i, it.callID, err)
			}
			p := ir.ToolCallProposal{
				SessionID:      a.opts.SessionID,
				RequestID:      a.opts.RequestID,
				CallID:         it.callID,
				ProtocolItemID: it.itemID,
				Tool: ir.ToolID{
					Namespace: ir.NamespaceClient,
					Name:      it.name,
					Version:   ir.VersionDeclared,
				},
				Arguments:          args,
				Source:             ir.SourceNative,
				RawCandidateDigest: ir.DigestRawCandidate(it.args.Bytes()),
				CreatedAt:          now,
			}
			if err := p.Validate(); err != nil {
				return nil, fmt.Errorf("protocol: output_index %d 的调用非法：%w", i, err)
			}
			resp.ToolCalls = append(resp.ToolCalls, p)

		case "message":
			if resp.Content != nil {
				return nil, fmt.Errorf("protocol: 流中含多个 message item，当前只支持一个")
			}
			resp.MessageItemID = it.itemID
			// done 事件给出的完整 content 是权威版本；没有时用逐字累积的文本兜底。
			if len(it.content) > 0 {
				c, err := compactJSON(it.content)
				if err != nil {
					return nil, fmt.Errorf("protocol: output_index %d 的 content %w", i, err)
				}
				resp.Content = c
			} else if it.text.Len() > 0 {
				block, err := json.Marshal([]any{map[string]string{
					"type": "output_text", "text": it.text.String(),
				}})
				if err != nil {
					return nil, fmt.Errorf("protocol: 编码 output_index %d 的文本失败：%w", i, err)
				}
				resp.Content = block
			}

		case "reasoning":
			return nil, fmt.Errorf("protocol: output_index %d 是 reasoning item，暂不支持", i)

		default:
			return nil, fmt.Errorf("protocol: output_index %d 的 item 类型 %q 暂不支持", i, it.kind)
		}
	}

	if err := resp.Validate(); err != nil {
		return nil, err
	}
	return resp, nil
}

// EncodeResponsesStream 把一个完整响应渲染成 Responses 的事件序列。
func EncodeResponsesStream(enc *SSEEncoder, r ResponsesResponse) error {
	if err := r.Validate(); err != nil {
		return err
	}

	write := func(evType string, v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Errorf("protocol: 编码 %s 事件失败：%w", evType, err)
		}
		return enc.Write(Event{Type: evType, Data: b})
	}

	envelope := func(status ResponseStatus, output []any) map[string]any {
		m := map[string]any{
			"id":         r.ID,
			"object":     "response",
			"created_at": r.CreatedAt,
			"status":     string(status),
			"model":      r.Model,
			"output":     output,
		}
		if r.Usage != nil && status.Terminal() {
			m["usage"] = r.Usage
		}
		if r.IncompleteReason != "" && status == ResponseIncomplete {
			m["incomplete_details"] = map[string]string{"reason": r.IncompleteReason}
		}
		return m
	}

	if err := write(EventResponseCreated, map[string]any{
		"type": EventResponseCreated, "response": envelope(ResponseInProgress, []any{}),
	}); err != nil {
		return err
	}

	index := 0
	var finalOutput []any

	if len(r.Content) > 0 && string(r.Content) != "null" {
		item := map[string]any{
			"id": r.MessageItemID, "type": "message", "role": "assistant",
			"status": "completed", "content": r.Content,
		}
		if err := write(EventOutputItemAdded, map[string]any{
			"type": EventOutputItemAdded, "output_index": index,
			"item": map[string]any{
				"id": r.MessageItemID, "type": "message", "role": "assistant",
				"status": "in_progress", "content": []any{},
			},
		}); err != nil {
			return err
		}
		if err := write(EventOutputItemDone, map[string]any{
			"type": EventOutputItemDone, "output_index": index, "item": item,
		}); err != nil {
			return err
		}
		finalOutput = append(finalOutput, item)
		index++
	}

	for _, c := range r.ToolCalls {
		if c.ArgumentForm.Text() {
			// 裸文本调用只发 output_item.added 与 output_item.done，不发增量。
			//
			// 不是偷懒：custom 工具的增量事件名没有可核对的真实样本
			// （历史会话只存最终 item，不存 SSE）。猜一个名字发出去，客户端
			// 要么忽略（白发）要么按错的形状去拼（更糟）。而 item.done 里带
			// 完整的 input，客户端照样能拿到全部内容——本来就是伪流式，
			// 第一个字要等模型说完才到，少发增量不损失任何东西。
			text, err := c.ArgumentsText()
			if err != nil {
				return err
			}
			if err := write(EventOutputItemAdded, map[string]any{
				"type": EventOutputItemAdded, "output_index": index,
				"item": map[string]any{
					"id": c.ProtocolItemID, "type": "custom_tool_call", "status": "in_progress",
					"call_id": c.CallID, "name": c.Tool.Name, "input": "",
				},
			}); err != nil {
				return err
			}
			item := map[string]any{
				"id": c.ProtocolItemID, "type": "custom_tool_call", "status": "completed",
				"call_id": c.CallID, "name": c.Tool.Name, "input": text,
			}
			if err := write(EventOutputItemDone, map[string]any{
				"type": EventOutputItemDone, "output_index": index, "item": item,
			}); err != nil {
				return err
			}
			finalOutput = append(finalOutput, item)
			index++
			continue
		}

		if err := write(EventOutputItemAdded, map[string]any{
			"type": EventOutputItemAdded, "output_index": index,
			"item": map[string]any{
				"id": c.ProtocolItemID, "type": "function_call", "status": "in_progress",
				"call_id": c.CallID, "name": c.Tool.Name, "arguments": "",
			},
		}); err != nil {
			return err
		}
		if err := write(EventFunctionArgsDelta, map[string]any{
			"type": EventFunctionArgsDelta, "output_index": index,
			"item_id": c.ProtocolItemID, "delta": string(c.Arguments),
		}); err != nil {
			return err
		}
		if err := write(EventFunctionArgsDone, map[string]any{
			"type": EventFunctionArgsDone, "output_index": index,
			"item_id": c.ProtocolItemID, "arguments": string(c.Arguments),
		}); err != nil {
			return err
		}
		item := map[string]any{
			"id": c.ProtocolItemID, "type": "function_call", "status": "completed",
			"call_id": c.CallID, "name": c.Tool.Name, "arguments": string(c.Arguments),
		}
		if err := write(EventOutputItemDone, map[string]any{
			"type": EventOutputItemDone, "output_index": index, "item": item,
		}); err != nil {
			return err
		}
		finalOutput = append(finalOutput, item)
		index++
	}

	evType := EventResponseCompleted
	switch r.Status {
	case ResponseIncomplete:
		evType = EventResponseIncomplete
	case ResponseFailed:
		evType = EventResponseFailed
	}
	return write(evType, map[string]any{
		"type": evType, "response": envelope(r.Status, finalOutput),
	})
}
