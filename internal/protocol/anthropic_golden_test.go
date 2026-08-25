package protocol

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kw-decade/Texvoke/internal/ir"
)

const anthFixtureDir = "../../tests/fixtures/anthropic"

func TestAnthropicFixturesRoundTrip(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join(anthFixtureDir, "request_*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatalf("在 %s 下没有找到任何 request fixture", anthFixtureDir)
	}

	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			req, err := DecodeMessagesRequest(data, testOpts)
			if err != nil {
				t.Fatalf("解码失败：%v", err)
			}
			encoded, err := EncodeMessagesRequest(*req)
			if err != nil {
				t.Fatalf("编码失败：%v", err)
			}
			again, err := DecodeMessagesRequest(encoded, testOpts)
			if err != nil {
				t.Fatalf("二次解码失败：%v\n中间结果：%s", err, encoded)
			}
			stripDigests(req.Messages)
			stripDigests(again.Messages)
			if !reflect.DeepEqual(req, again) {
				t.Errorf("往返改变了语义\n首次：%+v\n再次：%+v", req, again)
			}
		})
	}
}

func TestAnthropicFixtureSpecifics(t *testing.T) {
	load := func(t *testing.T, name string) *MessagesRequest {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(anthFixtureDir, name))
		if err != nil {
			t.Fatal(err)
		}
		req, err := DecodeMessagesRequest(data, testOpts)
		if err != nil {
			t.Fatalf("解码 %s 失败：%v", name, err)
		}
		return req
	}

	t.Run("带工具的请求", func(t *testing.T) {
		req := load(t, "request_with_tools.json")
		if req.MaxTokens != 2048 {
			t.Errorf("max_tokens 为 %d", req.MaxTokens)
		}
		if req.Messages[0].Role != RoleSystem {
			t.Fatalf("system 未转成首条消息：%+v", req.Messages[0])
		}
		if req.ParallelToolCalls == nil || !*req.ParallelToolCalls {
			t.Error("disable_parallel_tool_use=false 应翻转成 ParallelToolCalls=true")
		}
		if _, ok := req.Extra["temperature"]; !ok {
			t.Error("temperature 未进入 Extra，将无法透传给上游")
		}
		schema := string(req.Tools[0].InputSchema)
		for _, want := range []string{"enum", "additionalProperties", "required"} {
			if !strings.Contains(schema, want) {
				t.Errorf("schema 中丢失了 %q：%s", want, schema)
			}
		}
	})

	t.Run("并行调用与多个结果", func(t *testing.T) {
		req := load(t, "request_parallel_results.json")

		calls := req.Messages[1].ToolCalls
		if len(calls) != 2 {
			t.Fatalf("并行调用数为 %d，期望 2", len(calls))
		}
		// 真实的 Anthropic call id 很长且大小写混排，必须逐字保留。
		if calls[0].CallID != "toolu_01A09q90qw90lkasdjl" {
			t.Errorf("call id 未原样保留：%q", calls[0].CallID)
		}
		if calls[0].IdempotencyKey() == calls[1].IdempotencyKey() {
			t.Error("两次不同城市的查询算出了相同的幂等键")
		}

		results := req.Messages[2].ToolResults
		if len(results) != 2 {
			t.Fatalf("同一条消息里的结果数为 %d，期望 2", len(results))
		}
		if results[0].IsError {
			t.Error("第一个结果不应带错误标记")
		}
		if !results[1].IsError {
			t.Error("第二个结果的 is_error 丢失——超时与正常返回将无法区分")
		}
		// 结果与文本共存于同一条 user 消息，两者都不能丢。
		if !strings.Contains(string(req.Messages[2].Content), "该穿什么") {
			t.Errorf("同消息内的文本丢失：%s", req.Messages[2].Content)
		}
	})

	t.Run("thinking 与具名选择", func(t *testing.T) {
		req := load(t, "request_thinking_named_choice.json")
		if req.ToolChoice.Mode != ToolChoiceNamed || req.ToolChoice.Name != "describe_image" {
			t.Errorf("tool_choice 解析为 %+v", req.ToolChoice)
		}
		if req.ParallelToolCalls == nil || *req.ParallelToolCalls {
			t.Error("disable_parallel_tool_use=true 应翻转成 ParallelToolCalls=false")
		}
		// thinking block 原样搬运，签名不能丢——丢了签名的 thinking 会被
		// Anthropic 拒绝，且我们无权改写模型的隐藏推理内容。
		content := string(req.Messages[1].Content)
		for _, want := range []string{"thinking", "signature", "ErUBCkYIBBgCIkAyy7Z0"} {
			if !strings.Contains(content, want) {
				t.Errorf("thinking block 中丢失了 %q：%s", want, content)
			}
		}
	})
}

// TestAnthropicGoldenResponse 固定住渲染给 Anthropic 客户端的响应形状。
func TestAnthropicGoldenResponse(t *testing.T) {
	resp := MessagesResponse{
		ID:      "msg_01fixture",
		Model:   "claude-sonnet-5",
		Content: json.RawMessage(`[{"type":"text","text":"我来分别查一下这两个城市。"}]`),
		ToolCalls: []ir.ToolCallProposal{
			{
				SessionID: "sess-1", RequestID: "req-1", CallID: "toolu_01A09q90qw90lkasdjl",
				Tool:      ir.ToolID{Namespace: ir.NamespaceClient, Name: "get_weather", Version: ir.VersionDeclared},
				Arguments: json.RawMessage(`{"city":"San Francisco","unit":"celsius"}`),
				Source:    ir.SourceNative, CreatedAt: testOpts.Now,
			},
			{
				SessionID: "sess-1", RequestID: "req-1", CallID: "toolu_02B18r81rx81mlbtekm",
				Tool:      ir.ToolID{Namespace: ir.NamespaceClient, Name: "get_weather", Version: ir.VersionDeclared},
				Arguments: json.RawMessage(`{"city":"Tokyo","unit":"celsius"}`),
				Source:    ir.SourceNative, CreatedAt: testOpts.Now,
			},
		},
		StopReason: StopToolUse,
		Usage:      &AnthropicUsage{InputTokens: 384, OutputTokens: 96, CacheReadInputTokens: 256},
	}

	got, err := EncodeMessagesResponse(resp)
	if err != nil {
		t.Fatalf("编码失败：%v", err)
	}

	var pretty any
	if err := json.Unmarshal(got, &pretty); err != nil {
		t.Fatal(err)
	}
	formatted, err := json.MarshalIndent(pretty, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	formatted = append(formatted, '\n')

	goldenPath := filepath.Join(anthFixtureDir, "response_tool_use.golden.json")
	if *updateGolden {
		if err := os.WriteFile(goldenPath, formatted, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("已更新 %s", goldenPath)
		return
	}

	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("读取 golden 失败（首次运行请加 -update）：%v", err)
	}

	var gotAny, wantAny any
	if err := json.Unmarshal(formatted, &gotAny); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(want, &wantAny); err != nil {
		t.Fatalf("golden 文件不是合法 JSON：%v", err)
	}
	if !reflect.DeepEqual(gotAny, wantAny) {
		t.Errorf("响应形状与 golden 不符\n实际：\n%s\n期望：\n%s", formatted, want)
	}
}
