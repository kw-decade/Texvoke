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

// reasoning 跳过，但必须留下痕迹。
//
// 一开始这里是拒绝整个请求。扫了 528 个真实 Codex 会话后改掉了：
// reasoning 是出现次数最高的 item 类型（1787 次，比 message 还多），
// 拒绝它等于宣布这个中间件只能跑第一轮。
func TestReasoningItemIsSkippedButVisible(t *testing.T) {
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
	if len(req.Input) != 1 || req.Input[0].Role != RoleUser {
		t.Fatalf("应只剩那条 user 消息：%+v", req.Input)
	}
	// 跳过必须可见。
	if len(req.SkippedItemTypes) != 1 || req.SkippedItemTypes[0] != "reasoning" {
		t.Errorf("跳过的 reasoning 没记下来：%v", req.SkippedItemTypes)
	}
	// 但绝不能把 summary 变成正文——那是真正的改写，
	// 把模型没说出口的东西变成它公开说过的话。
	encoded, err := EncodeResponsesRequest(*req)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "我在想") {
		t.Errorf("隐藏推理内容被转成了正文：%s", encoded)
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
