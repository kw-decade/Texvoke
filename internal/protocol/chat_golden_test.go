package protocol

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kw-decade/Texvoke/internal/ir"
)

// updateGolden 用 -update 重新生成 golden 文件。协议渲染有意变更时跑
//
//	go test ./internal/protocol/ -update
//
// 然后审阅 diff——golden 文件的每一处变化都应当是你能解释的。
var updateGolden = flag.Bool("update", false, "重新生成 golden 文件")

const fixtureDir = "../../tests/fixtures/chat"

// TestFixtureRequestsRoundTrip 用真实形状的线上请求验证往返不丢信息。
//
// 这些 fixture 是回归基准：解析器改动后若它们仍然通过，说明真实客户端
// 发来的请求还能被正确处理。
func TestFixtureRequestsRoundTrip(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join(fixtureDir, "request_*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatalf("在 %s 下没有找到任何 request fixture", fixtureDir)
	}

	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}

			req, err := DecodeChatRequest(data, testOpts)
			if err != nil {
				t.Fatalf("解码失败：%v", err)
			}

			encoded, err := EncodeChatRequest(*req)
			if err != nil {
				t.Fatalf("编码失败：%v", err)
			}

			again, err := DecodeChatRequest(encoded, testOpts)
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

// TestFixtureSpecifics 对每份 fixture 断言它专门要覆盖的那个点，
// 避免「往返一致」掩盖了「两次都同样地丢了某个字段」。
func TestFixtureSpecifics(t *testing.T) {
	load := func(t *testing.T, name string) *ChatRequest {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(fixtureDir, name))
		if err != nil {
			t.Fatal(err)
		}
		req, err := DecodeChatRequest(data, testOpts)
		if err != nil {
			t.Fatalf("解码 %s 失败：%v", name, err)
		}
		return req
	}

	t.Run("带工具的请求", func(t *testing.T) {
		req := load(t, "request_with_tools.json")
		if len(req.Tools) != 1 || req.Tools[0].Name != "get_weather" {
			t.Fatalf("工具解析有误：%+v", req.Tools)
		}
		if req.ToolChoice.Mode != ToolChoiceAuto {
			t.Errorf("tool_choice 为 %q，期望 auto", req.ToolChoice.Mode)
		}
		if req.ParallelToolCalls == nil || !*req.ParallelToolCalls {
			t.Error("parallel_tool_calls=true 丢失")
		}
		if _, ok := req.Extra["temperature"]; !ok {
			t.Error("temperature 未进入 Extra，将无法透传给上游")
		}
		// schema 里的 enum 与 additionalProperties 是校验语义的一部分，
		// 压缩描述可以，改写这些不行。
		schema := string(req.Tools[0].InputSchema)
		for _, want := range []string{"enum", "additionalProperties", "required"} {
			if !strings.Contains(schema, want) {
				t.Errorf("schema 中丢失了 %q：%s", want, schema)
			}
		}
	})

	t.Run("含调用历史的请求", func(t *testing.T) {
		req := load(t, "request_with_history.json")
		if len(req.Messages) != 4 {
			t.Fatalf("消息数为 %d，期望 4", len(req.Messages))
		}

		calls := req.Messages[1].ToolCalls
		if len(calls) != 2 {
			t.Fatalf("并行调用数为 %d，期望 2", len(calls))
		}
		// 真实的 call ID 含大小写混排，必须逐字保留而不是规范化。
		if calls[0].CallID != "call_aBc123" || calls[1].CallID != "call_dEf456" {
			t.Errorf("call ID 未原样保留：%q %q", calls[0].CallID, calls[1].CallID)
		}
		// 两个调用的参数不同，幂等键必须不同，否则账本会把它们当成重试。
		if calls[0].IdempotencyKey() == calls[1].IdempotencyKey() {
			t.Error("两次不同城市的查询算出了相同的幂等键")
		}
		// 结果消息必须能关联回各自的调用。
		if len(req.Messages[2].ToolResults) != 1 || req.Messages[2].ToolResults[0].CallID != "call_aBc123" {
			t.Errorf("第 3 条消息与调用的关联丢失：%+v", req.Messages[2].ToolResults)
		}
		if len(req.Messages[3].ToolResults) != 1 || req.Messages[3].ToolResults[0].CallID != "call_dEf456" {
			t.Errorf("第 4 条消息与调用的关联丢失：%+v", req.Messages[3].ToolResults)
		}
		if req.Messages[2].Role != RoleTool || req.Messages[2].Role.Instructional() {
			t.Error("工具结果不得被当作指令来源")
		}
	})

	t.Run("多模态与具名选择", func(t *testing.T) {
		req := load(t, "request_multimodal_named_choice.json")
		if req.ToolChoice.Mode != ToolChoiceNamed || req.ToolChoice.Name != "describe_image" {
			t.Errorf("tool_choice 解析为 %+v", req.ToolChoice)
		}
		if !req.ToolChoice.RequiresCall() {
			t.Error("具名选择应当要求至少一个调用")
		}
		// content 是 part 数组时，Text 只取文本部分，而原始 content
		// 必须完整保留，否则图片在往返后会消失。
		if got := req.Messages[0].Text(); got != "这张图里是什么？" {
			t.Errorf("Text() = %q", got)
		}
		if !strings.Contains(string(req.Messages[0].Content), "image_url") {
			t.Error("多模态内容在归一化中丢失了图片部分")
		}
	})
}

// TestGoldenResponse 固定住渲染给客户端的响应形状。
//
// 这份 golden 的意义是「官方 SDK 能不能消费我们的输出」：字段名、
// arguments 的字符串形态、finish_reason 一旦改动，这里立刻会响。
func TestGoldenResponse(t *testing.T) {
	resp := ChatResponse{
		ID:      "chatcmpl-fixture-1",
		Model:   "gpt-4o",
		Created: 1700000000,
		ToolCalls: []ir.ToolCallProposal{
			{
				SessionID: "sess-1", RequestID: "req-1", CallID: "call_aBc123",
				Tool:      ir.ToolID{Namespace: ir.NamespaceClient, Name: "get_weather", Version: ir.VersionDeclared},
				Arguments: json.RawMessage(`{"city":"San Francisco","unit":"celsius"}`),
				Source:    ir.SourceNative, CreatedAt: testOpts.Now,
			},
			{
				SessionID: "sess-1", RequestID: "req-1", CallID: "call_dEf456",
				Tool:      ir.ToolID{Namespace: ir.NamespaceClient, Name: "get_weather", Version: ir.VersionDeclared},
				Arguments: json.RawMessage(`{"city":"Tokyo","unit":"celsius"}`),
				Source:    ir.SourceNative, CreatedAt: testOpts.Now,
			},
		},
		FinishReason: FinishToolCalls,
		Usage:        &Usage{PromptTokens: 120, CompletionTokens: 48, TotalTokens: 168},
	}

	got, err := EncodeChatResponse(resp)
	if err != nil {
		t.Fatalf("编码失败：%v", err)
	}

	// 落盘前重排成缩进形式，让 diff 可读。
	var pretty any
	if err := json.Unmarshal(got, &pretty); err != nil {
		t.Fatal(err)
	}
	formatted, err := json.MarshalIndent(pretty, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	formatted = append(formatted, '\n')

	goldenPath := filepath.Join(fixtureDir, "response_tool_calls.golden.json")
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

	// 按 JSON 语义比对而不是逐字节：键序不是协议的一部分，
	// 但字段的存在与形态是。
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
