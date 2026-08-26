package gateway

// 编排循环的单测：mock adapter 直接注入，不碰网络。
//
// 场景全部来自前身实现时代的实战事故：
//   - 一次就成功 → calls_parsed，渲染回客户端协议
//   - 先拒绝后成功 → 阶梯递进、追问消息追加、最终拿到调用
//   - 永远拒绝 → 阶梯封顶停手，如实返回纯文本而不是无限循环
//   - 解析器异常 → 降级纯文本透传，请求不死
//   - 上游瞬态故障 → 静默重试后成功
//   - 上游永久故障 → 不浪费重试

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kw-decade/Texvoke/internal/protocol"
	"github.com/kw-decade/Texvoke/internal/serving"
	"github.com/kw-decade/Texvoke/internal/upstream"
	"github.com/kw-decade/Texvoke/pkg/toolbridge"
)

// mockAdapter 按脚本逐轮返回预设文本。
type mockAdapter struct {
	responses []string
	errs      []error // 每轮的可选错误；nil 表示该轮成功
	calls     atomic.Int32
	lastMsgs  []protocol.Message
}

func (m *mockAdapter) Name() string { return "mock" }

// Models 满足 Adapter 接口；编排循环不查模型清单，测试里给个占位。
func (m *mockAdapter) Models(context.Context) ([]string, error) { return []string{"mock-model"}, nil }

func (m *mockAdapter) Complete(ctx context.Context, model string, messages []protocol.Message) (upstream.Reply, error) {
	i := int(m.calls.Load())
	m.calls.Add(1)
	m.lastMsgs = messages
	if i < len(m.errs) && m.errs[i] != nil {
		return upstream.Reply{}, m.errs[i]
	}
	if i < len(m.responses) {
		return upstream.Reply{Text: m.responses[i], Status: 200}, nil
	}
	return upstream.Reply{}, errors.New("mock: 脚本耗尽")
}

// signalAdapter 从收到的消息里提取本轮真实信号，按脚本行为回应。

func bridge(t *testing.T) *toolbridge.Bridge {
	t.Helper()
	b, err := toolbridge.New(toolbridge.Config{})
	if err != nil {
		t.Fatalf("初始化 Bridge 失败：%v", err)
	}
	return b
}

