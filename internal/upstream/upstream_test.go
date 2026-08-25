package upstream

// 两个适配器的单测：httptest 起本地假上游，验证请求形状与响应还原。
//
// 关键场景来自前身实现时代的实战事故，不是想象出来的：
//   - SSE 增量按任意边界切碎后仍能正确拼接（这类上游实测 8-9 字节一块）
//   - 错误内嵌在 200 的流里（自有方言）与 HTTP 非 200（标准方言）
//   - 流中途断掉要显式报错，不能把半截文本当好答案

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kw-decade/Texvoke/internal/protocol"
)

func chatMessages() []protocol.Message {
	return []protocol.Message{
		{Role: protocol.RoleSystem, Content: json.RawMessage(`"你是命令行 agent"`)},
		{Role: protocol.RoleUser, Content: json.RawMessage(`"列出当前目录"`)},
	}
}

/* ---------- openai-chat ---------- */

func TestOpenAIChatComplete(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("authorization")
		body := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)
		_ = json.Unmarshal(body, &gotBody)

		w.Header().Set("content-type", "text/event-stream")
		f := w.(http.Flusher)
		// 故意按小块切，模拟真实上游的碎片增量。
		chunks := []string{
			`data: {"id":"c1","model":"gpt-x","created":1,"choices":[{"index":0,"delta":{"role":"assistant"}}]}` + "\n\n",
			`data: {"id":"c1","model":"gpt-x","choices":[{"index":0,"delta":{"content":"你"}}]}` + "\n\n",
			`data: {"id":"c1","model":"gpt-x","choices":[{"index":0,"delta":{"content":"好"}}]}` + "\n\n",
			`data: {"id":"c1","model":"gpt-x","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n",
			"data: [DONE]\n\n",
		}
		for _, c := range chunks {
			_, _ = w.Write([]byte(c))
			f.Flush()
		}
	}))
	defer srv.Close()

	ad := OpenAIChat{Endpoint: srv.URL + "/v1/chat/completions", APIKey: "sk-test"}
	rep, err := ad.Complete(context.Background(), "gpt-x", chatMessages())
	if err != nil {
		t.Fatalf("Complete 失败：%v", err)
	}
	if rep.Text != "你好" {
		t.Fatalf("拼出的文本 = %q，期望 %q", rep.Text, "你好")
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("请求路径 = %q", gotPath)
	}
	if gotAuth != "Bearer sk-test" {
		t.Fatalf("鉴权头 = %q", gotAuth)
	}
	if gotBody["stream"] == true {
		t.Fatal("伪流式架构下不应向上游声明 stream=true")
	}
	msgs, _ := gotBody["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("消息数 = %d，期望 2", len(msgs))
	}
}

func TestOpenAIChatUpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"message":"country not supported","type":"policy"}}`))
	}))
	defer srv.Close()

	ad := OpenAIChat{Endpoint: srv.URL}
	_, err := ad.Complete(context.Background(), "m", chatMessages())
	var ue *protocol.UpstreamError
	if !errors.As(err, &ue) {
		t.Fatalf("期望 UpstreamError，得到 %T：%v", err, err)
	}
	if ue.Status != http.StatusForbidden || ue.Message != "country not supported" {
		t.Fatalf("错误内容不符：status=%d message=%q", ue.Status, ue.Message)
	}
}

func TestOpenAIChatTruncatedStreamFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 既无 [DONE] 也无 finish_reason：流断在中途。
		_, _ = w.Write([]byte(`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"半截"}}]}` + "\n\n"))
	}))
	defer srv.Close()

	ad := OpenAIChat{Endpoint: srv.URL}
	_, err := ad.Complete(context.Background(), "m", chatMessages())
	if err == nil {
		t.Fatal("流中断必须报错而不是返回半截文本")
	}
}
