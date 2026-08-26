package protocol

import (
	"strings"
	"testing"
)

// 真实客户端会发内容为空的 system 消息——Codex 经 CC Switch 转协议时就会
// 产生一条。空消息不携带任何信息，丢掉是无损的；为它拒掉整个请求，等于让
// 一段无害的噪声把可用的对话挡在门外。
//
// 这是接真实反代时踩到的：整条链路 502，错误是
// 「protocol: 第 1 条消息非法：protocol: "system" 消息没有任何内容」。
func TestEmptyMessagesAreDroppedNotRejected(t *testing.T) {
	t.Run("chat", func(t *testing.T) {
		req, err := DecodeChatRequest([]byte(`{
			"model": "gpt-4o",
			"messages": [
				{"role": "system", "content": ""},
				{"role": "user", "content": "你好"}
			]
		}`), testOpts)
		if err != nil {
			t.Fatalf("空 system 消息不该让整个请求失败：%v", err)
		}
		if len(req.Messages) != 1 {
			t.Fatalf("应只剩 1 条消息，得到 %d：%+v", len(req.Messages), req.Messages)
		}
		if req.Messages[0].Role != RoleUser {
			t.Errorf("剩下的应是 user 消息，得到 %q", req.Messages[0].Role)
		}
	})

	t.Run("chat 的 content 为 null", func(t *testing.T) {
		req, err := DecodeChatRequest([]byte(`{
			"model": "gpt-4o",
			"messages": [
				{"role": "system", "content": null},
				{"role": "user", "content": "你好"}
			]
		}`), testOpts)
		if err != nil {
			t.Fatalf("content 为 null 的消息不该让请求失败：%v", err)
		}
		if len(req.Messages) != 1 {
			t.Fatalf("应只剩 1 条消息，得到 %d", len(req.Messages))
		}
	})

	t.Run("anthropic", func(t *testing.T) {
		req, err := DecodeMessagesRequest([]byte(`{
			"model": "claude-sonnet-4",
			"max_tokens": 100,
			"messages": [
				{"role": "assistant", "content": []},
				{"role": "user", "content": "你好"}
			]
		}`), testOpts)
		if err != nil {
			t.Fatalf("空 content 数组不该让请求失败：%v", err)
		}
		if len(req.Messages) != 1 {
			t.Fatalf("应只剩 1 条消息，得到 %d：%+v", len(req.Messages), req.Messages)
		}
	})

	// Codex 经 CC Switch 转协议产生的就是这种形状：system 在 messages 里且可能为空。
	t.Run("anthropic 的空 system", func(t *testing.T) {
		req, err := DecodeMessagesRequest([]byte(`{
			"model": "claude-sonnet-4",
			"max_tokens": 100,
			"messages": [
				{"role": "system", "content": ""},
				{"role": "user", "content": "你好"}
			]
		}`), testOpts)
		if err != nil {
			t.Fatalf("空 system 消息不该让请求失败：%v", err)
		}
		if len(req.Messages) != 1 || req.Messages[0].Role != RoleUser {
			t.Fatalf("应只剩那条 user 消息：%+v", req.Messages)
		}
	})

	t.Run("responses", func(t *testing.T) {
		req, err := DecodeResponsesRequest([]byte(`{
			"model": "gpt-4o",
			"input": [
				{"type": "message", "role": "system", "content": ""},
				{"type": "message", "role": "user", "content": "你好"}
			]
		}`), testOpts)
		if err != nil {
			t.Fatalf("空 system 消息不该让整个请求失败：%v", err)
		}
		if len(req.Input) != 1 {
			t.Fatalf("应只剩 1 条消息，得到 %d：%+v", len(req.Input), req.Input)
		}
	})
}

