package toolbridge

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/kw-decade/Texvoke/internal/capability"
)

func refusalResult(text string) *Result {
	return &Result{Outcome: OutcomePlainText, Text: text}
}

func parseFailResult() *Result {
	return &Result{Outcome: OutcomeMalformed}
}

func parseErrOf(kind ErrorKind) error {
	if kind == "" {
		return nil
	}
	return wrap(kind, errors.New("测试错误"))
}

// 诊断是纯接线：给什么证据出什么结论，不发明判断。
func TestDiagnoseWiresClassify(t *testing.T) {
	b, _ := New(Config{})
	sess, _ := b.NewSession("s", "r")

	t.Run("模型说不能执行 → persona_refusal", func(t *testing.T) {
		d := sess.Diagnose(refusalResult("我无法直接读取文件系统，请自行运行命令"), nil, Evidence{
			ToolsDeclared: 3, ToolsSent: 3, ToolChoice: "auto",
			ModelText: "我无法直接读取文件系统，请自行运行命令",
			AgentMode: true,
		})
		if d.Kind != string(capability.PersonaRefusal) {
			t.Errorf("根因 = %q", d.Kind)
		}
		if d.Terminal {
			t.Error("人格误会不是硬拒绝")
		}
	})

	t.Run("客户端没发工具 → client_capability_missing", func(t *testing.T) {
		d := sess.Diagnose(nil, nil, Evidence{ToolsDeclared: 0})
		if d.Kind != string(capability.ClientCapabilityMissing) {
			t.Errorf("根因 = %q", d.Kind)
		}
	})

	t.Run("解析失败 → format_noncompliance 且可修", func(t *testing.T) {
		d := sess.Diagnose(parseFailResult(), parseErrOf(ErrParseFailed), Evidence{
			ToolsDeclared: 2, ToolsSent: 2,
		})
		if d.Kind != string(capability.FormatNoncompliance) {
			t.Errorf("根因 = %q", d.Kind)
		}
	})

	t.Run("上游 403 → 硬拒绝立即停手", func(t *testing.T) {
		d := sess.Diagnose(nil, nil, Evidence{
			ToolsDeclared: 2, ToolsSent: 2,
			HTTPStatus:      403,
			UpstreamErrCode: "permission_denied",
		})
		if !d.Terminal {
			t.Fatal("政策拒绝必须标记 Terminal")
		}
		rc, err := sess.Recover(d, nil, nil, []string{"f"}, Evidence{})
		if err != nil {
			t.Fatal(err)
		}
		if rc.ShouldRetry {
			t.Errorf("Terminal 类不得给出重试：%+v", rc)
		}
	})
}

