package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// feedAnth 按 (事件类型, 负载) 成对喂入累积器。
func feedAnth(t *testing.T, a *AnthropicStreamAccumulator, pairs ...string) {
	t.Helper()
	if len(pairs)%2 != 0 {
		t.Fatal("参数必须成对：事件类型 + 负载")
	}
	for i := 0; i < len(pairs); i += 2 {
		if err := a.Add(Event{Type: pairs[i], Data: []byte(pairs[i+1])}); err != nil {
			t.Fatalf("第 %d 个事件（%s）累积失败：%v", i/2, pairs[i], err)
		}
	}
}

const anthMsgStart = `{"type":"message_start","message":{"id":"msg_01","type":"message","role":"assistant","model":"claude-sonnet-5","content":[],"stop_reason":null,"usage":{"input_tokens":100,"output_tokens":1}}}`

// input_json_delta 的片段以任意边界到达，累积器必须能还原。
func TestAnthropicStreamAccumulatesFragmentedInput(t *testing.T) {
	fullArgs := `{"city":"San Francisco","unit":"celsius"}`

	for _, size := range []int{1, 3, 7, len(fullArgs)} {
		t.Run("片长="+itoa(size), func(t *testing.T) {
			a := NewAnthropicStreamAccumulator(testOpts)
			feedAnth(t, a,
				EventMessageStart, anthMsgStart,
				EventContentBlockStart, `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_01","name":"get_weather","input":{}}}`,
			)

			for i := 0; i < len(fullArgs); i += size {
				end := i + size
				if end > len(fullArgs) {
					end = len(fullArgs)
				}
				frag, err := json.Marshal(fullArgs[i:end])
				if err != nil {
					t.Fatal(err)
				}
				feedAnth(t, a, EventContentBlockDelta,
					`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":`+string(frag)+`}}`)
			}

			feedAnth(t, a,
				EventContentBlockStop, `{"type":"content_block_stop","index":0}`,
				EventMessageDelta, `{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":50}}`,
				EventMessageStop, `{"type":"message_stop"}`,
			)

			resp, err := a.Result()
			if err != nil {
				t.Fatalf("组装失败：%v", err)
			}
			if len(resp.ToolCalls) != 1 {
				t.Fatalf("调用数为 %d", len(resp.ToolCalls))
			}
			call := resp.ToolCalls[0]
			if call.CallID != "toolu_01" || call.Tool.Name != "get_weather" {
				t.Errorf("调用标识有误：%+v", call)
			}
			if string(call.Arguments) != fullArgs {
				t.Errorf("参数累积为 %s，期望 %s", call.Arguments, fullArgs)
			}
			if resp.StopReason != StopToolUse {
				t.Errorf("stop_reason 为 %q", resp.StopReason)
			}
		})
	}
}

// 文本与工具调用混排，各 block 按 index 独立累积。
func TestAnthropicStreamMixedBlocks(t *testing.T) {
	a := NewAnthropicStreamAccumulator(testOpts)
	feedAnth(t, a,
		EventMessageStart, anthMsgStart,
		EventContentBlockStart, `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		EventContentBlockDelta, `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"我来"}}`,
		EventContentBlockDelta, `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"查一下"}}`,
		EventContentBlockStop, `{"type":"content_block_stop","index":0}`,
		EventContentBlockStart, `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_a","name":"get_weather","input":{}}}`,
		EventContentBlockDelta, `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"city\":\"SF\"}"}}`,
		EventContentBlockStop, `{"type":"content_block_stop","index":1}`,
		EventContentBlockStart, `{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_b","name":"get_weather","input":{}}}`,
		EventContentBlockDelta, `{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"city\":\"Tokyo\"}"}}`,
		EventContentBlockStop, `{"type":"content_block_stop","index":2}`,
		EventMessageDelta, `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":80}}`,
		EventMessageStop, `{"type":"message_stop"}`,
	)

	resp, err := a.Result()
	if err != nil {
		t.Fatalf("组装失败：%v", err)
	}
	if !strings.Contains(string(resp.Content), "我来查一下") {
		t.Errorf("文本 block 累积为 %s", resp.Content)
	}
	if len(resp.ToolCalls) != 2 {
		t.Fatalf("调用数为 %d，期望 2", len(resp.ToolCalls))
	}
	if resp.ToolCalls[0].CallID != "toolu_a" || resp.ToolCalls[1].CallID != "toolu_b" {
		t.Errorf("调用顺序错乱：%q %q", resp.ToolCalls[0].CallID, resp.ToolCalls[1].CallID)
	}
	// input_tokens 来自 message_start，output_tokens 来自 message_delta。
	// 合并而不是覆盖，否则输入侧计数会被清零。
	if resp.Usage == nil || resp.Usage.InputTokens != 100 || resp.Usage.OutputTokens != 80 {
		t.Errorf("usage 合并有误：%+v", resp.Usage)
	}
}

