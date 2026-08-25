package protocol

import (
	"bytes"
	"encoding/json"
	"errors"

	"github.com/kw-decade/Texvoke/internal/ir"
	"strings"
	"testing"
)

func feedResp(t *testing.T, a *ResponsesStreamAccumulator, payloads ...string) {
	t.Helper()
	for i, p := range payloads {
		if err := a.Add(Event{Data: []byte(p)}); err != nil {
			t.Fatalf("第 %d 个事件累积失败：%v\n负载：%s", i, err, p)
		}
	}
}

const respCreated = `{"type":"response.created","response":{"id":"resp_01","object":"response","created_at":1700000000,"status":"in_progress","model":"gpt-4o","output":[]}}`

func respCompleted(output string) string {
	return `{"type":"response.completed","response":{"id":"resp_01","object":"response",` +
		`"created_at":1700000000,"status":"completed","model":"gpt-4o","output":` + output +
		`,"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}`
}

func TestResponsesStreamAccumulatesFragmentedArgs(t *testing.T) {
	fullArgs := `{"city":"San Francisco","unit":"celsius"}`

	for _, size := range []int{1, 3, 9, len(fullArgs)} {
		t.Run("片长="+itoa(size), func(t *testing.T) {
			a := NewResponsesStreamAccumulator(testOpts)
			feedResp(t, a, respCreated,
				`{"type":"response.output_item.added","output_index":0,"item":{"id":"fc_01","type":"function_call","call_id":"call_01","name":"get_weather","arguments":""}}`)

			for i := 0; i < len(fullArgs); i += size {
				end := i + size
				if end > len(fullArgs) {
					end = len(fullArgs)
				}
				frag, err := json.Marshal(fullArgs[i:end])
				if err != nil {
					t.Fatal(err)
				}
				feedResp(t, a, `{"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc_01","delta":`+string(frag)+`}`)
			}

			argsJSON, err := json.Marshal(fullArgs)
			if err != nil {
				t.Fatal(err)
			}
			feedResp(t, a,
				`{"type":"response.function_call_arguments.done","output_index":0,"item_id":"fc_01","arguments":`+string(argsJSON)+`}`,
				`{"type":"response.output_item.done","output_index":0,"item":{"id":"fc_01","type":"function_call","status":"completed","call_id":"call_01","name":"get_weather","arguments":`+string(argsJSON)+`}}`,
				respCompleted(`[{"id":"fc_01","type":"function_call","call_id":"call_01","name":"get_weather","arguments":`+string(argsJSON)+`}]`),
			)

			resp, err := a.Result()
			if err != nil {
				t.Fatalf("组装失败：%v", err)
			}
			if len(resp.ToolCalls) != 1 {
				t.Fatalf("调用数为 %d", len(resp.ToolCalls))
			}
			call := resp.ToolCalls[0]
			// 两个 ID 都必须保留。
			if call.CallID != "call_01" || call.ProtocolItemID != "fc_01" {
				t.Errorf("两个 ID 未同时保留：call=%q item=%q", call.CallID, call.ProtocolItemID)
			}
			if string(call.Arguments) != fullArgs {
				t.Errorf("参数累积为 %s，期望 %s", call.Arguments, fullArgs)
			}
			if resp.Status != ResponseCompleted {
				t.Errorf("状态为 %q", resp.Status)
			}
		})
	}
}

