package main

// 评测工具唯一的判断题的测试：一轮请求的产出该归进哪个格子。
//
// 值得有测试的理由很具体：这个函数判错不会报错，只会让报表用一个好看的
// 数字盖住真实故障。2026-08-26 就发生过——上游回 400 model does not exist，
// 网关按不变量 16 降级成 200 + 空 plain_text，报表显示 100% plain_text_ok。

import (
	"strings"
	"testing"

	"github.com/kw-decade/Texvoke/internal/capability"
)

func TestClassifyRun(t *testing.T) {
	for _, tc := range []struct {
		name       string
		kind       capability.RefusalKind
		out        outcome
		expectTool string
		wantKind   string
		wantWrong  bool
	}{
		{
			name:     "拒绝分类命中时原样上报",
			kind:     capability.PersonaRefusal,
			out:      outcome{text: "我不能直接操作你的文件系统"},
			wantKind: string(capability.PersonaRefusal),
		},
		{
			name:       "拿到期望的调用",
			kind:       capability.RefusalNone,
			out:        outcome{calls: []string{"exec"}},
			expectTool: "exec",
			wantKind:   "calls_parsed",
		},
		{
			name:       "调用了别的工具：仍算协议层成功，只记旁注",
			kind:       capability.RefusalNone,
			out:        outcome{calls: []string{"read_file"}},
			expectTool: "exec",
			wantKind:   "calls_parsed",
			wantWrong:  true,
		},
		{
			name:     "零调用但有正文：auto 下的合法回答",
			kind:     capability.RefusalNone,
			out:      outcome{text: "旧金山现在多云。"},
			wantKind: "plain_text_ok",
		},
		{
			name:     "零调用零正文：这一轮什么都没发生，不许算良性成功",
			kind:     capability.RefusalNone,
			out:      outcome{},
			wantKind: "empty_response",
		},
		{
			name:     "只有空白的正文同样算空",
			kind:     capability.RefusalNone,
			out:      outcome{text: "  \n\t "},
			wantKind: "empty_response",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kind, wrong := classifyRun(tc.kind, tc.out, tc.expectTool)
			if kind != tc.wantKind {
				t.Errorf("kind = %q，期望 %q", kind, tc.wantKind)
			}
			if wrong != tc.wantWrong {
				t.Errorf("wrongTool = %v，期望 %v", wrong, tc.wantWrong)
			}
		})
	}
}

func TestOverrideModel(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4","messages":[]}`)

	got, err := overrideModel(body, "gpt-5.6")
	if err != nil {
		t.Fatal(err)
	}
	if want := `"model":"gpt-5.6"`; !strings.Contains(string(got), want) {
		t.Fatalf("模型名没换掉：%s", got)
	}

	// 空覆盖必须原样返回：抓包的价值就在于它是原样的。
	same, err := overrideModel(body, "")
	if err != nil {
		t.Fatal(err)
	}
	if string(same) != string(body) {
		t.Fatalf("空覆盖改动了 fixture：%s", same)
	}

	if _, err := overrideModel([]byte(`{`), "gpt-5.6"); err == nil {
		t.Fatal("非法 JSON 必须报错而不是静默放过")
	}
}