func chatBody(t *testing.T, tools bool) []byte {
	t.Helper()
	body := map[string]any{
		"model":    "test-model",
		"messages": []map[string]string{{"role": "user", "content": "在 /tmp 建一个 hello.txt"}},
	}
	if tools {
		body["tools"] = []map[string]any{
			{
				"type": "function",
				"function": map[string]any{
					"name":        "write_file",
					"description": "写入文件",
					"parameters": map[string]any{
						"type":       "object",
						"properties": map[string]any{"path": map[string]string{"type": "string"}},
					},
				},
			},
		}
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func gw(t *testing.T, ad upstream.Adapter) *Gateway {
	t.Helper()
	g, err := New(Config{Bridge: bridge(t), Adapter: ad})
	if err != nil {
		t.Fatalf("创建 Gateway 失败：%v", err)
	}
	return g
}

// signalOf 从 mock 收到的消息里提取本轮注入的信号。
// 编译产物会把信号写进末尾提醒；没有工具时（virtual_protocol=false）
// 返回空——那种轮次本来就没有协议。
func signalOf(msgs []protocol.Message) string {
	for _, m := range msgs {
		if m.Role == protocol.RoleUser && strings.Contains(m.Text(), "[[UTR-CALL:") {
			s := m.Text()
			start := strings.Index(s, "[[UTR-CALL:")
			end := strings.Index(s[start:], "]]")
			if end >= 0 {
				return s[start : start+end+2]
			}
		}
	}
	return ""
}

// signalAdapter 从收到的消息里提取本轮真实信号，按脚本行为回应。
// 第 1 轮拒绝（触发阶梯），之后每轮都回「信号 + 正确 envelope」。
type signalAdapter struct {
	refuseFirst *atomic.Value // bool；true 时第 1 轮先拒绝
	calls       atomic.Int32
	lastMsgs    []protocol.Message
}

func (s *signalAdapter) Name() string { return "signal" }

func (s *signalAdapter) Models(context.Context) ([]string, error) { return []string{"mock-model"}, nil }

func (s *signalAdapter) Complete(ctx context.Context, model string, messages []protocol.Message) (upstream.Reply, error) {
	n := s.calls.Add(1)
	s.lastMsgs = messages
	sig := extractSignal(messages)
	if n == 1 && s.refuseFirst != nil {
		v, _ := s.refuseFirst.Load().(bool)
		s.refuseFirst.Store(false)
		if v {
			return upstream.Reply{Text: "我无法直接操作你的文件系统，请你自己在终端执行。", Status: 200}, nil
		}
	}
	if sig == "" {
		// 没有协议注入（无工具轮次）：正常回答。
		return upstream.Reply{Text: "好的，完成了。", Status: 200}, nil
	}
	call := `<tool_call_envelope version="1"><call id="c1"><tool>write_file</tool>` +
		`<arguments_json>{"path":"/tmp/hello.txt"}</arguments_json></call></tool_call_envelope>`
	return upstream.Reply{Text: sig + "\n" + call, Status: 200}, nil
}

// extractSignal 从消息数组里找编译注入的触发信号。
func extractSignal(msgs []protocol.Message) string {
	for _, m := range msgs {
		txt := m.Text()
		if i := strings.Index(txt, "[[UTR-CALL:"); i >= 0 {
			if j := strings.Index(txt[i:], "]]"); j >= 0 {
				return txt[i : i+j+2]
			}
		}
	}
	return ""
}

func TestGatewayDirectSuccess(t *testing.T) {
	ad := &signalAdapter{}
	g := gw(t, ad)

	out, err := g.Handle(context.Background(), "chat", chatBody(t, true))
	if err != nil {
		t.Fatalf("Handle 失败：%v", err)
	}

	var resp struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					Function struct {
						Name string `json:"name"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("响应不是合法 JSON：%v", err)
	}
	if len(resp.Choices) == 0 || len(resp.Choices[0].Message.ToolCalls) == 0 {
		t.Fatalf("期望渲染出 tool_calls，得到 %s", truncate(out))
	}
	if resp.Choices[0].Message.ToolCalls[0].Function.Name != "write_file" {
		t.Fatalf("工具名 = %q", resp.Choices[0].Message.ToolCalls[0].Function.Name)
	}
	if got := ad.calls.Load(); got != 1 {
		t.Fatalf("上游调用次数 = %d，期望 1", got)
	}
}

func TestGatewayLadderRecoversThenSucceeds(t *testing.T) {
	ad := &signalAdapter{refuseFirst: atomicBool(true)}
	g := gw(t, ad)

	out, err := g.Handle(context.Background(), "chat", chatBody(t, true))
	if err != nil {
		t.Fatalf("Handle 失败：%v", err)
	}
	var resp struct {
		Choices []struct {
			Message struct {
				ToolCalls []json.RawMessage `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("响应不是合法 JSON：%v", err)
	}
	if len(resp.Choices) == 0 || len(resp.Choices[0].Message.ToolCalls) == 0 {
		t.Fatalf("追问后应拿到调用，得到 %s", truncate(out))
	}
	if got := ad.calls.Load(); got != 2 {
		t.Fatalf("上游调用次数 = %d，期望 2（拒绝一次后追问成功）", got)
	}
}

func atomicBool(v bool) *atomic.Value {
	var a atomic.Value
	a.Store(v)
	return &a
}

// scriptAdapter 第一轮回拒绝，第二轮回「信号 + 正确 envelope」。

func TestGatewayUpstreamDeadReturnsEmptyPlainText(t *testing.T) {
	ad := &mockAdapter{errs: []error{
		errors.New("upstream: 连接失败：connection refused"),
		errors.New("upstream: 连接失败：connection refused"),
		errors.New("upstream: 连接失败：connection refused"),
		errors.New("upstream: 连接失败：connection refused"),
	}}
	g := gw(t, ad)

	// 无工具请求（virtual_protocol=false）走纯路径，验证降级而非报错。
	out, err := g.Handle(context.Background(), "chat", chatBody(t, false))
	if err != nil {
		t.Fatalf("上游死亡不应杀死请求：%v", err)
	}
	var resp map[string]any
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("响应不是合法 JSON：%v", err)
	}
	if got := ad.calls.Load(); got != maxUpstreamRetries+1 {
		t.Fatalf("重试次数 = %d，期望 %d（含首次）", got, maxUpstreamRetries+1)
	}
}

func TestGatewayPermanentErrorNoRetry(t *testing.T) {
	ad := &mockAdapter{errs: []error{
		fmt.Errorf("upstream: 上游错误 [policy]: content filtered"),
	}}
	g := gw(t, ad)

	_, _ = g.Handle(context.Background(), "chat", chatBody(t, false))
	if got := ad.calls.Load(); got != 1 {
		t.Fatalf("永久错误重试了 %d 次，期望 1 次即止", got)
	}
}

func TestGatewayTransientRetryThenSuccess(t *testing.T) {
	ad := &mockAdapter{
		errs:      []error{errors.New("upstream: 连接失败：connection reset by peer"), nil, nil},
		responses: []string{"", "好的，完成了。"},
	}
	g := gw(t, ad)

	out, err := g.Handle(context.Background(), "chat", chatBody(t, false))
	if err != nil {
		t.Fatalf("Handle 失败：%v", err)
	}
	if !strings.Contains(string(out), "完成") {
		t.Fatalf("重试后的文本未透传：%s", truncate(out))
	}
}

// alwaysRefuseAdapter 永远人格拒绝，验证阶梯封顶停手而不是无限循环。
type alwaysRefuseAdapter struct {
	calls atomic.Int32
}

func (a *alwaysRefuseAdapter) Name() string { return "refuse" }

func (a *alwaysRefuseAdapter) Models(context.Context) ([]string, error) {
	return []string{"mock-model"}, nil
}

func (a *alwaysRefuseAdapter) Complete(ctx context.Context, model string, messages []protocol.Message) (upstream.Reply, error) {
	a.calls.Add(1)
	return upstream.Reply{Text: "抱歉，我没有工具可用，请你自己在终端里执行这个命令。", Status: 200}, nil
}

func TestGatewayLadderExhaustionStops(t *testing.T) {
	ad := &alwaysRefuseAdapter{}
	g, err := New(Config{Bridge: bridge(t), Adapter: ad})
	if err != nil {
		t.Fatal(err)
	}

	out, err := g.Handle(context.Background(), "chat", chatBody(t, true))
	if err != nil {
		t.Fatalf("阶梯用尽不应报错，应如实返回纯文本：%v", err)
	}
	var resp struct {
		Choices []struct {
			Message struct {
				Content   *string           `json:"content"`
				ToolCalls []json.RawMessage `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("响应不是合法 JSON：%v", err)
	}
	if len(resp.Choices) == 0 || len(resp.Choices[0].Message.ToolCalls) != 0 {
		t.Fatalf("不应伪造调用：%s", truncate(out))
	}
	// L1-L4 四次追问 + 封顶后停手。上限受 maxRecoverRounds 兜底约束，
	// 实际次数由阶梯决定：4 级用尽即停。
	if got := ad.calls.Load(); got > serving.MaxLadderLevel+1 {
		t.Fatalf("上游调用 %d 次，超过阶梯应有上限（%d 次内）", got, serving.MaxLadderLevel+1)
	}
	t.Logf("拒绝场景上游共调用 %d 次（L1-L4 追问 + 首轮）", ad.calls.Load())
}

func TestSessionKeyOfStable(t *testing.T) {
	b1 := []byte(`{"messages":[{"role":"user","content":"hi"}],"model":"m"}`)
	b2 := []byte(`{"prompt_cache_key":"abc123","input":[{"type":"message","role":"user"}]}`)
	if k := sessionKeyOf(b1); !strings.HasPrefix(k, "hash:") {
		t.Fatalf("无 pck 时应回退哈希键，得到 %q", k)
	}
	if k := sessionKeyOf(b2); k != "pck:abc123" {
		t.Fatalf("pck 键 = %q", k)
	}
	if sessionKeyOf([]byte(`{`)) != "" {
		t.Fatal("非法 body 应返回空键而不是 panic")
	}
}

// TestGatewayHandleSSEThreeProtocols 钉住流式出口的事件序列形状。
//
// 这条路径此前零覆盖，而它是 Codex / Claude Code 的默认路径——客户端发
// stream:true 就走这里。断言只挑「缺了客户端就会一直等」的必需元素：
// 三协议的终止事件各不相同（Chat 的 [DONE]、Anthropic 的 message_stop、
// Responses 的 response.completed），漏任何一个都是挂住而不是报错。
func TestGatewayHandleSSEThreeProtocols(t *testing.T) {
	for _, tc := range []struct {
		proto string
		must  []string
	}{
		{"chat", []string{"data: ", `"tool_calls"`, `"finish_reason":"tool_calls"`, "data: [DONE]"}},
		{"anthropic", []string{
			"event: " + protocol.EventMessageStart,
			"event: " + protocol.EventContentBlockStart,
			`"tool_use"`,
			"event: " + protocol.EventMessageStop,
		}},
		{"responses", []string{
			"event: " + protocol.EventResponseCreated,
			`"function_call"`,
			"event: " + protocol.EventResponseCompleted,
		}},
	} {
		t.Run(tc.proto, func(t *testing.T) {
			var buf bytes.Buffer
			g := gw(t, &signalAdapter{})
			if err := g.HandleSSE(context.Background(), tc.proto, bodyFor(t, tc.proto, true), &buf); err != nil {
				t.Fatalf("HandleSSE 失败：%v", err)
			}
			out := buf.String()
			for _, want := range tc.must {
				if !strings.Contains(out, want) {
					t.Errorf("事件流缺少 %q\n--- 实际 ---\n%s", want, out)
				}
			}
		})
	}
}

// TestGatewayHandleSSEDegradesCleanly 覆盖流式的两条异常出口。
//
// 上游彻底不可用时，客户端已经收到 200 和 SSE 头，此时唯一正确的行为是
// 发一个完整但空的事件序列——漏掉终止事件就是让客户端永远等下去，
// 那比返回错误更糟。未知协议必须在写出任何字节前失败。
func TestGatewayHandleSSEDegradesCleanly(t *testing.T) {
	t.Run("上游死亡仍产出终止事件", func(t *testing.T) {
		ad := &mockAdapter{errs: []error{
			errors.New("upstream: connection refused"),
			errors.New("upstream: connection refused"),
			errors.New("upstream: connection refused"),
			errors.New("upstream: connection refused"),
		}}
		var buf bytes.Buffer
		g := gw(t, ad)
		if err := g.HandleSSE(context.Background(), "chat", bodyFor(t, "chat", true), &buf); err != nil {
			t.Fatalf("上游死亡不应让流式报错：%v", err)
		}
		if out := buf.String(); !strings.Contains(out, "data: [DONE]") {
			t.Fatalf("缺少终止事件，客户端会挂住：\n%s", out)
		}
	})

	t.Run("未知协议不写半截流", func(t *testing.T) {
		var buf bytes.Buffer
		g := gw(t, &signalAdapter{})
		if err := g.HandleSSE(context.Background(), "grpc", bodyFor(t, "chat", true), &buf); err == nil {
			t.Fatal("未知协议应报错")
		}
		if buf.Len() != 0 {
			t.Fatalf("失败前不该写出任何字节，得到 %q", buf.String())
		}
	})
}

// slowRefuseAdapter 第一轮立刻人格拒绝（触发阶梯追问），之后每轮都卡住
// 直到 ctx 到期——模拟「墙钟预算在救援循环中途耗尽」。
type slowRefuseAdapter struct {
	calls atomic.Int32
}

func (s *slowRefuseAdapter) Name() string { return "slow" }

func (s *slowRefuseAdapter) Models(context.Context) ([]string, error) {
	return []string{"mock-model"}, nil
}

func (s *slowRefuseAdapter) Complete(ctx context.Context, _ string, _ []protocol.Message) (upstream.Reply, error) {
	if s.calls.Add(1) == 1 {
		return upstream.Reply{Text: "我无法访问你的文件系统，请你自己在终端执行。", Status: 200}, nil
	}
	<-ctx.Done()
	return upstream.Reply{}, ctx.Err()
}

// TestGatewayRequestBudget 钉住墙钟预算的两条语义。
//
// 没有预算时最坏情况是 maxRecoverRounds 轮 × 每轮 5 分钟上游超时 × 3 次
// 瞬态重试——几十分钟，而客户端早已放弃。加了预算之后还要保证一件事：
// 预算耗尽时把已经拿到的文本交出去，不是当作彻底失败清空重来。
func TestGatewayRequestBudget(t *testing.T) {
	ad := &slowRefuseAdapter{}
	g, err := New(Config{
		Bridge:        bridge(t),
		Adapter:       ad,
		RequestBudget: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	out, err := g.Handle(context.Background(), "chat", chatBody(t, true))
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("预算耗尽不应报错，应如实降级：%v", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("预算未生效，耗时 %v", elapsed)
	}
	if !strings.Contains(string(out), "无法访问") {
		t.Fatalf("预算耗尽丢掉了首轮已拿到的文本：%s", truncate(out))
	}
	// 卡住的那一轮只该被打一次：ctx 已死时不再走瞬态重试。
	if got := ad.calls.Load(); got != 2 {
		t.Fatalf("上游调用 %d 次，期望 2（首轮拒绝 + 卡住那轮不重试）", got)
	}
}

// partialAdapter 每轮都返回部分文本 + 瞬态错误：模拟断流。
// gateway 应在重试全部失败后把部分文本透传出去，而不是交空响应。
type partialAdapter struct {
	calls atomic.Int32
}

func (p *partialAdapter) Name() string { return "partial" }

func (p *partialAdapter) Models(context.Context) ([]string, error) {
	return []string{"mock-model"}, nil
}

func (p *partialAdapter) Complete(context.Context, string, []protocol.Message) (upstream.Reply, error) {
	p.calls.Add(1)
	return upstream.Reply{Text: "断流前已生成的部分文本", Status: 200},
		errors.New("upstream: connection reset by peer")
}

func TestGatewayPartialTextSurvivesUpstreamFailure(t *testing.T) {
	ad := &partialAdapter{}
	g := gw(t, ad)

	out, err := g.Handle(context.Background(), "chat", chatBody(t, false))
	if err != nil {
		t.Fatalf("断流不应杀死请求：%v", err)
	}
	if !strings.Contains(string(out), "断流前已生成的部分文本") {
		t.Fatalf("部分文本被丢弃了：%s", truncate(out))
	}
	if got := ad.calls.Load(); got != maxUpstreamRetries+1 {
		t.Fatalf("重试次数 = %d，期望 %d", got, maxUpstreamRetries+1)
	}
}

// slowAdapter 先睡一会儿再照 signalAdapter 的脚本回答：模拟真实上游的
// 思考时间，让心跳有机会上场。
type slowAdapter struct {
	delay time.Duration
	inner signalAdapter
}

func (s *slowAdapter) Name() string { return "slow-ok" }

func (s *slowAdapter) Models(context.Context) ([]string, error) { return []string{"mock-model"}, nil }

func (s *slowAdapter) Complete(ctx context.Context, model string, msgs []protocol.Message) (upstream.Reply, error) {
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
		return upstream.Reply{}, ctx.Err()
	}
	return s.inner.Complete(ctx, model, msgs)
}

// TestGatewayHandleSSEHeartbeat 钉住伪流式等待期的保活帧。
//
// 没有它，客户端会在服务端仍在正常工作时先判定连接死掉：伪流式的第一个
// 数据事件要等整轮上游（含阶梯追问）说完，而客户端的空闲阈值通常只有
// 30-60 秒。断言三件事——心跳出现过、它排在第一个数据事件之前、
// 它没有把事件序列本身弄脏（终止事件照常到达）。
func TestGatewayHandleSSEHeartbeat(t *testing.T) {
	g, err := New(Config{
		Bridge:            bridge(t),
		Adapter:           &slowAdapter{delay: 80 * time.Millisecond},
		HeartbeatInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := g.HandleSSE(context.Background(), "chat", bodyFor(t, "chat", true), &buf); err != nil {
		t.Fatalf("HandleSSE 失败：%v", err)
	}
	out := buf.String()

	hb := strings.Index(out, ": texvoke keepalive")
	if hb < 0 {
		t.Fatalf("等待期没有心跳，客户端会先超时断开：\n%s", out)
	}
	if data := strings.Index(out, "data: "); data >= 0 && hb > data {
		t.Fatalf("心跳排在数据事件之后，说明它没有覆盖等待期：\n%s", out)
	}
	if !strings.Contains(out, `"tool_calls"`) || !strings.Contains(out, "data: [DONE]") {
		t.Fatalf("心跳污染了事件序列：\n%s", out)
	}
}

// 心跳可以关掉：负间隔用于「客户端自己有保活」或调试对比。
func TestGatewayHandleSSEHeartbeatDisabled(t *testing.T) {
	g, err := New(Config{
		Bridge:            bridge(t),
		Adapter:           &slowAdapter{delay: 60 * time.Millisecond},
		HeartbeatInterval: -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := g.HandleSSE(context.Background(), "chat", bodyFor(t, "chat", true), &buf); err != nil {
		t.Fatalf("HandleSSE 失败：%v", err)
	}
	if strings.Contains(buf.String(), "keepalive") {
		t.Fatal("负间隔应关闭心跳")
	}
}

// bodyFor 按协议构造带工具的最小请求体。三协议的工具声明形状不同
// （Chat 包一层 function、Anthropic 用 input_schema、Responses 平铺），
// 工具名统一 write_file，signalAdapter 才认得。
func bodyFor(t *testing.T, proto string, stream bool) []byte {
	t.Helper()
	schema := map[string]any{
		"type":       "object",
		"properties": map[string]any{"path": map[string]string{"type": "string"}},
	}
	const ask = "在 /tmp 建一个 hello.txt"
	var body map[string]any
	switch proto {
	case "chat":
		body = map[string]any{
			"model":    "test-model",
			"messages": []map[string]string{{"role": "user", "content": ask}},
			"tools": []map[string]any{{"type": "function", "function": map[string]any{
				"name": "write_file", "description": "写入文件", "parameters": schema}}},
		}
	case "anthropic":
		body = map[string]any{
			"model":      "test-model",
			"max_tokens": 1024,
			"messages":   []map[string]string{{"role": "user", "content": ask}},
			"tools": []map[string]any{{
				"name": "write_file", "description": "写入文件", "input_schema": schema}},
		}
	case "responses":
		body = map[string]any{
			"model": "test-model",
			"input": []map[string]any{{"role": "user", "content": ask}},
			"tools": []map[string]any{{
				"type": "function", "name": "write_file", "description": "写入文件", "parameters": schema}},
		}
	default:
		t.Fatalf("bodyFor: 未知协议 %q", proto)
	}
	if stream {
		body["stream"] = true
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func truncate(b []byte) string {
	s := string(b)
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}