// thinking block 原样重组，签名必须一起带上——丢了签名的 thinking
// 会被 Anthropic 拒绝，而且我们无权改写模型的隐藏推理内容。
func TestAnthropicStreamThinkingBlock(t *testing.T) {
	a := NewAnthropicStreamAccumulator(testOpts)
	feedAnth(t, a,
		EventMessageStart, anthMsgStart,
		EventContentBlockStart, `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
		EventContentBlockDelta, `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"先想想"}}`,
		EventContentBlockDelta, `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"再决定"}}`,
		EventContentBlockDelta, `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sigABC"}}`,
		EventContentBlockStop, `{"type":"content_block_stop","index":0}`,
		EventMessageDelta, `{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
		EventMessageStop, `{"type":"message_stop"}`,
	)

	resp, err := a.Result()
	if err != nil {
		t.Fatalf("组装失败：%v", err)
	}
	content := string(resp.Content)
	for _, want := range []string{"thinking", "先想想再决定", "sigABC"} {
		if !strings.Contains(content, want) {
			t.Errorf("content 中丢失了 %q：%s", want, content)
		}
	}
}

// ping 与未知事件跳过而不报错：Anthropic 会新增事件类型，
// 遇到没见过的就中断会让 Bridge 在上游升级后突然罢工。
func TestAnthropicStreamIgnoresPingAndUnknown(t *testing.T) {
	a := NewAnthropicStreamAccumulator(testOpts)
	feedAnth(t, a,
		EventMessageStart, anthMsgStart,
		EventPing, `{"type":"ping"}`,
		"some_future_event", `{"type":"some_future_event","payload":1}`,
		EventContentBlockStart, `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		EventPing, `{"type":"ping"}`,
		EventContentBlockDelta, `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
		EventContentBlockStop, `{"type":"content_block_stop","index":0}`,
		EventMessageDelta, `{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
		EventMessageStop, `{"type":"message_stop"}`,
	)

	if err := a.Add(Event{Comment: "heartbeat"}); err == nil {
		// 注释心跳在 message_stop 之后也不应报错，它不携带内容。
		t.Log("注释心跳被正确忽略")
	}

	resp, err := a.Result()
	if err != nil {
		t.Fatalf("组装失败：%v", err)
	}
	if !strings.Contains(string(resp.Content), "ok") {
		t.Errorf("文本丢失：%s", resp.Content)
	}
}

// 上游在流中途报错时，要返回可分类的 *UpstreamError，
// 而不是笼统的格式错误——拒绝分类依赖这个类型。
func TestAnthropicStreamUpstreamError(t *testing.T) {
	a := NewAnthropicStreamAccumulator(testOpts)
	feedAnth(t, a, EventMessageStart, anthMsgStart)

	err := a.Add(Event{
		Type: EventError,
		Data: []byte(`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`),
	})
	if err == nil {
		t.Fatal("流中的 error 事件必须报错")
	}
	var ue *UpstreamError
	if !errors.As(err, &ue) {
		t.Fatalf("错误类型为 %T，期望 *UpstreamError", err)
	}
	if ue.Type != "overloaded_error" {
		t.Errorf("错误分类丢失：%+v", ue)
	}
}

