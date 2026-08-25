package protocol

import (
	"encoding/json"
	"testing"
)

// 同一件事在三个协议里有三个名字。少认一个，那一类消息在 Text() 里就
// 静默变成空串——而 Text() 是工具候选排序与历史文本化的取值口。
//
// input_text 是漏得最久的一个：扫 528 个真实 Codex 会话，它出现 8889 次，
// 是最常见的用户内容形态。
func TestTextRecognizesAllThreeNames(t *testing.T) {
	for _, tc := range []struct{ name, content, want string }{
		{"chat 的 text", `[{"type":"text","text":"甲"}]`, "甲"},
		{"responses 的 output_text", `[{"type":"output_text","text":"乙"}]`, "乙"},
		{"responses 的 input_text", `[{"type":"input_text","text":"丙"}]`, "丙"},
		{"纯字符串", `"丁"`, "丁"},
		{"混合", `[{"type":"input_text","text":"戊"},{"type":"text","text":"己"}]`, "戊己"},
		{"非文本不提取", `[{"type":"image_url","image_url":{"url":"x"}}]`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := Message{Role: RoleUser, Content: json.RawMessage(tc.content)}
			if got := m.Text(); got != tc.want {
				t.Errorf("Text() = %q，期望 %q", got, tc.want)
			}
		})
	}
}

// 真实 Codex 的用户消息形态，走完整解码后要能提出原文。
func TestResponsesUserTextIsExtractable(t *testing.T) {
	req, err := DecodeResponsesRequest([]byte(`{"model":"m","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"旧金山天气怎么样"}]}]}`),
		testOpts)
	if err != nil {
		t.Fatal(err)
	}
	if got := req.Input[0].Text(); got != "旧金山天气怎么样" {
		t.Errorf("提不出用户原文，工具候选排序等于没做：%q", got)
	}
}
