package protocol

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func decodeResp(t *testing.T, body string) *ResponsesRequest {
	t.Helper()
	req, err := DecodeResponsesRequest([]byte(body), testOpts)
	if err != nil {
		t.Fatalf("解码失败：%v\n请求体：%s", err, body)
	}
	return req
}

func decodeRespErr(t *testing.T, body, want string) {
	t.Helper()
	_, err := DecodeResponsesRequest([]byte(body), testOpts)
	if err == nil {
		t.Fatalf("期望报错包含 %q，却解码成功了\n请求体：%s", want, body)
	}
	if !strings.Contains(err.Error(), want) {
		t.Errorf("错误信息 %q 未包含 %q", err.Error(), want)
	}
}

const respTool = `{"type":"function","name":"get_weather","description":"查询天气","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}`

// Responses 的每个工具调用带两个 ID：item 的 id（fc_xxx）与关联用的
// call_id（call_xxx）。这是三个协议里独有的，也是 IR 必须加
// ProtocolItemID 字段的原因——用一个字段硬扛两种 ID，重新渲染时必然丢一个。
func TestResponsesKeepsBothIDs(t *testing.T) {
	body := `{"model":"gpt-4o","input":[
		{"role":"user","content":"旧金山天气"},
		{"type":"function_call","id":"fc_abc123","call_id":"call_xyz789",
		 "name":"get_weather","arguments":"{\"city\":\"SF\"}"},
		{"type":"function_call_output","call_id":"call_xyz789","output":"18 度"}
	],"tools":[` + respTool + `]}`

	req := decodeResp(t, body)

	if len(req.Input) != 3 {
		t.Fatalf("消息数为 %d，期望 3", len(req.Input))
	}
	call := req.Input[1].ToolCalls[0]
	if call.CallID != "call_xyz789" {
		t.Errorf("call_id 为 %q，期望 call_xyz789", call.CallID)
	}
	if call.ProtocolItemID != "fc_abc123" {
		t.Errorf("item id 为 %q，期望 fc_abc123——丢了它，流式增量就拼不回同一个 item", call.ProtocolItemID)
	}
	// 结果按 call_id 关联，不是按 item id。
	if req.Input[2].ToolResults[0].CallID != "call_xyz789" {
		t.Errorf("结果关联的 id 为 %q", req.Input[2].ToolResults[0].CallID)
	}

	// 两个 ID 都要活着回到线上格式。
	encoded, err := EncodeResponsesRequest(*req)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"call_xyz789"`, `"fc_abc123"`} {
		if !strings.Contains(string(encoded), want) {
			t.Errorf("编码结果中丢失了 %s：%s", want, encoded)
		}
	}
}

// 上游没给 item id 时不能凭空生成一个：客户端会拿到一个它从未见过、
// 也无法与流式事件对上的 ID。
func TestResponsesDoesNotInventItemID(t *testing.T) {
	body := `{"model":"gpt-4o","input":[
		{"type":"function_call","call_id":"call_1","name":"f","arguments":"{}"}
	]}`
	req := decodeResp(t, body)
	if got := req.Input[0].ToolCalls[0].ProtocolItemID; got != "" {
		t.Errorf("item id 应为空，实际为 %q", got)
	}
	encoded, err := EncodeResponsesRequest(*req)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Input []map[string]json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(encoded, &out); err != nil {
		t.Fatal(err)
	}
	if _, ok := out.Input[0]["id"]; ok {
		t.Errorf("不应凭空写出 id 字段：%s", encoded)
	}
}

// instructions 是顶层字段，转成序列首位的 system 消息。
func TestResponsesInstructions(t *testing.T) {
	req := decodeResp(t, `{"model":"m","instructions":"你是助手","input":"hi"}`)
	if len(req.Input) != 2 {
		t.Fatalf("消息数为 %d，期望 2", len(req.Input))
	}
	if req.Input[0].Role != RoleSystem || req.Input[0].Text() != "你是助手" {
		t.Errorf("instructions 未转成 system 消息：%+v", req.Input[0])
	}
	if !req.Input[0].Role.Instructional() {
		t.Error("instructions 内容应被视为指令来源")
	}

	t.Run("只有 instructions 没有 input", func(t *testing.T) {
		decodeRespErr(t, `{"model":"m","instructions":"x"}`, "只有 instructions")
	})
}

// input 可以直接是一段文本，等价于一条 user 消息。
func TestResponsesStringInput(t *testing.T) {
	req := decodeResp(t, `{"model":"m","input":"你好"}`)
	if len(req.Input) != 1 || req.Input[0].Role != RoleUser {
		t.Fatalf("字符串 input 未转成 user 消息：%+v", req.Input)
	}
	if req.Input[0].Text() != "你好" {
		t.Errorf("内容为 %q", req.Input[0].Text())
	}
}

// 分组规则：连续的同类 item 合并成一条 Message，类型一变就开新的。
// 编码时按同样规则展开，因此往返能回到原样。
func TestResponsesItemGrouping(t *testing.T) {
	body := `{"model":"m","input":[
		{"role":"user","content":"两地天气"},
		{"type":"function_call","id":"fc_1","call_id":"call_a","name":"f","arguments":"{\"c\":\"SF\"}"},
		{"type":"function_call","id":"fc_2","call_id":"call_b","name":"f","arguments":"{\"c\":\"TK\"}"},
		{"type":"function_call_output","call_id":"call_a","output":"18"},
		{"type":"function_call_output","call_id":"call_b","output":"24"},
		{"role":"assistant","content":"两地分别是 18 和 24 度。"}
	]}`

	req := decodeResp(t, body)

	if len(req.Input) != 4 {
		t.Fatalf("消息数为 %d，期望 4（user / 两个调用 / 两个结果 / assistant）", len(req.Input))
	}
	if len(req.Input[1].ToolCalls) != 2 {
		t.Errorf("连续的两个 function_call 应合并成一条消息，实际 %d 个", len(req.Input[1].ToolCalls))
	}
	if len(req.Input[2].ToolResults) != 2 {
		t.Errorf("连续的两个 function_call_output 应合并成一条消息，实际 %d 个", len(req.Input[2].ToolResults))
	}
	if req.Input[3].Role != RoleAssistant {
		t.Errorf("末条消息角色为 %q", req.Input[3].Role)
	}

	// 展开后 item 数量与顺序必须与原始一致。
	encoded, err := EncodeResponsesRequest(*req)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Input []struct {
			Type   string `json:"type"`
			CallID string `json:"call_id"`
		} `json:"input"`
	}
	if err := json.Unmarshal(encoded, &out); err != nil {
		t.Fatal(err)
	}
	wantTypes := []string{"message", "function_call", "function_call", "function_call_output", "function_call_output", "message"}
	if len(out.Input) != len(wantTypes) {
		t.Fatalf("展开后 item 数为 %d，期望 %d：%s", len(out.Input), len(wantTypes), encoded)
	}
	for i, want := range wantTypes {
		if out.Input[i].Type != want {
			t.Errorf("第 %d 个 item 类型为 %q，期望 %q", i, out.Input[i].Type, want)
		}
	}
}

// 交错的调用与文本也要保持顺序：[fc, msg, fc] 不能被重排成 [msg, fc, fc]。
func TestResponsesInterleavedOrder(t *testing.T) {
	body := `{"model":"m","input":[
		{"type":"function_call","call_id":"call_a","name":"f","arguments":"{}"},
		{"role":"assistant","content":"中间说了句话"},
		{"type":"function_call","call_id":"call_b","name":"f","arguments":"{}"}
	]}`

	req := decodeResp(t, body)
	if len(req.Input) != 3 {
		t.Fatalf("消息数为 %d，期望 3——中间的文本应当切断分组", len(req.Input))
	}

	encoded, err := EncodeResponsesRequest(*req)
	if err != nil {
		t.Fatal(err)
	}
	posA := strings.Index(string(encoded), "call_a")
	posMsg := strings.Index(string(encoded), "中间说了句话")
	posB := strings.Index(string(encoded), "call_b")
	if !(posA < posMsg && posMsg < posB) {
		t.Errorf("item 顺序被打乱：%s", encoded)
	}
}

func TestResponsesRoundTrip(t *testing.T) {
	original := `{"model":"gpt-4o",
		"instructions":"你是天气助手",
		"input":[
			{"role":"user","content":"两地天气"},
			{"type":"function_call","id":"fc_1","call_id":"call_a","name":"get_weather","arguments":"{\"city\":\"SF\"}"},
			{"type":"function_call","id":"fc_2","call_id":"call_b","name":"get_weather","arguments":"{\"city\":\"Tokyo\"}"},
			{"type":"function_call_output","call_id":"call_a","output":"18 度"},
			{"type":"function_call_output","call_id":"call_b","output":"24 度"},
			{"role":"assistant","content":"分别是 18 和 24 度。"}
		],
		"tools":[` + respTool + `],
		"tool_choice":"auto",
		"parallel_tool_calls":true,
		"max_output_tokens":2048,
		"previous_response_id":"resp_prev123",
		"store":false,
		"temperature":0.4}`

	req := decodeResp(t, original)
	encoded, err := EncodeResponsesRequest(*req)
	if err != nil {
		t.Fatalf("编码失败：%v", err)
	}
	again, err := DecodeResponsesRequest(encoded, testOpts)
	if err != nil {
		t.Fatalf("二次解码失败：%v\n中间结果：%s", err, encoded)
	}

	stripDigests(req.Input)
	stripDigests(again.Input)
	if !reflect.DeepEqual(req, again) {
		t.Errorf("往返改变了语义\n首次：%+v\n再次：%+v", req, again)
	}

	// previous_response_id 是服务端状态的句柄，改写或丢弃会让上游看到
	// 一段与客户端预期不同的历史。
	if again.PreviousResponseID != "resp_prev123" {
		t.Errorf("previous_response_id 丢失：%q", again.PreviousResponseID)
	}
	if again.MaxOutputTokens != 2048 {
		t.Error("max_output_tokens 丢失")
	}
	if again.Store == nil || *again.Store {
		t.Error("store=false 丢失")
	}
	if _, ok := again.Extra["temperature"]; !ok {
		t.Error("未知字段丢失")
	}
	// instructions 统一写回 input，顶层不再出现。
	if strings.Contains(string(encoded), `"instructions"`) {
		t.Errorf("instructions 不应再出现在顶层：%s", encoded)
	}
}

// 请求侧的 reasoning 跳过（见 empty_message_test.go），但跳完一条消息
// 都不剩时仍要报错——那是一个没有对话的请求。
func TestResponsesReasoningOnlyInputIsError(t *testing.T) {
	decodeRespErr(t, `{"model":"m","input":[
		{"type":"reasoning","id":"rs_1","summary":[]}]}`, "没有产出任何消息")
}

// 响应侧仍然拒绝。
//
// 与请求侧的区别是刻意的：请求里的 reasoning 来自客户端的历史，跳过它
// 只是丢掉一段对当前上游无意义的记录；而响应里的 reasoning 是**这一次**
// 上游返回的推理，悄悄吞掉它，客户端会以为模型什么都没想。
//
// 这条路径在本项目里也不该可达：上游是纯文本模型，它不产生 reasoning。
// 真撞上了，说明上游不是我们以为的那个，值得报错。
func TestResponsesResponseRejectsReasoningItem(t *testing.T) {
	_, err := DecodeResponsesResponse([]byte(`{"id":"resp_1","model":"m","status":"completed",
		"output":[{"type":"reasoning","id":"rs_1","summary":[]}]}`), testOpts)
	if err == nil || !strings.Contains(err.Error(), "reasoning") {
		t.Errorf("响应里的 reasoning item 应拒绝，实际：%v", err)
	}
}

func TestResponsesRejectsBuiltinTools(t *testing.T) {
	for _, typ := range []string{"web_search", "file_search", "computer_use_preview"} {
		t.Run(typ, func(t *testing.T) {
			decodeRespErr(t, `{"model":"m","input":"hi","tools":[{"type":"`+typ+`"}]}`,
				"暂只支持 function")
		})
	}
}

func TestResponsesRejectsMalformed(t *testing.T) {
	tests := []struct{ name, body, want string }{
		{"不是 JSON", `nope`, "不是合法的 JSON 对象"},
		{"缺 model", `{"input":"hi"}`, "缺少 model"},
		{"缺 input", `{"model":"m"}`, "缺少 input"},
		{"input 为空数组", `{"model":"m","input":[]}`, "不能为空数组"},
		{"item 无 type 无 role", `{"model":"m","input":[{"content":"x"}]}`, "既没有 type 也没有 role"},
		// 不认识的 item 类型现在会被跳过（客户端会带自己的扩展 item），
		// 但跳完一条消息都不剩时仍要报错。
		{"只有未知 item 类型", `{"model":"m","input":[{"type":"mystery"}]}`, "没有产出任何消息"},
		{"function_call 缺 call_id", `{"model":"m","input":[{"type":"function_call","name":"f","arguments":"{}"}]}`, "缺少 call_id"},
		{"function_call_output 缺 call_id", `{"model":"m","input":[{"type":"function_call_output","output":"x"}]}`, "缺少 call_id"},
		{"call_id 重复", `{"model":"m","input":[
			{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"},
			{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"}]}`, "重复"},
		{"参数不是合法 JSON", `{"model":"m","input":[
			{"type":"function_call","call_id":"c1","name":"f","arguments":"我不能执行"}]}`, "不是 JSON 对象"},
		{"参数写成对象", `{"model":"m","input":[
			{"type":"function_call","call_id":"c1","name":"f","arguments":{"a":1}}]}`, "必须是字符串"},
		{"message item 用了 tool 角色", `{"model":"m","input":[{"role":"tool","content":"x"}]}`, "function_call_output"},
		{"max_output_tokens 为零", `{"model":"m","input":"hi","max_output_tokens":0}`, "必须为正"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			decodeRespErr(t, tc.body, tc.want)
		})
	}
}

func TestDecodeResponsesResponse(t *testing.T) {
	body := `{"id":"resp_01","object":"response","created_at":1700000000,
		"status":"completed","model":"gpt-4o",
		"output":[
			{"type":"message","id":"msg_01","role":"assistant","status":"completed",
			 "content":[{"type":"output_text","text":"我来查"}]},
			{"type":"function_call","id":"fc_01","call_id":"call_01","status":"completed",
			 "name":"get_weather","arguments":"{\"city\":\"SF\"}"}
		],
		"incomplete_details":null,
		"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`

	resp, err := DecodeResponsesResponse([]byte(body), testOpts)
	if err != nil {
		t.Fatalf("解码失败：%v", err)
	}
	if resp.ID != "resp_01" || resp.Status != ResponseCompleted {
		t.Errorf("顶层字段有误：%+v", resp)
	}
	if !resp.Status.Terminal() {
		t.Error("completed 应是终态")
	}
	// message item 的 ID 同样要保留，理由与工具调用的 item id 相同。
	if resp.MessageItemID != "msg_01" {
		t.Errorf("message item id 为 %q", resp.MessageItemID)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("调用数为 %d", len(resp.ToolCalls))
	}
	if resp.ToolCalls[0].CallID != "call_01" || resp.ToolCalls[0].ProtocolItemID != "fc_01" {
		t.Errorf("两个 ID 未同时保留：%+v", resp.ToolCalls[0])
	}
	if !strings.Contains(string(resp.Content), "我来查") {
		t.Errorf("文本内容丢失：%s", resp.Content)
	}
}

func TestResponsesResponseRoundTrip(t *testing.T) {
	original := `{"id":"resp_01","object":"response","created_at":1700000000,
		"status":"completed","model":"gpt-4o",
		"output":[
			{"type":"message","id":"msg_01","role":"assistant","status":"completed",
			 "content":[{"type":"output_text","text":"我来查"}]},
			{"type":"function_call","id":"fc_01","call_id":"call_a","status":"completed",
			 "name":"get_weather","arguments":"{\"city\":\"SF\"}"},
			{"type":"function_call","id":"fc_02","call_id":"call_b","status":"completed",
			 "name":"get_weather","arguments":"{\"city\":\"Tokyo\"}"}
		],
		"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`

	resp, err := DecodeResponsesResponse([]byte(original), testOpts)
	if err != nil {
		t.Fatalf("解码失败：%v", err)
	}
	encoded, err := EncodeResponsesResponse(*resp)
	if err != nil {
		t.Fatalf("编码失败：%v", err)
	}
	again, err := DecodeResponsesResponse(encoded, testOpts)
	if err != nil {
		t.Fatalf("二次解码失败：%v\n中间结果：%s", err, encoded)
	}

	for i := range resp.ToolCalls {
		resp.ToolCalls[i].RawCandidateDigest = ""
	}
	for i := range again.ToolCalls {
		again.ToolCalls[i].RawCandidateDigest = ""
	}
	if !reflect.DeepEqual(resp, again) {
		t.Errorf("往返改变了语义\n首次：%+v\n再次：%+v", resp, again)
	}
	// 所有 ID 都必须原样活着——规格三章把「每次渲染重新生成 ID」
	// 列为必须纠正的问题。
	for _, want := range []string{"resp_01", "msg_01", "fc_01", "fc_02", "call_a", "call_b"} {
		if !strings.Contains(string(encoded), want) {
			t.Errorf("ID %q 在往返中丢失：%s", want, encoded)
		}
	}
}

func TestResponsesResponseRejects(t *testing.T) {
	tests := []struct{ name, body, want string }{
		{"缺 output", `{"id":"r","model":"m","status":"completed"}`, "缺少 output"},
		{"status 未知", `{"id":"r","model":"m","status":"done","output":[]}`, "status 非法"},
		{"缺 id", `{"model":"m","status":"completed","output":[]}`, "缺少 id"},
		{
			"多个 message item",
			`{"id":"r","model":"m","status":"completed","output":[
			  {"type":"message","id":"m1","role":"assistant","content":[]},
			  {"type":"message","id":"m2","role":"assistant","content":[]}]}`,
			"只支持一个",
		},
		{
			"call_id 重复",
			`{"id":"r","model":"m","status":"completed","output":[
			  {"type":"function_call","id":"f1","call_id":"c1","name":"f","arguments":"{}"},
			  {"type":"function_call","id":"f2","call_id":"c1","name":"f","arguments":"{}"}]}`,
			"call_id",
		},
		{
			"item id 重复",
			`{"id":"r","model":"m","status":"completed","output":[
			  {"type":"function_call","id":"f1","call_id":"c1","name":"f","arguments":"{}"},
			  {"type":"function_call","id":"f1","call_id":"c2","name":"f","arguments":"{}"}]}`,
			"item id",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DecodeResponsesResponse([]byte(tc.body), testOpts)
			if err == nil {
				t.Fatalf("期望报错包含 %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误信息 %q 未包含 %q", err.Error(), tc.want)
			}
		})
	}
}

func TestResponseStatusTerminal(t *testing.T) {
	terminal := map[ResponseStatus]bool{
		ResponseCompleted:  true,
		ResponseIncomplete: true,
		ResponseFailed:     true,
		ResponseCancelled:  true,
		ResponseInProgress: false,
		ResponseQueued:     false,
	}
	for s, want := range terminal {
		if got := s.Terminal(); got != want {
			t.Errorf("%q.Terminal() = %v，期望 %v", s, got, want)
		}
	}
	if (ResponseStatus("")).Valid() {
		t.Error("空状态不应有效")
	}
}

func TestResponsesIncompleteReason(t *testing.T) {
	body := `{"id":"r","model":"m","status":"incomplete","output":[],
		"incomplete_details":{"reason":"max_output_tokens"}}`
	resp, err := DecodeResponsesResponse([]byte(body), testOpts)
	if err != nil {
		t.Fatalf("解码失败：%v", err)
	}
	if resp.IncompleteReason != "max_output_tokens" {
		t.Errorf("incomplete_reason 为 %q", resp.IncompleteReason)
	}

	// 状态与原因必须自洽。
	resp.Status = ResponseCompleted
	if _, err := EncodeResponsesResponse(*resp); err == nil {
		t.Error("completed 却带 incomplete_reason，必须报错")
	}
}
