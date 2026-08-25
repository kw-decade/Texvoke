package protocol

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// feed 把一串 SSE data 负载依次喂给累积器。
func feed(t *testing.T, a *ChatStreamAccumulator, payloads ...string) {
	t.Helper()
	for i, p := range payloads {
		if err := a.Add(Event{Data: []byte(p)}); err != nil {
			t.Fatalf("第 %d 个事件累积失败：%v\n负载：%s", i, err, p)
		}
	}
}

// chatChunk 构造一个 chat.completion.chunk 负载。
func chatChunk(delta string, finish string) string {
	f := "null"
	if finish != "" {
		f = `"` + finish + `"`
	}
	return `{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1700000000,` +
		`"model":"gpt-4o","choices":[{"index":0,"delta":` + delta + `,"finish_reason":` + f + `}]}`
}

// 参数以任意边界到达，累积器必须能还原。逐字符切分是最极端的情形——
// 上游实测约 8-9 个字符一块，但没有任何协议保证切分点。
func TestChatStreamAccumulatesFragmentedArguments(t *testing.T) {
	fullArgs := `{"city":"San Francisco","unit":"celsius"}`

	for _, size := range []int{1, 2, 3, 7, 13, len(fullArgs)} {
		t.Run("片长="+itoa(size), func(t *testing.T) {
			a := NewChatStreamAccumulator(testOpts)

			feed(t, a, chatChunk(`{"role":"assistant"}`, ""))
			// 首片带 id、type 与工具名，后续片只带参数——这是 OpenAI 的既定形态。
			feed(t, a, chatChunk(`{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":""}}]}`, ""))

			for i := 0; i < len(fullArgs); i += size {
				end := i + size
				if end > len(fullArgs) {
					end = len(fullArgs)
				}
				frag, err := json.Marshal(fullArgs[i:end])
				if err != nil {
					t.Fatal(err)
				}
				feed(t, a, chatChunk(`{"tool_calls":[{"index":0,"function":{"arguments":`+string(frag)+`}}]}`, ""))
			}

			feed(t, a, chatChunk(`{}`, "tool_calls"), StreamDoneMarker)

			resp, err := a.Result()
			if err != nil {
				t.Fatalf("组装失败：%v", err)
			}
			if len(resp.ToolCalls) != 1 {
				t.Fatalf("调用数为 %d，期望 1", len(resp.ToolCalls))
			}
			call := resp.ToolCalls[0]
			if call.CallID != "call_1" || call.Tool.Name != "get_weather" {
				t.Errorf("调用标识有误：%+v", call)
			}
			if string(call.Arguments) != fullArgs {
				t.Errorf("参数累积为 %s，期望 %s", call.Arguments, fullArgs)
			}
			if resp.FinishReason != FinishToolCalls {
				t.Errorf("finish_reason 为 %q", resp.FinishReason)
			}
		})
	}
}

// 并行调用的增量是交错到达的。按出现顺序猜会把两次调用的参数拼到一起，
// 所以必须按 index 分组。
func TestChatStreamInterleavedParallelCalls(t *testing.T) {
	a := NewChatStreamAccumulator(testOpts)

	feed(t, a,
		chatChunk(`{"role":"assistant"}`, ""),
		chatChunk(`{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"get_weather","arguments":""}}]}`, ""),
		chatChunk(`{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"get_weather","arguments":""}}]}`, ""),
		// 交错：0 的一片、1 的一片、0 的一片、1 的一片。
		chatChunk(`{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]}`, ""),
		chatChunk(`{"tool_calls":[{"index":1,"function":{"arguments":"{\"city\":"}}]}`, ""),
		chatChunk(`{"tool_calls":[{"index":0,"function":{"arguments":"\"SF\"}"}}]}`, ""),
		chatChunk(`{"tool_calls":[{"index":1,"function":{"arguments":"\"Tokyo\"}"}}]}`, ""),
		chatChunk(`{}`, "tool_calls"),
		StreamDoneMarker,
	)

	resp, err := a.Result()
	if err != nil {
		t.Fatalf("组装失败：%v", err)
	}
	if len(resp.ToolCalls) != 2 {
		t.Fatalf("调用数为 %d，期望 2", len(resp.ToolCalls))
	}
	if got := string(resp.ToolCalls[0].Arguments); got != `{"city":"SF"}` {
		t.Errorf("index 0 的参数为 %s——交错的增量被拼错了", got)
	}
	if got := string(resp.ToolCalls[1].Arguments); got != `{"city":"Tokyo"}` {
		t.Errorf("index 1 的参数为 %s", got)
	}
	if resp.ToolCalls[0].CallID != "call_a" || resp.ToolCalls[1].CallID != "call_b" {
		t.Errorf("调用顺序错乱：%q %q", resp.ToolCalls[0].CallID, resp.ToolCalls[1].CallID)
	}
	// 两次调用参数不同，幂等键必须不同。
	if resp.ToolCalls[0].IdempotencyKey() == resp.ToolCalls[1].IdempotencyKey() {
		t.Error("两次不同的调用算出了相同的幂等键")
	}
}

