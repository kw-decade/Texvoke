package main

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kw-decade/Texvoke/pkg/toolbridge"
)

/* ---------- 夹具 ---------- */

// 三种协议下「带一个工具的一句话请求」，形状各按各的规矩。
func protocolBodies() map[string]string {
	return map[string]string{
		"chat": `{
			"model": "gpt-4o",
			"messages": [{"role": "user", "content": "北京天气怎么样"}],
			"tools": [{"type": "function", "function": {
				"name": "get_weather", "description": "查询天气",
				"parameters": {"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}
			}}]
		}`,
		"anthropic": `{
			"model": "claude-sonnet-4",
			"max_tokens": 1024,
			"messages": [{"role": "user", "content": "北京天气怎么样"}],
			"tools": [{"name": "get_weather", "description": "查询天气",
				"input_schema": {"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}]
		}`,
		"responses": `{
			"model": "gpt-4o",
			"input": [{"role": "user", "content": "北京天气怎么样"}],
			"tools": [{"type": "function", "name": "get_weather", "description": "查询天气",
				"parameters": {"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}]
		}`,
	}
}

func adaptOnce(t *testing.T, s *server, proto, body string) adaptResponse {
	t.Helper()
	rec := post(t, s.handleAdapt, "/v1/adapt", adaptRequest{
		Protocol: proto, SessionID: "s1", RequestID: "r1",
		Body: json.RawMessage(body),
	})
	if rec.Code != 200 {
		t.Fatalf("adapt 失败 %d：%s", rec.Code, rec.Body.String())
	}
	return decodeJSON[adaptResponse](t, rec)
}

/* ---------- /v1/adapt ---------- */

// 三协议都能进来，出去的请求体里工具协议���经就位。
func TestAdaptPerProtocol(t *testing.T) {
	s := testServer(t)
	for proto, body := range protocolBodies() {
		t.Run(proto, func(t *testing.T) {
			got := adaptOnce(t, s, proto, body)

			if got.Signal == "" || got.Nonce == "" {
				t.Fatal("信号与 nonce 都该有")
			}
			// 上游要看到的是 prompt 里的协议说明，不是原生工具字段。
			up := string(got.UpstreamBody)
			if !strings.Contains(up, got.Signal) {
				t.Errorf("发给上游的请求里没有信号：%s", up)
			}
			if !strings.Contains(up, "get_weather") {
				t.Errorf("工具没进 prompt：%s", up)
			}
			if len(got.ToolsIncluded) != 1 || got.ToolsIncluded[0] != "get_weather" {
				t.Errorf("tools_included 不对：%v", got.ToolsIncluded)
			}
			// 模型名要原样回报——响应里该显示客户端问的那个。
			if got.Model == "" {
				t.Error("应回报客户端请求的模型名")
			}
		})
	}
}

// max_tools 收紧进入 Prompt 的工具数，always_include 的工具必须全进——
// 收紧上限是为了控清单规模，不是让核心工具消失。
func TestAdaptMaxToolsAndAlwaysInclude(t *testing.T) {
	s := testServer(t)
	var tools []map[string]any
	for i := 0; i < 8; i++ {
		tools = append(tools, map[string]any{
			"name":         fmt.Sprintf("tool_%d", i),
			"description":  "工具",
			"input_schema": map[string]any{"type": "object"},
		})
	}
	bodyBytes, _ := json.Marshal(map[string]any{
		"model": "m", "max_tokens": 100,
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
		"tools":    tools,
	})
	rec := post(t, s.handleAdapt, "/v1/adapt", adaptRequest{
		Protocol: "anthropic", SessionID: "s1", RequestID: "r1",
		Body:          json.RawMessage(bodyBytes),
		MaxTools:      3,
		AlwaysInclude: []string{"tool_5", "tool_7"},
	})
	if rec.Code != 200 {
		t.Fatalf("adapt 失败 %d：%s", rec.Code, rec.Body.String())
	}
	got := decodeJSON[adaptResponse](t, rec)
	if len(got.ToolsIncluded) != 3 {
		t.Fatalf("max_tools=3 时应选 3 个工具，得到 %v", got.ToolsIncluded)
	}
	gotSet := map[string]bool{}
	for _, n := range got.ToolsIncluded {
		gotSet[n] = true
	}
	for _, n := range []string{"tool_5", "tool_7"} {
		if !gotSet[n] {
			t.Errorf("always_include 的 %q 被截掉了：%v", n, got.ToolsIncluded)
		}
	}
}

