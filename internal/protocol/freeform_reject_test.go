package protocol

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kw-decade/Texvoke/internal/ir"
)

func freeformCall() ir.ToolCallProposal {
	return ir.ToolCallProposal{
		SessionID: "s", RequestID: "r", CallID: "call_a",
		Tool:         ir.ToolID{Namespace: ir.NamespaceClient, Name: "exec", Version: ir.VersionDeclared},
		Arguments:    ir.TextArguments("ls()"),
		ArgumentForm: ir.InputFormText,
		Source:       ir.SourceVirtual,
		CreatedAt:    time.Now(),
	}
}

// 裸文本参数在 Chat 与 Anthropic 上的处理**分方向**，这是接真实链路时
// 才暴露出来的：
//
//   - 返回客户端：拒绝。客户端会拿 arguments 去 JSON.parse，收到一段脚本
//     就炸了，而且炸在客户端那边，错误信息与真正的原因隔着一整条链路。
//   - 发给上游：允许。上游是纯文本模型，历史里的调用必须原样传达；
//     拒掉它，第二轮开始整条对话直接断——实测就是这么断的。
//
// 一开始两个方向都拒，第二轮请求立刻 502。这条测试钉住的就是那次修正。
func TestFreeformDirectionality(t *testing.T) {
	calls := []ir.ToolCallProposal{freeformCall()}

	t.Run("返回客户端时拒绝", func(t *testing.T) {
		_, err := EncodeChatResponse(ChatResponse{
			ID: "c", Model: "m", Created: 1, FinishReason: FinishToolCalls, ToolCalls: calls,
		})
		assertFreeformError(t, err)

		_, err = EncodeMessagesResponse(MessagesResponse{
			ID: "m1", Model: "m", StopReason: StopToolUse, ToolCalls: calls,
		})
		assertFreeformError(t, err)
	})

	t.Run("发给上游时放行", func(t *testing.T) {
		msgs := []Message{
			{Role: RoleUser, Content: json.RawMessage(`"hi"`)},
			{Role: RoleAssistant, ToolCalls: calls},
		}
		out, err := EncodeChatRequest(ChatRequest{Model: "m", Messages: msgs})
		if err != nil {
			t.Fatalf("请求方向不该拒：%v", err)
		}
		// arguments 是字符串字段，裸文本正好装得下，且不能被双重转义。
		var got struct {
			Messages []struct {
				ToolCalls []struct {
					Function struct {
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatal(err)
		}
		if a := got.Messages[1].ToolCalls[0].Function.Arguments; a != "ls()" {
			t.Errorf("参数被改写了：%q（双重转义会变成 \"\\\"ls()\\\"\"）", a)
		}

		if _, err := EncodeMessagesRequest(MessagesRequest{
			Model: "m", MaxTokens: 100, Messages: msgs,
		}); err != nil {
			t.Errorf("请求方向不该拒：%v", err)
		}
	})
}

func assertFreeformError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("裸文本参数返回给客户端时必须显式报错")
	}
	if !strings.Contains(err.Error(), "裸文本") {
		t.Errorf("错误信息应说清原因：%v", err)
	}
}

// 常规调用两个方向都不受影响。
func TestObjectCallsStillEncode(t *testing.T) {
	c := freeformCall()
	c.ArgumentForm = ir.InputFormObject
	c.Arguments = []byte(`{"a":1}`)
	calls := []ir.ToolCallProposal{c}

	if _, err := encodeToolCalls(calls); err != nil {
		t.Errorf("常规调用不该受影响：%v", err)
	}
	if _, err := encodeAnthropicContent(nil, calls, nil); err != nil {
		t.Errorf("常规调用不该受影响：%v", err)
	}
	if _, err := EncodeChatResponse(ChatResponse{
		ID: "c", Model: "m", Created: 1, FinishReason: FinishToolCalls, ToolCalls: calls,
	}); err != nil {
		t.Errorf("常规调用不该受影响：%v", err)
	}
}
