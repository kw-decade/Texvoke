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

const respFixtureDir = "../../tests/fixtures/responses"

func TestResponsesFixturesRoundTrip(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join(respFixtureDir, "request_*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatalf("在 %s 下没有找到任何 request fixture", respFixtureDir)
	}

	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			req, err := DecodeResponsesRequest(data, testOpts)
			if err != nil {
				t.Fatalf("解码失败：%v", err)
			}
			encoded, err := EncodeResponsesRequest(*req)
			if err != nil {
				t.Fatalf("编码失败：%v", err)
			}
			again, err := DecodeResponsesRequest(encoded, testOpts)
			if err != nil {
				t.Fatalf("二次解码失败：%v\n中间结果：%s", err, encoded)
			}
			stripDigests(req.Input)
			stripDigests(again.Input)
			if !reflect.DeepEqual(req, again) {
				t.Errorf("往返改变了语义\n首次：%+v\n再次：%+v", req, again)
			}
		})
	}
}

func TestResponsesFixtureSpecifics(t *testing.T) {
	load := func(t *testing.T, name string) *ResponsesRequest {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(respFixtureDir, name))
		if err != nil {
			t.Fatal(err)
		}
		req, err := DecodeResponsesRequest(data, testOpts)
		if err != nil {
			t.Fatalf("解码 %s 失败：%v", name, err)
		}
		return req
	}

	t.Run("带工具的请求", func(t *testing.T) {
		req := load(t, "request_with_tools.json")
		if req.Input[0].Role != RoleSystem {
			t.Fatalf("instructions 未转成首条 system 消息：%+v", req.Input[0])
		}
		if req.MaxOutputTokens != 2048 {
			t.Errorf("max_output_tokens 为 %d", req.MaxOutputTokens)
		}
		if req.Store == nil || !*req.Store {
			t.Error("store=true 丢失")
		}
		if _, ok := req.Extra["temperature"]; !ok {
			t.Error("temperature 未进入 Extra")
		}
		schema := string(req.Tools[0].InputSchema)
		for _, want := range []string{"enum", "additionalProperties", "required"} {
			if !strings.Contains(schema, want) {
				t.Errorf("schema 中丢失了 %q：%s", want, schema)
			}
		}
	})

	t.Run("完整的调用往返", func(t *testing.T) {
		req := load(t, "request_call_roundtrip.json")

		if req.PreviousResponseID != "resp_68a1b2c3d4e5f6" {
			t.Errorf("previous_response_id 为 %q——它是服务端状态的句柄，丢了会让上游看到另一段历史", req.PreviousResponseID)
		}

		calls := req.Input[1].ToolCalls
		if len(calls) != 2 {
			t.Fatalf("并行调用数为 %d，期望 2", len(calls))
		}
		// 真实的 Responses ID 是两套独立的前缀，必须分别原样保留。
		if calls[0].ProtocolItemID != "fc_68a1aaa1111" || calls[0].CallID != "call_9kL2mN4pQ" {
			t.Errorf("两个 ID 未同时保留：item=%q call=%q", calls[0].ProtocolItemID, calls[0].CallID)
		}
		if calls[1].ProtocolItemID != "fc_68a1bbb2222" || calls[1].CallID != "call_7xY8zA1bC" {
			t.Errorf("第二个调用的 ID 有误：item=%q call=%q", calls[1].ProtocolItemID, calls[1].CallID)
		}
		if calls[0].IdempotencyKey() == calls[1].IdempotencyKey() {
			t.Error("两次不同城市的查询算出了相同的幂等键")
		}

		results := req.Input[2].ToolResults
		if len(results) != 2 {
			t.Fatalf("结果数为 %d，期望 2", len(results))
		}
		// 结果按 call_id 关联，不是按 item id。
		if results[0].CallID != "call_9kL2mN4pQ" || results[1].CallID != "call_7xY8zA1bC" {
			t.Errorf("结果关联错乱：%q %q", results[0].CallID, results[1].CallID)
		}
	})
}

func TestResponsesGoldenResponse(t *testing.T) {
	resp := ResponsesResponse{
		ID:            "resp_fixture01",
		Model:         "gpt-4o",
		CreatedAt:     1700000000,
		Status:        ResponseCompleted,
		Content:       json.RawMessage(`[{"type":"output_text","text":"我来分别查一下这两个城市。","annotations":[]}]`),
		MessageItemID: "msg_fixture01",
		ToolCalls: []ir.ToolCallProposal{
			{
				SessionID: "sess-1", RequestID: "req-1",
				CallID: "call_9kL2mN4pQ", ProtocolItemID: "fc_68a1aaa1111",
				Tool:      ir.ToolID{Namespace: ir.NamespaceClient, Name: "get_weather", Version: ir.VersionDeclared},
				Arguments: json.RawMessage(`{"city":"San Francisco","unit":"celsius"}`),
				Source:    ir.SourceNative, CreatedAt: testOpts.Now,
			},
			{
				SessionID: "sess-1", RequestID: "req-1",
				CallID: "call_7xY8zA1bC", ProtocolItemID: "fc_68a1bbb2222",
				Tool:      ir.ToolID{Namespace: ir.NamespaceClient, Name: "get_weather", Version: ir.VersionDeclared},
				Arguments: json.RawMessage(`{"city":"Tokyo","unit":"celsius"}`),
				Source:    ir.SourceNative, CreatedAt: testOpts.Now,
			},
		},
		Usage: &ResponsesUsage{InputTokens: 384, OutputTokens: 96, TotalTokens: 480},
	}

	got, err := EncodeResponsesResponse(resp)
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

	goldenPath := filepath.Join(respFixtureDir, "response_function_call.golden.json")
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
