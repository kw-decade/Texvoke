package main

// HTTP 门面层的测试。编排逻辑在 internal/gateway 有自己的测试网，这里只钉
// 三件门面自己的事：流式与非流式的分派、SSE 头的发出时机、以及「头已发出
// 之后才失败」时错误必须进事件流而不是变成 500。
//
// 第三条是这一层最容易写反的地方：WriteHeader(200) 一旦发出就收不回来，
// 此时再调 writeErr 只会让客户端收到一个 200 里夹着 JSON 错误体的怪东西。

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kw-decade/Texvoke/internal/gateway"
	"github.com/kw-decade/Texvoke/internal/protocol"
	"github.com/kw-decade/Texvoke/internal/upstream"
	"github.com/kw-decade/Texvoke/pkg/toolbridge"
)

// fakeUpstream 是一个永远成功的纯文本上游。
type fakeUpstream struct{ text string }

func (f fakeUpstream) Name() string { return "fake" }

func (f fakeUpstream) Complete(context.Context, string, []protocol.Message) (upstream.Reply, error) {
	return upstream.Reply{Text: f.text, Status: 200}, nil
}

func (f fakeUpstream) Models(context.Context) ([]string, error) {
	return []string{"fake-model"}, nil
}

// failUpstream 的模型清单拉取永远失败，用于钉住 /v1/models 的 502 路径。
type failUpstream struct{ fakeUpstream }

func (f failUpstream) Models(context.Context) ([]string, error) {
	return nil, fmt.Errorf("upstream: 连接失败")
}

func testGateway(t *testing.T) *gateway.Gateway {
	t.Helper()
	br, err := toolbridge.New(toolbridge.Config{})
	if err != nil {
		t.Fatalf("初始化 Bridge 失败：%v", err)
	}
	gw, err := gateway.New(gateway.Config{
		Bridge:  br,
		Adapter: fakeUpstream{text: "好的，已经处理完了。"},
	})
	if err != nil {
		t.Fatalf("创建 Gateway 失败：%v", err)
	}
	return gw
}

func TestHandleDispatch(t *testing.T) {
	log := slog.New(slog.DiscardHandler)

	t.Run("非流式返回 JSON", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
		handle(testGateway(t), log, "chat")(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("状态码 = %d，期望 200；体：%s", rec.Code, rec.Body.String())
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Fatalf("Content-Type = %q", ct)
		}
		if !strings.Contains(rec.Body.String(), "已经处理完了") {
			t.Fatalf("上游文本未透传：%s", rec.Body.String())
		}
	})

	t.Run("流式发 SSE 头并正常终止", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
		handle(testGateway(t), log, "chat")(rec, req)

		if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
			t.Fatalf("Content-Type = %q，客户端会当普通 JSON 解", ct)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
			t.Fatalf("Cache-Control = %q，中间代理可能缓存整条流", cc)
		}
		if !strings.Contains(rec.Body.String(), "data: [DONE]") {
			t.Fatalf("缺少终止标记，客户端会挂住：%s", rec.Body.String())
		}
	})

	t.Run("头已发出后失败必须落进事件流", func(t *testing.T) {
		rec := httptest.NewRecorder()
		// stream 探测成功但协议解码会失败：messages 不是数组。
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"m","stream":true,"messages":"not-an-array"}`))
		handle(testGateway(t), log, "chat")(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("SSE 头已发出，状态码只能是 200，得到 %d", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "event: error") {
			t.Fatalf("错误没有落进事件流：%s", rec.Body.String())
		}
	})
}

func TestIsStreamRequest(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want bool
	}{
		{"chat 显式流式", `{"stream":true,"messages":[]}`, true},
		{"anthropic 显式流式", `{"stream":true,"max_tokens":1,"messages":[]}`, true},
		{"responses 显式流式", `{"stream":true,"input":[]}`, true},
		{"缺字段按非流式", `{"messages":[]}`, false},
		{"显式 false", `{"stream":false}`, false},
		{"非法 JSON 不 panic", `{`, false},
		{"空体", ``, false},
	} {
		if got := isStreamRequest([]byte(tc.body)); got != tc.want {
			t.Errorf("%s：isStreamRequest = %v，期望 %v", tc.name, got, tc.want)
		}
	}
}

func TestBuildAdapter(t *testing.T) {
	for _, format := range []string{"openai-chat", "OpenAI-Chat"} {
		if _, err := buildAdapter(format, "https://example.invalid/v1", ""); err != nil {
			t.Errorf("format %q 应被接受：%v", format, err)
		}
	}
	if _, err := buildAdapter("gemini-native", "https://example.invalid", ""); err == nil {
		t.Fatal("未实现的 format 必须报错，不能静默降级成默认值")
	}
}

func TestSplitList(t *testing.T) {
	got := splitList(" Bash , Read ,, Write ")
	want := []string{"Bash", "Read", "Write"}
	if len(got) != len(want) {
		t.Fatalf("splitList = %#v，期望 %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("splitList[%d] = %q，期望 %q", i, got[i], want[i])
		}
	}
	if splitList("") != nil {
		t.Fatal("空串应返回 nil 而不是空切片——nil 才让 CompileOptions 走默认路径")
	}
}

// TestListModelsShape 钉住 /v1/models 的响应形状。
//
// 这条有实战出处：CC Switch 加供应商时提示「该供应商不支持获取模型列表」，
// 原因是 data 元素给了裸字符串而不是完整对象，且缺 CORS 头——Electron 渲染
// 进程发的请求在浏览器层就被拦了，服务端日志一片干净。三个形状约束一个都
// 不能少，退化成任何一个，坏的方式都是在客户端静默失败。
func TestListModelsShape(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	listModels(fakeUpstream{}, log)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("缺 CORS 头，Electron 客户端会在浏览器层被拦：%q", got)
	}
	var body struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			Created int64  `json:"created"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON：%v", err)
	}
	if len(body.Data) != 1 || body.Data[0].ID != "fake-model" {
		t.Fatalf("data 应为完整对象数组：%+v", body.Data)
	}
	if body.Data[0].Object != "model" || body.Data[0].OwnedBy == "" {
		t.Fatalf("data 元素缺字段，按 m.id 之外的字段取值的客户端会静默失败：%+v", body.Data[0])
	}
}

// TestListModelsUpstreamFailure 上游清单不可用时回 502。
// 装作有模型会让客户端在第一次请求时才炸，不如现在就说清楚。
func TestListModelsUpstreamFailure(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	listModels(failUpstream{}, log)(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("上游不可用应回 502，得到 %d", rec.Code)
	}
}