// index 乱序到达时，还原出的顺序应当由 index 决定，而不是到达顺序。
func TestChatStreamOrdersByIndex(t *testing.T) {
	a := NewChatStreamAccumulator(testOpts)
	feed(t, a,
		// 先到的是 index 1。
		chatChunk(`{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"f","arguments":"{}"}}]}`, ""),
		chatChunk(`{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"f","arguments":"{}"}}]}`, ""),
		chatChunk(`{}`, "tool_calls"),
		StreamDoneMarker,
	)

	resp, err := a.Result()
	if err != nil {
		t.Fatalf("组装失败：%v", err)
	}
	if resp.ToolCalls[0].CallID != "call_a" || resp.ToolCalls[1].CallID != "call_b" {
		t.Errorf("应按 index 升序还原，实际为 %q %q", resp.ToolCalls[0].CallID, resp.ToolCalls[1].CallID)
	}
}

func TestChatStreamAccumulatesText(t *testing.T) {
	a := NewChatStreamAccumulator(testOpts)
	feed(t, a,
		chatChunk(`{"role":"assistant","content":""}`, ""),
		chatChunk(`{"content":"旧金"}`, ""),
		chatChunk(`{"content":"山现在"}`, ""),
		chatChunk(`{"content":"18 度。"}`, ""),
		chatChunk(`{}`, "stop"),
		StreamDoneMarker,
	)

	resp, err := a.Result()
	if err != nil {
		t.Fatalf("组装失败：%v", err)
	}
	var s string
	if err := json.Unmarshal(resp.Content, &s); err != nil {
		t.Fatalf("content 不是字符串：%s", resp.Content)
	}
	if s != "旧金山现在18 度。" {
		t.Errorf("文本累积为 %q", s)
	}
	if resp.FinishReason != FinishStop {
		t.Errorf("finish_reason 为 %q", resp.FinishReason)
	}
}