// 突破阶梯：人格类拒绝的追问强度随 attempt 与 handshake 状态递进。
// L1 能力说明 → L2 运行时通知 → L3 完整示范 → L4 明示行动。
func TestRecoverEscalationLadder(t *testing.T) {
	b, _ := New(Config{})
	sess, _ := b.NewSession("s", "r")
	text := "我会先读取项目的 AGENTS.md 约定；没有冲突就创建空白.txt，并确认大小为 0。"
	d := func() Diagnosis {
		return sess.Diagnose(refusalResult(text), nil, Evidence{
			ToolsDeclared: 1, ToolsSent: 1, ModelText: text,
			AgentMode: true, // agent 会话零调用：结构性判定，不依赖词表
		})
	}

	t.Run("L1 首次失败给能力说明", func(t *testing.T) {
		first, err := sess.Recover(d(), refusalResult(text), nil, []string{"exec"}, Evidence{Attempt: 1})
		if err != nil {
			t.Fatal(err)
		}
		if !first.ShouldRetry || !first.HandshakeDone {
			t.Fatalf("第一级应是能力说明：%+v", first)
		}
		msg := first.Messages[0].Text
		for _, want := range []string{"不需要亲自执行", "建议"} {
			if !strings.Contains(msg, want) {
				t.Errorf("能力说明缺 %q：\n%s", want, msg)
			}
		}
	})

	t.Run("L2 运行时通知体 + 成功事实引用", func(t *testing.T) {
		rc, err := sess.Recover(d(), refusalResult(text), nil, []string{"exec"}, Evidence{
			Attempt: 2, HandshakeDone: true, HasSuccessfulHistory: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		if !rc.ShouldRetry || len(rc.Messages) != 1 {
			t.Fatalf("第二级应给运行时通知：%+v", rc)
		}
		msg := rc.Messages[0].Text
		for _, want := range []string{
			"[runtime]", "未检测到调用信号", "已成功送出", "若你判断此任务确实不应执行",
		} {
			if !strings.Contains(msg, want) {
				t.Errorf("运行时通知缺 %q：\n%s", want, msg)
			}
		}
		for _, banned := range []string{"你必须", "忽略之前", "事实错误"} {
			if strings.Contains(msg, banned) {
				t.Errorf("含施压措辞 %q：\n%s", banned, msg)
			}
		}
	})

	t.Run("L3 附完整可照抄示例且参数是占位符", func(t *testing.T) {
		rc, err := sess.Recover(d(), refusalResult(text), nil, []string{"exec"}, Evidence{
			Attempt: 3, HandshakeDone: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		msg := rc.Messages[0].Text
		for _, want := range []string{"[runtime]", "<tool_call_envelope", "<tool>exec</tool>", "<参数值>"} {
			if !strings.Contains(msg, want) {
				t.Errorf("示范缺 %q：\n%s", want, msg)
			}
		}
	})

	t.Run("L4 明示行动要求", func(t *testing.T) {
		rc, err := sess.Recover(d(), refusalResult(text), nil, []string{"exec"}, Evidence{
			Attempt: 4, HandshakeDone: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		msg := rc.Messages[0].Text
		for _, want := range []string{"需要执行一次工具操作", "任选其一"} {
			if !strings.Contains(msg, want) {
				t.Errorf("L4 缺 %q：\n%s", want, msg)
			}
		}
	})

	// 跨请求「从上次进度继续、不重复 L1」由调用方的会话状态保证：反代记住
	// 上次用到的级数，把新请求的 Attempt 从那里接着编。sidecar 无状态，
	// Attempt 是唯一的事实来源。

	t.Run("格式失败给针对性提示（不走阶梯）", func(t *testing.T) {
		d := sess.Diagnose(parseFailResult(), parseErrOf(ErrTruncated), Evidence{
			ToolsDeclared: 1, ToolsSent: 1,
		})
		rc, err := sess.Recover(d, parseFailResult(), parseErrOf(ErrTruncated), []string{"exec"}, Evidence{})
		if err != nil {
			t.Fatal(err)
		}
		if !rc.ShouldRetry || len(rc.Messages) != 1 {
			t.Fatalf("格式失败应给出修复提示：%+v", rc)
		}
		if rc.HandshakeDone {
			t.Error("格式修复不该动 handshake 状态")
		}
		// 截断要被翻译成「没写完」，而不是 Go 的错误字符串。
		msg := rc.Messages[0].Text
		if strings.Contains(msg, "toolbridge") || strings.Contains(msg, "测试错误") {
			t.Errorf("内部错误泄漏进了给模型的话：\n%s", msg)
		}
		if !strings.Contains(msg, "闭合之前就中断") {
			t.Errorf("截断没被翻译：\n%s", msg)
		}
	})

	t.Run("传输类不在这里重试", func(t *testing.T) {
		// 三种重试分开：传输归上游适配器那一层，
		// 这里给了 Backoff 建议也不该产出追问消息。
		tr := sess.Diagnose(nil, nil, Evidence{
			ToolsDeclared: 1, ToolsSent: 1, TransportError: true,
		})
		rc, err := sess.Recover(tr, nil, nil, []string{"exec"}, Evidence{})
		if err != nil {
			t.Fatal(err)
		}
		if rc.ShouldRetry || len(rc.Messages) > 0 {
			t.Errorf("传输失败不该在格式层重试：%+v", rc)
		}
	})
}

// 正常回答不算拒绝：tool_choice=auto 时模型不调用是完全合法的，
// 对它追问就是循环施压。
func TestPlainAnswerIsNotRefused(t *testing.T) {
	b, _ := New(Config{})
	sess, _ := b.NewSession("s", "r")

	d := sess.Diagnose(refusalResult("今天天气不错"), nil, Evidence{
		ToolsDeclared: 3, ToolsSent: 3, ToolChoice: "auto",
		ModelText: "今天天气不错",
	})
	if d.Kind != string(capability.RefusalNone) {
		t.Fatalf("正常回答被判成 %q", d.Kind)
	}
	rc, err := sess.Recover(d, refusalResult("今天天气不错"), nil, []string{"f"}, Evidence{})
	if err != nil {
		t.Fatal(err)
	}
	if rc.ShouldRetry {
		t.Errorf("对正常回答追问 = 循环施压：%+v", rc)
	}
}

// 编译过的会话能解析出调用时，诊断应看到调用数而不是误判。
func TestDiagnoseSeesParsedCalls(t *testing.T) {
	b, _ := New(Config{})
	sess, _ := b.NewSession("s", "r")

	res := &Result{
		Outcome: OutcomeCallsParsed,
		Calls: []Call{{
			ID: "c1", Name: "f",
			Arguments: json.RawMessage(`{"a":1}`),
		}},
	}
	d := sess.Diagnose(res, nil, Evidence{
		ToolsDeclared: 1, ToolsSent: 1, ToolChoice: "required",
	})
	if d.Kind != "" {
		t.Errorf("产出了调用却判成 %q", d.Kind)
	}
}