// 客户端会在 input 里带自己的扩展 item。为一个我们不认识的扩展把整轮对话
// 挡在门外，代价远大于收益。
//
// 注意 additional_tools 不在此列——它现在是被认识的，见 codex_tools_test.go。
func TestUnknownInputItemsAreSkipped(t *testing.T) {
	req, err := DecodeResponsesRequest([]byte(`{
		"model": "gpt-4o",
		"input": [
			{"type": "some_client_extension", "payload": {"a": 1}},
			{"type": "message", "role": "user", "content": "你好"}
		]
	}`), testOpts)
	if err != nil {
		t.Fatalf("未知 item 不该让整个请求失败：%v", err)
	}
	if len(req.Input) != 1 || req.Input[0].Role != RoleUser {
		t.Fatalf("应只剩那条 user 消息：%+v", req.Input)
	}
	// 跳过必须留下痕迹——静默丢弃客户端发来的数据是以后最难查的问题。
	if len(req.SkippedItemTypes) != 1 || req.SkippedItemTypes[0] != "some_client_extension" {
		t.Errorf("被跳过的类型没记下来：%v", req.SkippedItemTypes)
	}
}

// 同一种未知类型出现多次只记一条，日志里不该刷屏。
func TestSkippedItemTypesAreDeduped(t *testing.T) {
	req, err := DecodeResponsesRequest([]byte(`{
		"model": "gpt-4o",
		"input": [
			{"type": "mystery"},
			{"type": "mystery"},
			{"type": "other"},
			{"type": "message", "role": "user", "content": "你好"}
		]
	}`), testOpts)
	if err != nil {
		t.Fatalf("不该失败：%v", err)
	}
	if len(req.SkippedItemTypes) != 2 {
		t.Errorf("应去重成 2 种，得到 %v", req.SkippedItemTypes)
	}
}

// reasoning 的 summary 保留、encrypted_content 丢弃，两者分开处置。
//
// 这条断言反过来改过一次，两次都有理由，记下来免得再翻烧饼：
//   - 最早拒绝整个请求。扫了 528 个真实 Codex 会话后改掉——reasoning 是
//     出现次数最高的 item 类型（1787 次，比 message 还多），拒绝它等于宣布
//     这个中间件只能跑第一轮。
//   - 然后是整个 item 跳过，理由「把 summary 变成正文等于替模型改写它没说
//     出口的话」。这个理由对 encrypted_content 成立，对 summary 不成立：
//     summary 是模型公开写给人看的，客户端已经显示在屏幕上了。丢掉它，
//     多轮会话里「上一轮打算怎么做」整段消失。
//
// 所以现在：summary 带标记回灌（标记保证它不冒充公开发言），
// encrypted_content 仍然不出现在任何编码结果里。
func TestReasoningSummaryIsPreservedEncryptedIsNot(t *testing.T) {
	req, err := DecodeResponsesRequest([]byte(`{
		"model": "gpt-4o",
		"input": [
			{"type": "reasoning", "id": "rs_1", "summary": [{"type":"summary_text","text":"我在想"}],
			 "encrypted_content": "xxx"},
			{"type": "message", "role": "user", "content": "你好"}
		]
	}`), testOpts)
	if err != nil {
		t.Fatalf("reasoning item 不该让整轮对话失败：%v", err)
	}
	if len(req.Input) != 2 {
		t.Fatalf("应有摘要消息 + user 消息两条：%+v", req.Input)
	}
	if req.Input[0].Role != RoleAssistant {
		t.Fatalf("摘要应挂在 assistant 名下，得到 %q", req.Input[0].Role)
	}
	if got := req.Input[0].Text(); !strings.Contains(got, "我在想") {
		t.Errorf("摘要内容丢了：%q", got)
	}
	if got := req.Input[0].Text(); !strings.HasPrefix(got, ReasoningSummaryMarker) {
		t.Errorf("摘要缺少标记，会被当成模型的公开发言：%q", got)
	}
	// 这个 item 类型没有被完整表达，观测语义不变。
	if len(req.SkippedItemTypes) != 1 || req.SkippedItemTypes[0] != "reasoning" {
		t.Errorf("reasoning 的跳过记录不该消失：%v", req.SkippedItemTypes)
	}
	encoded, err := EncodeResponsesRequest(*req)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "xxx") {
		t.Errorf("加密载荷被带出去了：%s", encoded)
	}
}

