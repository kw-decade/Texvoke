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
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

// TestOpenAIChatModels 钉住模型清单的两件事：端点从 chat/completions 推导到
// 同源的 /models（网关不编名字，答案来自上游），以及响应按 OpenAI 形状解析。
func TestOpenAIChatModels(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("authorization")
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[
			{"id":"model-a","object":"model","created":1,"owned_by":"x"},
			{"id":"model-b","object":"model","created":1,"owned_by":"x"}]}`))
	}))
	defer srv.Close()

	ad := OpenAIChat{Endpoint: srv.URL + "/v1/chat/completions", APIKey: "sk-test"}
	names, err := ad.Models(context.Background())
	if err != nil {
		t.Fatalf("Models 失败：%v", err)
	}
	if gotPath != "/v1/models" {
		t.Fatalf("端点推导错：%q，期望 /v1/models", gotPath)
	}
	if gotAuth != "Bearer sk-test" {
		t.Fatalf("鉴权头 = %q", gotAuth)
	}
	if len(names) != 2 || names[0] != "model-a" || names[1] != "model-b" {
		t.Fatalf("模型名单 = %v", names)
	}
}

// 上游清单不可用必须显式报错，装作有模型会让客户端在第一次请求时才炸。
func TestOpenAIChatModelsUpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"message":"country not supported","type":"policy"}}`))
	}))
	defer srv.Close()

	ad := OpenAIChat{Endpoint: srv.URL}
	if _, err := ad.Models(context.Background()); err == nil {
		t.Fatal("上游清单不可用必须报错")
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
	reply, err := ad.Complete(context.Background(), "m", chatMessages())
	if err == nil {
		t.Fatal("流中断必须报错而不是返回半截文本")
	}
	// 断前已生成的内容不该跟着断流一起消失：调用方（gateway）拿它做降级透传。
	if reply.Text != "半截" {
		t.Fatalf("部分文本丢失：got %q, want %q", reply.Text, "半截")
	}
}

// errAfterReader 先交出全部数据，再报一个传输层错误。
//
// 这是真实断流的形状：字节已经进了客户端缓冲，连接才死。用 io.Reader 而不是
// 真 TCP 断连来模拟，是因为「服务端 close 时客户端缓冲里还剩多少」由内核决定，
// 各平台不一致，测出来的会是脚手架行为而不是产品行为。
type errAfterReader struct {
	r   io.Reader
	err error
}

func (e *errAfterReader) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if err == io.EOF {
		if n > 0 {
			return n, nil
		}
		return 0, e.err
	}
	return n, err
}

// 断流前的部分文本必须跟着错误一起交回来。上游已经生成的内容不该因为
// 最后一个 chunk 丢失就全部作废——调用方拿它做降级透传。
//
// 与上面那条的区别：这里是传输层报错（connection reset），走的是
// dec.Next() 的错误分支；上面是干净 EOF，走 acc.Result() 的未正常结束分支。
// 两条都会丢文本，必须分别钉住。
func TestAccumulateChatStreamKeepsPartialTextOnTransportError(t *testing.T) {
	body := `data: {"id":"c1","choices":[{"index":0,"delta":{"content":"断流前"}}]}` + "\n\n" +
		`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"的半截"}}]}` + "\n\n"
	r := &errAfterReader{r: strings.NewReader(body), err: errors.New("connection reset by peer")}

	text, err := accumulateChatStream(r)
	if err == nil {
		t.Fatal("传输层报错必须显式失败")
	}
	if text != "断流前的半截" {
		t.Fatalf("部分文本丢失：got %q, want %q", text, "断流前的半截")
	}
}