func TestAnthropicStreamRejects(t *testing.T) {
	tests := []struct {
		name  string
		pairs []string
		want  string
	}{
		{
			"增量缺少 index",
			[]string{
				EventMessageStart, anthMsgStart,
				EventContentBlockDelta, `{"type":"content_block_delta","delta":{"type":"text_delta","text":"x"}}`,
			},
			"缺少 index",
		},
		{
			"增量没有对应的 start",
			[]string{
				EventMessageStart, anthMsgStart,
				EventContentBlockDelta, `{"type":"content_block_delta","index":5,"delta":{"type":"text_delta","text":"x"}}`,
			},
			"没有对应的 content_block_start",
		},
		{
			"block 结束后仍有增量",
			[]string{
				EventMessageStart, anthMsgStart,
				EventContentBlockStart, `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
				EventContentBlockStop, `{"type":"content_block_stop","index":0}`,
				EventContentBlockDelta, `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"x"}}`,
			},
			"已结束",
		},
		{
			"同一 index 重复开始",
			[]string{
				EventMessageStart, anthMsgStart,
				EventContentBlockStart, `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
				EventContentBlockStart, `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			},
			"重复开始",
		},
		{
			"两次 message_start",
			[]string{EventMessageStart, anthMsgStart, EventMessageStart, anthMsgStart},
			"两次",
		},
		{
			"tool_use block 缺 id",
			[]string{
				EventMessageStart, anthMsgStart,
				EventContentBlockStart, `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","name":"f","input":{}}}`,
			},
			"缺少 id",
		},
		{
			"message_start 角色不是 assistant",
			[]string{EventMessageStart, `{"type":"message_start","message":{"id":"m","role":"user"}}`},
			"期望 assistant",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := NewAnthropicStreamAccumulator(testOpts)
			var err error
			for i := 0; i < len(tc.pairs); i += 2 {
				if err = a.Add(Event{Type: tc.pairs[i], Data: []byte(tc.pairs[i+1])}); err != nil {
					break
				}
			}
			if err == nil {
				t.Fatalf("期望报错包含 %q，却全部接受了", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误信息 %q 未包含 %q", err.Error(), tc.want)
			}
		})
	}
}

func TestAnthropicStreamRejectsMalformedAccumulatedInput(t *testing.T) {
	a := NewAnthropicStreamAccumulator(testOpts)
	feedAnth(t, a,
		EventMessageStart, anthMsgStart,
		EventContentBlockStart, `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t1","name":"f","input":{}}}`,
		EventContentBlockDelta, `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"a\":"}}`,
		EventContentBlockStop, `{"type":"content_block_stop","index":0}`,
		EventMessageDelta, `{"type":"message_delta","delta":{"stop_reason":"tool_use"}}`,
		EventMessageStop, `{"type":"message_stop"}`,
	)

	if _, err := a.Result(); err == nil {
		t.Fatal("残缺的参数必须报错")
	} else if !strings.Contains(err.Error(), "不是 JSON 对象") {
		t.Errorf("错误信息 %q 未说明原因", err.Error())
	}
}

func TestAnthropicStreamTruncated(t *testing.T) {
	a := NewAnthropicStreamAccumulator(testOpts)
	feedAnth(t, a,
		EventMessageStart, anthMsgStart,
		EventContentBlockStart, `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		EventContentBlockDelta, `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"说到一半"}}`,
	)

	if _, err := a.Result(); err == nil {
		t.Fatal("未收到 message_stop 也没有 stop_reason，必须报错")
	} else if !strings.Contains(err.Error(), "未正常结束") {
		t.Errorf("错误信息 %q", err.Error())
	}
}

func TestAnthropicStreamNoMessageStart(t *testing.T) {
	a := NewAnthropicStreamAccumulator(testOpts)
	if _, err := a.Result(); err == nil {
		t.Fatal("没有 message_start 的流必须报错")
	}
}

func TestAnthropicStreamAfterStop(t *testing.T) {
	a := NewAnthropicStreamAccumulator(testOpts)
	feedAnth(t, a,
		EventMessageStart, anthMsgStart,
		EventMessageDelta, `{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
		EventMessageStop, `{"type":"message_stop"}`,
	)
	if err := a.Add(Event{Type: EventMessageDelta, Data: []byte(`{"type":"message_delta","delta":{}}`)}); err == nil {
		t.Fatal("message_stop 之后的事件必须被拒绝")
	}
}