func TestChatStreamRejects(t *testing.T) {
	tests := []struct {
		name     string
		payloads []string
		want     string
	}{
		{
			// 没有 index 就无法判断这段参数属于哪次调用，按出现顺序猜
			// 会把并行调用的参数拼到一起。
			"增量缺少 index",
			[]string{chatChunk(`{"tool_calls":[{"id":"c1","function":{"arguments":"{}"}}]}`, "")},
			"缺少 index",
		},
		{
			"chunk 不是合法 JSON",
			[]string{`{"id":`},
			"不是合法的 chunk",
		},
		{
			"多个 choice",
			[]string{`{"id":"c","choices":[{"index":0,"delta":{}},{"index":1,"delta":{}}]}`},
			"多候选语义不明确",
		},
		{
			"响应 id 中途变化",
			[]string{
				chatChunk(`{"role":"assistant"}`, ""),
				`{"id":"chatcmpl-OTHER","choices":[{"index":0,"delta":{"content":"x"}}]}`,
			},
			"中途更换了响应 id",
		},
		{
			"同一 index 的调用 id 变化",
			[]string{
				chatChunk(`{"tool_calls":[{"index":0,"id":"call_a","function":{"name":"f","arguments":""}}]}`, ""),
				chatChunk(`{"tool_calls":[{"index":0,"id":"call_b","function":{"arguments":"{}"}}]}`, ""),
			},
			"id 中途变化",
		},
		{
			"非 function 类型",
			[]string{chatChunk(`{"tool_calls":[{"index":0,"type":"custom","function":{"arguments":"{}"}}]}`, "")},
			"只支持 function",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := NewChatStreamAccumulator(testOpts)
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

// 参数累积完成后仍不是合法 JSON，必须显式报错。绝不生成一个「能用」的
// 兜底提案——那等于让一段没被理解的文本进入执行路径。
func TestChatStreamRejectsMalformedAccumulatedArguments(t *testing.T) {
	a := NewChatStreamAccumulator(testOpts)
	feed(t, a,
		chatChunk(`{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"f","arguments":"{\"a\":"}}]}`, ""),
		chatChunk(`{}`, "tool_calls"),
		StreamDoneMarker,
	)

	resp, err := a.Result()
	if err == nil {
		t.Fatal("残缺的参数必须报错")
	}
	if resp != nil {
		t.Error("报错时不得同时返回可用的响应")
	}
	if !strings.Contains(err.Error(), "不是 JSON 对象") {
		t.Errorf("错误信息 %q 未说明原因", err.Error())
	}
}

// 流断在中途与正常结束是两回事：前者的内容可能只有一半，
// 当成完整响应用会静默丢数据。
func TestChatStreamRejectsTruncatedStream(t *testing.T) {
	a := NewChatStreamAccumulator(testOpts)
	feed(t, a,
		chatChunk(`{"role":"assistant"}`, ""),
		chatChunk(`{"content":"说到一半"}`, ""),
	)

	if _, err := a.Result(); err == nil {
		t.Fatal("未收到 [DONE] 也没有 finish_reason，必须报错")
	} else if !strings.Contains(err.Error(), "未正常结束") {
		t.Errorf("错误信息 %q 未说明是截断", err.Error())
	}
}

func TestChatStreamEmptyStream(t *testing.T) {
	a := NewChatStreamAccumulator(testOpts)
	if _, err := a.Result(); err == nil {
		t.Fatal("空流必须报错")
	} else if !strings.Contains(err.Error(), "没有任何 chunk") {
		t.Errorf("错误信息 %q", err.Error())
	}
}

func TestChatStreamAfterDone(t *testing.T) {
	a := NewChatStreamAccumulator(testOpts)
	feed(t, a, chatChunk(`{}`, "stop"), StreamDoneMarker)

	if err := a.Add(Event{Data: []byte(chatChunk(`{"content":"多余"}`, ""))}); err == nil {
		t.Fatal("[DONE] 之后的事件必须被拒绝")
	}
	if !a.Done() {
		t.Error("Done 应为 true")
	}
}

// 心跳与空事件不携带内容，跳过而不是报错——它们证明连接还活着。
func TestChatStreamIgnoresHeartbeats(t *testing.T) {
	a := NewChatStreamAccumulator(testOpts)

	for _, ev := range []Event{
		{Comment: "ping"},
		{Data: nil},
		{Data: []byte("  ")},
	} {
		if err := a.Add(ev); err != nil {
			t.Errorf("心跳事件不应报错：%v", err)
		}
	}
	feed(t, a, chatChunk(`{"content":"x"}`, "stop"), StreamDoneMarker)
	if _, err := a.Result(); err != nil {
		t.Errorf("组装失败：%v", err)
	}
}

// 只带 usage 而没有 choices 的尾包是合法的（stream_options.include_usage）。
func TestChatStreamUsageOnlyChunk(t *testing.T) {
	a := NewChatStreamAccumulator(testOpts)
	feed(t, a,
		chatChunk(`{"content":"x"}`, "stop"),
		`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1700000000,"model":"gpt-4o","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
		StreamDoneMarker,
	)

	resp, err := a.Result()
	if err != nil {
		t.Fatalf("组装失败：%v", err)
	}
	if resp.Usage == nil || resp.Usage.TotalTokens != 15 {
		t.Errorf("usage 丢失：%+v", resp.Usage)
	}
}

func TestChatStreamSizeLimit(t *testing.T) {
	opts := testOpts
	opts.MaxBytes = 200

	a := NewChatStreamAccumulator(opts)
	var err error
	for i := 0; i < 100; i++ {
		if err = a.Add(Event{Data: []byte(chatChunk(`{"content":"填充填充填充"}`, ""))}); err != nil {
			break
		}
	}
	if err == nil {
		t.Fatal("超限的流必须被拒绝")
	}
	if !strings.Contains(err.Error(), "超过上限") {
		t.Errorf("错误信息 %q 未说明是超限", err.Error())
	}
}

// 渲染成流再累积回来，必须得到同一个响应。这是 Bridge 在虚拟协议模式下
// 模拟流式输出的正确性基础。
func TestChatStreamRoundTrip(t *testing.T) {
	original := responseWithCall()
	original.Content = json.RawMessage(`"我来查一下"`)
	original.Usage = &Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}

	var buf bytes.Buffer
	if err := EncodeChatStream(NewSSEEncoder(&buf), original); err != nil {
		t.Fatalf("渲染失败：%v", err)
	}

	// 逐字节读回，顺带验证整条链路不依赖读取边界。
	dec := NewSSEDecoder(&chunkedReader{data: buf.Bytes(), size: 1}, SSEDecoderOptions{})
	acc := NewChatStreamAccumulator(testOpts)
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
	if got.FinishReason != original.FinishReason {
		t.Errorf("finish_reason 为 %q，期望 %q", got.FinishReason, original.FinishReason)
	}
	if len(got.ToolCalls) != len(original.ToolCalls) {
		t.Fatalf("调用数为 %d，期望 %d", len(got.ToolCalls), len(original.ToolCalls))
	}
	for i := range original.ToolCalls {
		if got.ToolCalls[i].CallID != original.ToolCalls[i].CallID {
			t.Errorf("第 %d 个调用 id 为 %q", i, got.ToolCalls[i].CallID)
		}
		if string(got.ToolCalls[i].Arguments) != string(original.ToolCalls[i].Arguments) {
			t.Errorf("第 %d 个调用参数为 %s，期望 %s", i, got.ToolCalls[i].Arguments, original.ToolCalls[i].Arguments)
		}
	}
	var gotText string
	if err := json.Unmarshal(got.Content, &gotText); err != nil {
		t.Fatalf("content 不是字符串：%s", got.Content)
	}
	if gotText != "我来查一下" {
		t.Errorf("文本为 %q", gotText)
	}
	if got.Usage == nil || got.Usage.TotalTokens != 15 {
		t.Errorf("usage 丢失：%+v", got.Usage)
	}
}

// 渲染出的流必须符合 Chat Completions 的既定形态，否则官方 SDK 消费不了。
func TestChatStreamEncodedShape(t *testing.T) {
	var buf bytes.Buffer
	if err := EncodeChatStream(NewSSEEncoder(&buf), responseWithCall()); err != nil {
		t.Fatalf("渲染失败：%v", err)
	}
	out := buf.String()

	if !strings.HasSuffix(out, "data: "+StreamDoneMarker+"\n\n") {
		t.Errorf("流必须以 %s 结尾：%q", StreamDoneMarker, out)
	}
	if !strings.Contains(out, `"object":"chat.completion.chunk"`) {
		t.Errorf("缺少 chunk 对象标记：%s", out)
	}
	if !strings.Contains(out, `"role":"assistant"`) {
		t.Errorf("首包应只带 role：%s", out)
	}
	// 工具调用的增量必须带 index，客户端靠它累积。
	if !strings.Contains(out, `"index":0`) {
		t.Errorf("工具调用增量缺少 index：%s", out)
	}
	if !strings.Contains(out, `"finish_reason":"tool_calls"`) {
		t.Errorf("缺少终止原因：%s", out)
	}
}