// 只有 encrypted_content 的 reasoning item 没有可表达的内容：整条跳过，
// 不产出一条只剩标记的空消息。
func TestReasoningWithoutSummaryAddsNothing(t *testing.T) {
	req, err := DecodeResponsesRequest([]byte(`{
		"model": "gpt-4o",
		"input": [
			{"type": "reasoning", "id": "rs_1", "encrypted_content": "xxx"},
			{"type": "message", "role": "user", "content": "你好"}
		]
	}`), testOpts)
	if err != nil {
		t.Fatalf("不该失败：%v", err)
	}
	if len(req.Input) != 1 || req.Input[0].Role != RoleUser {
		t.Fatalf("应只剩那条 user 消息：%+v", req.Input)
	}
	if len(req.SkippedItemTypes) != 1 || req.SkippedItemTypes[0] != "reasoning" {
		t.Errorf("跳过的 reasoning 没记下来：%v", req.SkippedItemTypes)
	}
}

// 摘要与紧随其后的 function_call 必须落在同一条 assistant 消息里。
//
// 分成两条会产生连续两个 assistant 消息，也会把「想了什么」和「因此调了
// 什么」在时间线上拆开——回灌历史的全部价值就是这个因果关系。
func TestReasoningSummaryMergesWithFollowingCall(t *testing.T) {
	req, err := DecodeResponsesRequest([]byte(`{
		"model": "gpt-4o",
		"input": [
			{"type": "message", "role": "user", "content": "看一下配置"},
			{"type": "reasoning", "id": "rs_1", "summary": [{"type":"summary_text","text":"先读文件"}]},
			{"type": "function_call", "id": "fc_1", "call_id": "call_1",
			 "name": "read_file", "arguments": "{\"path\":\"/etc/hosts\"}"},
			{"type": "function_call_output", "call_id": "call_1", "output": "127.0.0.1"}
		]
	}`), testOpts)
	if err != nil {
		t.Fatalf("不该失败：%v", err)
	}
	if len(req.Input) != 3 {
		t.Fatalf("应是 user / assistant(摘要+调用) / tool 三条：%+v", req.Input)
	}
	a := req.Input[1]
	if a.Role != RoleAssistant {
		t.Fatalf("第二条应是 assistant，得到 %q", a.Role)
	}
	if !strings.Contains(a.Text(), "先读文件") {
		t.Errorf("摘要没并进来：%q", a.Text())
	}
	if len(a.ToolCalls) != 1 {
		t.Fatalf("调用被拆到别的消息里了：%+v", a)
	}
}

// 丢空消息是为了容忍噪声，不是放宽真正的结构错误。
// 一条连 tool_call_id 都没有的 tool 消息是真的坏了——它关联不到任何调用，
// 静默丢掉会让「工具结果去哪了」变成一个查不出来的问题。
func TestBrokenToolMessageStillRejected(t *testing.T) {
	_, err := DecodeChatRequest([]byte(`{
		"model": "gpt-4o",
		"messages": [
			{"role": "user", "content": "你好"},
			{"role": "tool", "content": "结果"}
		]
	}`), testOpts)
	if err == nil {
		t.Fatal("没有 tool_call_id 的 tool 消息应被拒绝")
	}
}

// 非文本内容不是空内容。只认几种确定的空形态，是为了不把用户发的图片
// 当噪声丢掉——那种消息提不出文字，但绝不是空的。
func TestNonTextContentIsNotEmpty(t *testing.T) {
	req, err := DecodeChatRequest([]byte(`{
		"model": "gpt-4o",
		"messages": [
			{"role": "user", "content": [{"type": "image_url", "image_url": {"url": "data:image/png;base64,AAAA"}}]}
		]
	}`), testOpts)
	if err != nil {
		t.Fatalf("图片消息不该被拒：%v", err)
	}
	if len(req.Messages) != 1 {
		t.Fatalf("图片消息被当成空消息丢掉了：%+v", req.Messages)
	}
}

// 全空的 messages 不该被静默接受成一个没有对话的请求。
func TestAllMessagesEmptyIsStillAnError(t *testing.T) {
	_, err := DecodeChatRequest([]byte(`{
		"model": "gpt-4o",
		"messages": [
			{"role": "system", "content": ""},
			{"role": "user", "content": ""}
		]
	}`), testOpts)
	if err == nil {
		t.Fatal("全部消息都为空时应报错，而不是发一个空对话给上游")
	}
}