// 上游说的话跟客户端不一样，是反代的日常。
func TestAdaptCrossProtocol(t *testing.T) {
	s := testServer(t)
	rec := post(t, s.handleAdapt, "/v1/adapt", adaptRequest{
		Protocol: "anthropic", SessionID: "s1", RequestID: "r1",
		Body:           json.RawMessage(protocolBodies()["anthropic"]),
		TargetProtocol: "chat",
		UpstreamModel:  "gpt-5.6-sol",
	})
	if rec.Code != 200 {
		t.Fatalf("状态 %d：%s", rec.Code, rec.Body.String())
	}
	got := decodeJSON[adaptResponse](t, rec)

	up := string(got.UpstreamBody)
	// Chat 的形状：messages 数组、模型名被换掉。
	if !strings.Contains(up, `"messages"`) {
		t.Errorf("应编成 Chat 形状：%s", up)
	}
	if !strings.Contains(up, "gpt-5.6-sol") {
		t.Errorf("上游模型名没换：%s", up)
	}
	// 但回报给调用方的仍是客户端问的那个。
	if got.Model != "claude-sonnet-4" {
		t.Errorf("回报的模型名应是客户端问的那个，得到 %q", got.Model)
	}
}

func TestAdaptRejectsBadProtocol(t *testing.T) {
	s := testServer(t)
	rec := post(t, s.handleAdapt, "/v1/adapt", adaptRequest{
		Protocol: "grpc", SessionID: "s1", RequestID: "r1",
		Body: json.RawMessage(`{"model":"x","messages":[]}`),
	})
	if rec.Code != 400 {
		t.Errorf("非法协议应回 400，得到 %d：%s", rec.Code, rec.Body.String())
	}
}

func TestAdaptRejectsMissingBody(t *testing.T) {
	s := testServer(t)
	rec := post(t, s.handleAdapt, "/v1/adapt", adaptRequest{
		Protocol: "chat", SessionID: "s1", RequestID: "r1",
	})
	if rec.Code != 400 {
		t.Errorf("缺 body 应回 400，得到 %d", rec.Code)
	}
}

/* ---------- /v1/render ---------- */

// 工具调用要以客户端原本期待的形状回去。
func TestRenderShapePerProtocol(t *testing.T) {
	s := testServer(t)
	wants := map[string][]string{
		"chat":      {`"choices"`, `"tool_calls"`, `"finish_reason":"tool_calls"`},
		"anthropic": {`"content"`, `"tool_use"`, `"stop_reason":"tool_use"`},
		"responses": {`"output"`, `"function_call"`},
	}
	for proto, body := range protocolBodies() {
		t.Run(proto, func(t *testing.T) {
			ad := adaptOnce(t, s, proto, body)

			rec := post(t, s.handleRender, "/v1/render", renderRequest{
				Protocol: proto, SessionID: "s1", RequestID: "r1", Nonce: ad.Nonce,
				Body: json.RawMessage(body),
				Result: renderResult{
					Text:    "我来查一下",
					Outcome: "calls_parsed",
					Calls: []toolbridge.Call{{
						ID: "call_1", Name: "get_weather",
						Arguments: json.RawMessage(`{"city":"北京"}`),
					}},
				},
			})
			if rec.Code != 200 {
				t.Fatalf("render 失败 %d：%s", rec.Code, rec.Body.String())
			}
			out := rec.Body.String()
			for _, w := range wants[proto] {
				if !strings.Contains(out, w) {
					t.Errorf("缺 %s：%s", w, out)
				}
			}
			// call_id 是确定性摘要（跨轮唯一），参数原样保留。客户端把
			// 结果关联回调用靠的是回传我们发出的同一个 call_id。
			if !strings.Contains(out, "call_") || !strings.Contains(out, "北京") {
				t.Errorf("call_id 或参数丢了：%s", out)
			}
		})
	}
}

