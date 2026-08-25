package serving

import "testing"

// 三协议的历史调用形态各不相同，任何一种漏判都会让 L2 的运行时通知失去
// 「你明明成功过」这条最有力的反驳。
func TestHasToolCallInBody(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want bool
	}{
		{"Chat 的 assistant.tool_calls", `{"messages":[
			{"role":"user","content":"hi"},
			{"role":"assistant","tool_calls":[{"id":"c1","type":"function"}]}]}`, true},
		{"Anthropic 的 tool_use 块", `{"messages":[
			{"role":"assistant","content":[{"type":"text","text":"好"},{"type":"tool_use","id":"t1"}]}]}`, true},
		{"Responses 的 function_call item", `{"input":[
			{"type":"message","role":"user"},{"type":"function_call","name":"f"}]}`, true},
		{"Responses 的 custom_tool_call", `{"input":[{"type":"custom_tool_call","name":"exec"}]}`, true},
		{"Codex 的 local_shell_call", `{"input":[{"type":"local_shell_call"}]}`, true},

		{"纯对话无调用", `{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"你好"}]}`, false},
		{"user 消息里的 tool_result 不算", `{"messages":[
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1"}]}]}`, false},
		{"空 tool_calls 不算", `{"messages":[{"role":"assistant","tool_calls":[]}]}`, false},
		{"非法 JSON 不 panic", `{`, false},
		{"空体", ``, false},
	} {
		if got := HasToolCallInBody([]byte(tc.body)); got != tc.want {
			t.Errorf("%s：得到 %v，期望 %v", tc.name, got, tc.want)
		}
	}
}