// 规格十章：最终 response.output 必须与流中已发送的 item 对应。
// 不一致意味着上游有缺陷或中间有代理在改写流，静默采信任何一边
// 都会让客户端拿到一份与它看到的增量不符的最终结果。
func TestResponsesStreamDetectsInconsistentFinalOutput(t *testing.T) {
	t.Run("终态里的参数与增量不一致", func(t *testing.T) {
		a := NewResponsesStreamAccumulator(testOpts)
		feedResp(t, a, respCreated,
			`{"type":"response.output_item.added","output_index":0,"item":{"id":"fc_01","type":"function_call","call_id":"call_01","name":"f","arguments":""}}`,
			`{"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc_01","delta":"{\"city\":\"SF\"}"}`)

		err := a.Add(Event{Data: []byte(respCompleted(
			`[{"id":"fc_01","type":"function_call","call_id":"call_01","name":"f","arguments":"{\"city\":\"Tokyo\"}"}]`))})
		if err == nil {
			t.Fatal("终态与增量不一致时必须报错")
		}
		if !strings.Contains(err.Error(), "不一致") {
			t.Errorf("错误信息 %q 未说明原因", err.Error())
		}
	})

	t.Run("done 事件里的参数与增量不一致", func(t *testing.T) {
		a := NewResponsesStreamAccumulator(testOpts)
		feedResp(t, a, respCreated,
			`{"type":"response.output_item.added","output_index":0,"item":{"id":"fc_01","type":"function_call","call_id":"call_01","name":"f","arguments":""}}`,
			`{"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc_01","delta":"{\"a\":1}"}`)

		err := a.Add(Event{Data: []byte(
			`{"type":"response.function_call_arguments.done","output_index":0,"item_id":"fc_01","arguments":"{\"a\":2}"}`)})
		if err == nil {
			t.Fatal("done 事件与增量不一致时必须报错")
		}
	})

	t.Run("call_id 中途变化", func(t *testing.T) {
		a := NewResponsesStreamAccumulator(testOpts)
		feedResp(t, a, respCreated,
			`{"type":"response.output_item.added","output_index":0,"item":{"id":"fc_01","type":"function_call","call_id":"call_a","name":"f","arguments":""}}`)

		err := a.Add(Event{Data: []byte(
			`{"type":"response.output_item.done","output_index":0,"item":{"id":"fc_01","type":"function_call","call_id":"call_b","name":"f","arguments":"{}"}}`)})
		if err == nil {
			t.Fatal("call_id 变化必须报错")
		}
	})
}

func TestResponsesStreamTextItem(t *testing.T) {
	a := NewResponsesStreamAccumulator(testOpts)
	feedResp(t, a, respCreated,
		`{"type":"response.output_item.added","output_index":0,"item":{"id":"msg_01","type":"message","role":"assistant","content":[]}}`,
		`{"type":"response.output_text.delta","output_index":0,"item_id":"msg_01","delta":"我来"}`,
		`{"type":"response.output_text.delta","output_index":0,"item_id":"msg_01","delta":"查一下"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"id":"msg_01","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"我来查一下"}]}}`,
		respCompleted(`[{"id":"msg_01","type":"message","role":"assistant","content":[{"type":"output_text","text":"我来查一下"}]}]`),
	)

	resp, err := a.Result()
	if err != nil {
		t.Fatalf("组装失败：%v", err)
	}
	if resp.MessageItemID != "msg_01" {
		t.Errorf("message item id 为 %q", resp.MessageItemID)
	}
	if !strings.Contains(string(resp.Content), "我来查一下") {
		t.Errorf("文本丢失：%s", resp.Content)
	}
}

// done 事件没给完整 content 时，用逐字累积的文本兜底。
func TestResponsesStreamTextFallback(t *testing.T) {
	a := NewResponsesStreamAccumulator(testOpts)
	feedResp(t, a, respCreated,
		`{"type":"response.output_item.added","output_index":0,"item":{"id":"msg_01","type":"message","role":"assistant","content":[]}}`,
		`{"type":"response.output_text.delta","output_index":0,"item_id":"msg_01","delta":"只有增量"}`,
		respCompleted(`[]`),
	)

	resp, err := a.Result()
	if err != nil {
		t.Fatalf("组装失败：%v", err)
	}
	if !strings.Contains(string(resp.Content), "只有增量") {
		t.Errorf("兜底文本丢失：%s", resp.Content)
	}
	if !strings.Contains(string(resp.Content), "output_text") {
		t.Errorf("兜底应构造成 output_text block：%s", resp.Content)
	}
}