// 事件类型以 data 里的 type 为准：event 行可能被中间的代理剥掉，
// 而 data 里的 type 是 Anthropic 自己写进负载的。
func TestAnthropicStreamWorksWithoutEventLine(t *testing.T) {
	a := NewAnthropicStreamAccumulator(testOpts)
	// 全部不带 event 行。
	feedAnth(t, a,
		"", anthMsgStart,
		"", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		"", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
		"", `{"type":"content_block_stop","index":0}`,
		"", `{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
		"", `{"type":"message_stop"}`,
	)
	resp, err := a.Result()
	if err != nil {
		t.Fatalf("组装失败：%v", err)
	}
	if !strings.Contains(string(resp.Content), "ok") {
		t.Errorf("内容丢失：%s", resp.Content)
	}
}

// 渲染成流再累积回来，必须得到同一个响应。
func TestAnthropicStreamRoundTrip(t *testing.T) {
	original := anthResponseWithCall()
	original.Usage = &AnthropicUsage{InputTokens: 100, OutputTokens: 50}

	var buf bytes.Buffer
	if err := EncodeAnthropicStream(NewSSEEncoder(&buf), original); err != nil {
		t.Fatalf("渲染失败：%v", err)
	}

	dec := NewSSEDecoder(&chunkedReader{data: buf.Bytes(), size: 1}, SSEDecoderOptions{})
	acc := NewAnthropicStreamAccumulator(testOpts)
	for {
		ev, err := dec.Next()
		if err != nil {
			break
		}
		if err := acc.Add(*ev); err != nil {
			t.Fatalf("累积失败：%v", err)
		}
	}

	got, err := acc.Result()
	if err != nil {
		t.Fatalf("组装失败：%v\n流内容：%s", err, buf.String())
	}

	if got.ID != original.ID || got.Model != original.Model {
		t.Errorf("顶层字段变化：%+v", got)
	}
	if got.StopReason != original.StopReason {
		t.Errorf("stop_reason 为 %q，期望 %q", got.StopReason, original.StopReason)
	}
	if len(got.ToolCalls) != 1 {
		t.Fatalf("调用数为 %d", len(got.ToolCalls))
	}
	if got.ToolCalls[0].CallID != original.ToolCalls[0].CallID {
		t.Errorf("call id 为 %q", got.ToolCalls[0].CallID)
	}
	if string(got.ToolCalls[0].Arguments) != string(original.ToolCalls[0].Arguments) {
		t.Errorf("参数为 %s，期望 %s", got.ToolCalls[0].Arguments, original.ToolCalls[0].Arguments)
	}
	if !strings.Contains(string(got.Content), "我来查") {
		t.Errorf("文本丢失：%s", got.Content)
	}
	if got.Usage == nil || got.Usage.InputTokens != 100 || got.Usage.OutputTokens != 50 {
		t.Errorf("usage 有误：%+v", got.Usage)
	}
}

// 事件顺序必须严格符合规格：message_start → block 的 start/delta/stop
// → message_delta → message_stop。顺序错了客户端 SDK 会直接报协议错误。
func TestAnthropicStreamEventOrder(t *testing.T) {
	var buf bytes.Buffer
	if err := EncodeAnthropicStream(NewSSEEncoder(&buf), anthResponseWithCall()); err != nil {
		t.Fatalf("渲染失败：%v", err)
	}

	dec := NewSSEDecoder(bytes.NewReader(buf.Bytes()), SSEDecoderOptions{})
	var order []string
	for {
		ev, err := dec.Next()
		if err != nil {
			break
		}
		order = append(order, ev.Type)
	}

	want := []string{
		EventMessageStart,
		EventContentBlockStart, EventContentBlockDelta, EventContentBlockStop, // 文本
		EventContentBlockStart, EventContentBlockDelta, EventContentBlockStop, // 工具
		EventMessageDelta,
		EventMessageStop,
	}
	if len(order) != len(want) {
		t.Fatalf("事件序列为 %v\n期望 %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Errorf("第 %d 个事件为 %q，期望 %q\n完整序列：%v", i, order[i], want[i], order)
		}
	}
}

func TestAnthropicStreamSizeLimit(t *testing.T) {
	opts := testOpts
	opts.MaxBytes = 300

	a := NewAnthropicStreamAccumulator(opts)
	var err error
	for i := 0; i < 100; i++ {
		if err = a.Add(Event{Type: EventMessageStart, Data: []byte(anthMsgStart)}); err != nil {
			break
		}
	}
	if err == nil {
		t.Fatal("超限的流必须被拒绝")
	}
}