// 没有工具调用时就是普通回答，不该被渲染成 tool_calls。
func TestRenderPlainText(t *testing.T) {
	s := testServer(t)
	body := protocolBodies()["chat"]
	ad := adaptOnce(t, s, "chat", body)

	rec := post(t, s.handleRender, "/v1/render", renderRequest{
		Protocol: "chat", SessionID: "s1", RequestID: "r1", Nonce: ad.Nonce,
		Body:   json.RawMessage(body),
		Result: renderResult{Text: "今天北京晴", Outcome: "plain_text"},
	})
	if rec.Code != 200 {
		t.Fatalf("状态 %d：%s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	if strings.Contains(out, "tool_calls") {
		t.Errorf("没有调用却渲染成 tool_calls：%s", out)
	}
	if !strings.Contains(out, "今天北京晴") {
		t.Errorf("正文丢了：%s", out)
	}
}

// outcome 直接决定 finish_reason，写错了会让客户端以为模型正常说完了。
func TestRenderRejectsBadOutcome(t *testing.T) {
	s := testServer(t)
	body := protocolBodies()["chat"]
	ad := adaptOnce(t, s, "chat", body)

	rec := post(t, s.handleRender, "/v1/render", renderRequest{
		Protocol: "chat", SessionID: "s1", RequestID: "r1", Nonce: ad.Nonce,
		Body:   json.RawMessage(body),
		Result: renderResult{Text: "x", Outcome: "finished"},
	})
	if rec.Code != 400 {
		t.Errorf("非法 outcome 应回 400，得到 %d：%s", rec.Code, rec.Body.String())
	}
}

// 省略 outcome 是允许的：有调用就是 calls_parsed，没有就是 plain_text。
func TestRenderInfersOutcome(t *testing.T) {
	s := testServer(t)
	body := protocolBodies()["chat"]
	ad := adaptOnce(t, s, "chat", body)

	rec := post(t, s.handleRender, "/v1/render", renderRequest{
		Protocol: "chat", SessionID: "s1", RequestID: "r1", Nonce: ad.Nonce,
		Body: json.RawMessage(body),
		Result: renderResult{
			Text: "我来查一下",
			Calls: []toolbridge.Call{{
				ID: "c1", Name: "get_weather", Arguments: json.RawMessage(`{"city":"北京"}`),
			}},
		},
	})
	if rec.Code != 200 {
		t.Fatalf("状态 %d：%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"finish_reason":"tool_calls"`) {
		t.Errorf("有调用就该推断成 calls_parsed：%s", rec.Body.String())
	}
}

func TestRenderRejectsBadNonce(t *testing.T) {
	s := testServer(t)
	body := protocolBodies()["chat"]
	rec := post(t, s.handleRender, "/v1/render", renderRequest{
		Protocol: "chat", SessionID: "s1", RequestID: "r1", Nonce: "不是合法 nonce",
		Body:   json.RawMessage(body),
		Result: renderResult{Text: "x"},
	})
	if rec.Code < 400 {
		t.Errorf("坏 nonce 应报错，得到 %d", rec.Code)
	}
}

/* ---------- /v1/render/stream ---------- */

// 事件形状要对：少一个结束事件，客户端就一直等。
func TestRenderStreamPerProtocol(t *testing.T) {
	s := testServer(t)
	wants := map[string][]string{
		"chat":      {"[DONE]"},
		"anthropic": {"message_stop"},
		"responses": {"response.completed"},
	}
	for proto, body := range protocolBodies() {
		t.Run(proto, func(t *testing.T) {
			ad := adaptOnce(t, s, proto, body)

			rec := post(t, s.handleRenderStream, "/v1/render/stream", renderRequest{
				Protocol: proto, SessionID: "s1", RequestID: "r1", Nonce: ad.Nonce,
				Body: json.RawMessage(body),
				Result: renderResult{
					Text:    "我来查一下",
					Outcome: "calls_parsed",
					Calls: []toolbridge.Call{{
						ID: "call_1", Name: "get_weather",
						Arguments: json.RawMessage(`{"city":"北京"}`),
					}},
				},
			})
			if rec.Code != 200 {
				t.Fatalf("状态 %d：%s", rec.Code, rec.Body.String())
			}
			if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
				t.Errorf("Content-Type 应是 text/event-stream，得到 %q", ct)
			}
			out := rec.Body.String()
			if !strings.HasPrefix(out, "data:") && !strings.HasPrefix(out, "event:") {
				t.Errorf("不是 SSE：%s", out)
			}
			for _, w := range wants[proto] {
				if !strings.Contains(out, w) {
					t.Errorf("缺结束事件 %s：%s", w, out)
				}
			}
			if !strings.Contains(out, "get_weather") {
				t.Errorf("工具调用没进事件流：%s", out)
			}
		})
	}
}

/* ---------- 端到端 ---------- */

// 一个反代的完整流程：adapt → 假装上游回了调用 → parse → render。
// 这条测试是 B 方案的骨架，JS 那边照着这个顺序调就行。
func TestAdaptParseRenderRoundTrip(t *testing.T) {
	s := testServer(t)
	for proto, body := range protocolBodies() {
		t.Run(proto, func(t *testing.T) {
			ad := adaptOnce(t, s, proto, body)

			// 上游模型按 prompt 里教的格式输出。
			modelText := "我来查一下\n" + ad.Signal + `
<tool_call_envelope version="1">
  <call id="c1">
    <tool>get_weather</tool>
    <arguments_json><![CDATA[{"city":"北京"}]]></arguments_json>
  </call>
</tool_call_envelope>`

			rec := post(t, s.handleParse, "/v1/parse", parseRequest{
				SessionID: "s1", RequestID: "r1", Nonce: ad.Nonce, Text: modelText,
			})
			if rec.Code != 200 {
				t.Fatalf("parse 失败 %d：%s", rec.Code, rec.Body.String())
			}
			pr := decodeJSON[parseResponse](t, rec)
			if pr.Outcome != "calls_parsed" || len(pr.Calls) != 1 {
				t.Fatalf("没解析出调用：%+v", pr)
			}
			// 信号一个字都不该漏给客户端。
			if strings.Contains(pr.Text, ad.Signal) {
				t.Errorf("信号泄漏进正文：%q", pr.Text)
			}

			rec = post(t, s.handleRender, "/v1/render", renderRequest{
				Protocol: proto, SessionID: "s1", RequestID: "r1", Nonce: ad.Nonce,
				Body: json.RawMessage(body),
				Result: renderResult{
					Text: pr.Text, Calls: pr.Calls, Outcome: pr.Outcome,
				},
			})
			if rec.Code != 200 {
				t.Fatalf("render 失败 %d：%s", rec.Code, rec.Body.String())
			}
			out := rec.Body.String()
			if !strings.Contains(out, "get_weather") || !strings.Contains(out, "北京") {
				t.Errorf("调用没回到客户端：%s", out)
			}
		})
	}
}

// 服务端不保存任何会话——换一个实例照样能渲染。
func TestRenderWorksOnDifferentInstance(t *testing.T) {
	ad := adaptOnce(t, testServer(t), "chat", protocolBodies()["chat"])

	other := testServer(t) // 全新实例，什么都不知道
	rec := post(t, other.handleRender, "/v1/render", renderRequest{
		Protocol: "chat", SessionID: "s1", RequestID: "r1", Nonce: ad.Nonce,
		Body: json.RawMessage(protocolBodies()["chat"]),
		Result: renderResult{
			Text: "我来查一下", Outcome: "calls_parsed",
			Calls: []toolbridge.Call{{
				ID: "c1", Name: "get_weather", Arguments: json.RawMessage(`{"city":"北京"}`),
			}},
		},
	})
	if rec.Code != 200 {
		t.Fatalf("换实例后渲染失败 %d：%s", rec.Code, rec.Body.String())
	}
}

var _ = httptest.NewRequest // 保持与 main_test.go 相同的导入约定

// 数组字段绝不能返回 JSON null。
//
// 没有工具时 Go 的 nil slice 会编成 null，而弱类型的调用方多半直接读
// .length——症状是反代侧一个与工具毫无关系的 TypeError，排查方向完全
// 错误。这是实测踩到的：无工具的辅助请求让整条链路 502，错误信息是
// 「Cannot read properties of null (reading 'length')」。
func TestAdaptNeverReturnsNullArrays(t *testing.T) {
	s := testServer(t)

	body := `{"model":"m","tools":[],"input":[{"type":"message","role":"user","content":"起个标题"}]}`
	rec := post(t, s.handleAdapt, "/v1/adapt", adaptRequest{
		Protocol: "responses", SessionID: "s", RequestID: "r",
		Body: json.RawMessage(body),
	})
	if rec.Code != 200 {
		t.Fatalf("无工具的请求不该失败：%d %s", rec.Code, rec.Body)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if got := string(raw["tools_included"]); got != "[]" {
		t.Errorf("tools_included 应是空数组而不是 %s", got)
	}
	if string(raw["virtual_protocol"]) != "false" {
		t.Errorf("没有工具时不该注入虚拟协议：%s", raw["virtual_protocol"])
	}
}