func TestResponsesStreamRejects(t *testing.T) {
	tests := []struct {
		name     string
		payloads []string
		want     string
	}{
		{
			"item added 缺 output_index",
			[]string{respCreated, `{"type":"response.output_item.added","item":{"id":"x","type":"message"}}`},
			"缺少 output_index",
		},
		{
			"增量没有对应的 added",
			[]string{respCreated, `{"type":"response.function_call_arguments.delta","output_index":3,"item_id":"fc_x","delta":"{}"}`},
			"没有对应的 added",
		},
		{
			"同一 index 重复添加",
			[]string{
				respCreated,
				`{"type":"response.output_item.added","output_index":0,"item":{"id":"a","type":"message"}}`,
				`{"type":"response.output_item.added","output_index":0,"item":{"id":"b","type":"message"}}`,
			},
			"重复添加",
		},
		{
			"响应 id 中途变化",
			[]string{respCreated, `{"type":"response.in_progress","response":{"id":"resp_OTHER","status":"in_progress"}}`},
			"中途更换了响应 id",
		},
		{
			"item_id 与 output_index 不匹配",
			[]string{
				respCreated,
				`{"type":"response.output_item.added","output_index":0,"item":{"id":"fc_01","type":"function_call","call_id":"c1","name":"f"}}`,
				`{"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc_OTHER","delta":"{}"}`,
			},
			"item_id 不匹配",
		},
		{
			"增量既无 index 也无 item_id",
			[]string{respCreated, `{"type":"response.function_call_arguments.delta","delta":"{}"}`},
			"无法归位",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := NewResponsesStreamAccumulator(testOpts)
			var err error
			for _, p := range tc.payloads {
				if err = a.Add(Event{Data: []byte(p)}); err != nil {
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

func TestResponsesStreamUpstreamError(t *testing.T) {
	a := NewResponsesStreamAccumulator(testOpts)
	feedResp(t, a, respCreated)

	err := a.Add(Event{Type: "error", Data: []byte(`{"type":"error","code":"rate_limit_exceeded","message":"Slow down"}`)})
	if err == nil {
		t.Fatal("流中的 error 事件必须报错")
	}
	var ue *UpstreamError
	if !errors.As(err, &ue) {
		t.Fatalf("错误类型为 %T，期望 *UpstreamError", err)
	}
	if ue.Code != "rate_limit_exceeded" {
		t.Errorf("错误码丢失：%+v", ue)
	}
}

func TestResponsesStreamTruncated(t *testing.T) {
	a := NewResponsesStreamAccumulator(testOpts)
	feedResp(t, a, respCreated,
		`{"type":"response.output_item.added","output_index":0,"item":{"id":"msg_01","type":"message"}}`,
		`{"type":"response.output_text.delta","output_index":0,"item_id":"msg_01","delta":"半截"}`)

	if _, err := a.Result(); err == nil {
		t.Fatal("未进入终态必须报错")
	} else if !strings.Contains(err.Error(), "未进入终态") {
		t.Errorf("错误信息 %q", err.Error())
	}
}

func TestResponsesStreamIncomplete(t *testing.T) {
	a := NewResponsesStreamAccumulator(testOpts)
	feedResp(t, a, respCreated,
		`{"type":"response.incomplete","response":{"id":"resp_01","status":"incomplete","model":"gpt-4o","output":[],"incomplete_details":{"reason":"max_output_tokens"}}}`)

	resp, err := a.Result()
	if err != nil {
		t.Fatalf("组装失败：%v", err)
	}
	if resp.Status != ResponseIncomplete || resp.IncompleteReason != "max_output_tokens" {
		t.Errorf("终态信息有误：status=%q reason=%q", resp.Status, resp.IncompleteReason)
	}
}

func TestResponsesStreamIgnoresUnknownEvents(t *testing.T) {
	a := NewResponsesStreamAccumulator(testOpts)
	feedResp(t, a,
		respCreated,
		`{"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}`,
		`{"type":"response.some_future_event","payload":1}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"id":"msg_01","type":"message"}}`,
		`{"type":"response.output_text.delta","output_index":0,"item_id":"msg_01","delta":"ok"}`,
		respCompleted(`[]`),
	)

	resp, err := a.Result()
	if err != nil {
		t.Fatalf("组装失败：%v", err)
	}
	if !strings.Contains(string(resp.Content), "ok") {
		t.Errorf("内容丢失：%s", resp.Content)
	}
}

// 渲染成流再累积回来，必须得到同一个响应。渲染器会同时发出增量与完整版，
// 累积器的一致性校验也因此顺带被验证——两边对不上的话这个测试先炸。
func TestResponsesStreamRoundTrip(t *testing.T) {
	original := ResponsesResponse{
		ID:            "resp_01",
		Model:         "gpt-4o",
		CreatedAt:     1700000000,
		Status:        ResponseCompleted,
		Content:       json.RawMessage(`[{"type":"output_text","text":"我来查"}]`),
		MessageItemID: "msg_01",
		ToolCalls: []ir.ToolCallProposal{
			{
				SessionID: "sess-1", RequestID: "req-1",
				CallID: "call_a", ProtocolItemID: "fc_a",
				Tool:      ir.ToolID{Namespace: ir.NamespaceClient, Name: "get_weather", Version: ir.VersionDeclared},
				Arguments: json.RawMessage(`{"city":"SF"}`),
				Source:    ir.SourceNative, CreatedAt: testOpts.Now,
			},
			{
				SessionID: "sess-1", RequestID: "req-1",
				CallID: "call_b", ProtocolItemID: "fc_b",
				Tool:      ir.ToolID{Namespace: ir.NamespaceClient, Name: "get_weather", Version: ir.VersionDeclared},
				Arguments: json.RawMessage(`{"city":"Tokyo"}`),
				Source:    ir.SourceNative, CreatedAt: testOpts.Now,
			},
		},
		Usage: &ResponsesUsage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
	}

	var buf bytes.Buffer
	if err := EncodeResponsesStream(NewSSEEncoder(&buf), original); err != nil {
		t.Fatalf("渲染失败：%v", err)
	}

	dec := NewSSEDecoder(&chunkedReader{data: buf.Bytes(), size: 1}, SSEDecoderOptions{})
	acc := NewResponsesStreamAccumulator(testOpts)
	for {
		ev, err := dec.Next()
		if err != nil {
			break
		}
		if err := acc.Add(*ev); err != nil {
			t.Fatalf("累积失败：%v\n流内容：%s", err, buf.String())
		}
	}

	got, err := acc.Result()
	if err != nil {
		t.Fatalf("组装失败：%v\n流内容：%s", err, buf.String())
	}

	if got.ID != original.ID || got.Model != original.Model || got.Status != original.Status {
		t.Errorf("顶层字段变化：%+v", got)
	}
	if got.MessageItemID != "msg_01" {
		t.Errorf("message item id 为 %q", got.MessageItemID)
	}
	if len(got.ToolCalls) != 2 {
		t.Fatalf("调用数为 %d，期望 2", len(got.ToolCalls))
	}
	// 所有 ID 都必须原样活着。
	for i, want := range []struct{ call, item string }{
		{"call_a", "fc_a"}, {"call_b", "fc_b"},
	} {
		if got.ToolCalls[i].CallID != want.call || got.ToolCalls[i].ProtocolItemID != want.item {
			t.Errorf("第 %d 个调用的 ID 有误：call=%q item=%q，期望 %q / %q",
				i, got.ToolCalls[i].CallID, got.ToolCalls[i].ProtocolItemID, want.call, want.item)
		}
	}
	if string(got.ToolCalls[0].Arguments) != `{"city":"SF"}` {
		t.Errorf("第一个调用参数为 %s", got.ToolCalls[0].Arguments)
	}
	if !strings.Contains(string(got.Content), "我来查") {
		t.Errorf("文本丢失：%s", got.Content)
	}
	if got.Usage == nil || got.Usage.TotalTokens != 15 {
		t.Errorf("usage 丢失：%+v", got.Usage)
	}
}

// 事件序列必须符合 Responses 的既定顺序。
func TestResponsesStreamEventOrder(t *testing.T) {
	r := ResponsesResponse{
		ID: "resp_01", Model: "gpt-4o", Status: ResponseCompleted,
		ToolCalls: []ir.ToolCallProposal{{
			SessionID: "sess-1", RequestID: "req-1",
			CallID: "call_a", ProtocolItemID: "fc_a",
			Tool:      ir.ToolID{Namespace: ir.NamespaceClient, Name: "f", Version: ir.VersionDeclared},
			Arguments: json.RawMessage(`{}`),
			Source:    ir.SourceNative, CreatedAt: testOpts.Now,
		}},
	}

	var buf bytes.Buffer
	if err := EncodeResponsesStream(NewSSEEncoder(&buf), r); err != nil {
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
		EventResponseCreated,
		EventOutputItemAdded,
		EventFunctionArgsDelta,
		EventFunctionArgsDone,
		EventOutputItemDone,
		EventResponseCompleted,
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

func TestResponsesStreamSizeLimit(t *testing.T) {
	opts := testOpts
	opts.MaxBytes = 300

	a := NewResponsesStreamAccumulator(opts)
	var err error
	for i := 0; i < 100; i++ {
		if err = a.Add(Event{Data: []byte(respCreated)}); err != nil {
			break
		}
	}
	if err == nil {
		t.Fatal("超限的流必须被拒绝")
	}
	if !strings.Contains(err.Error(), "超过上限") {
		t.Errorf("错误信息 %q", err.Error())
	}
}
