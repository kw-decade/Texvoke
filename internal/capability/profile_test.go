package capability

import (
	"strings"
	"testing"
)

// 零值必须是「未判定」而不是某个真实层级。int 枚举的零值会落在第一个
// 常量上，一个忘记初始化的档案会被当成那一档放行——这里刻意把零值
// 留成无效值。
func TestLevelZeroValueIsInvalid(t *testing.T) {
	var l Level
	if l.Valid() {
		t.Error("Level 零值不应有效")
	}
	if l != LevelUnknown {
		t.Errorf("零值为 %d，期望 LevelUnknown", l)
	}
	if l.String() != "未判定" {
		t.Errorf("零值的名称为 %q", l.String())
	}

	for _, valid := range []Level{
		LevelNativePassthrough, LevelStructuredOutput, LevelVirtualProtocol,
		LevelSlotFilling, LevelControllerOnly,
	} {
		if !valid.Valid() {
			t.Errorf("%v 应当有效", valid)
		}
		if valid.String() == "未判定" {
			t.Errorf("%d 缺少可读名称", valid)
		}
	}
	if Level(99).Valid() {
		t.Error("越界的层级不应有效")
	}
}

func baseProfile() Profile {
	return Profile{
		ClientProtocol: "chat",
		ToolsReceived:  3,
		ToolChoice:     ToolChoiceAdvisory,
	}
}

// 判定顺序即降级顺序：先看能不能原生透传，不行再看能不能约束结构，
// 再不行才用虚拟协议，最后才是逐槽引导。
func TestSelectLevel(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Profile)
		want   Level
	}{
		{
			"原生接受并返回调用",
			func(p *Profile) {
				p.UpstreamAcceptsNativeTools = true
				p.UpstreamReturnsNativeCalls = true
			},
			LevelNativePassthrough,
		},
		{
			// 接受字段却从不返回调用：透传是徒劳的。
			"接受工具声明但从不返回调用",
			func(p *Profile) { p.UpstreamAcceptsNativeTools = true },
			LevelVirtualProtocol,
		},
		{
			"支持严格 Schema",
			func(p *Profile) { p.SupportsStrictSchema = true },
			LevelStructuredOutput,
		},
		{
			"支持 Grammar",
			func(p *Profile) { p.SupportsGrammar = true },
			LevelStructuredOutput,
		},
		{
			"只支持 JSON mode",
			func(p *Profile) { p.SupportsJSONMode = true },
			LevelStructuredOutput,
		},
		{
			"纯文本上游",
			func(p *Profile) {},
			LevelVirtualProtocol,
		},
		{
			"格式经常出错",
			func(p *Profile) { p.ModelFormatReliability = ReliabilityShaky },
			LevelSlotFilling,
		},
		{
			// 格式极不稳定时，无论上游支持什么都不该让它一次输出完整结构。
			"格式极不稳定压过原生能力",
			func(p *Profile) {
				p.UpstreamAcceptsNativeTools = true
				p.UpstreamReturnsNativeCalls = true
				p.ModelFormatReliability = ReliabilityPoor
			},
			LevelSlotFilling,
		},
		{
			"没有工具",
			func(p *Profile) { p.ToolsReceived = 0 },
			LevelControllerOnly,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := baseProfile()
			tc.mutate(&p)
			got, reasons := p.SelectLevel()
			if got != tc.want {
				t.Errorf("判定为 %v，期望 %v\n理由：%v", got, tc.want, reasons)
			}
			// 每一档都必须给出理由——「为什么这次走了 L3 而不是 L1」
			// 是运维排查时最先要问的问题。
			if len(reasons) == 0 {
				t.Error("未给出判定理由")
			}
			for _, r := range reasons {
				if strings.TrimSpace(r) == "" {
					t.Error("理由中有空串")
				}
			}
		})
	}
}

// 路由可能改写时要在理由里提醒核对，否则透传路径上的工具丢失会被
// 误当成模型的问题。
func TestSelectLevelWarnsAboutRouter(t *testing.T) {
	p := baseProfile()
	p.UpstreamAcceptsNativeTools = true
	p.UpstreamReturnsNativeCalls = true
	p.RouterMayRewrite = true

	level, reasons := p.SelectLevel()
	if level != LevelNativePassthrough {
		t.Fatalf("层级为 %v", level)
	}
	joined := strings.Join(reasons, " ")
	if !strings.Contains(joined, "路由") {
		t.Errorf("理由中未提醒核对路由改写：%v", reasons)
	}
}

// tool_choice=required 是不是硬保证，取决于上游有没有真正的约束能力。
// 只是虚拟 Prompt 的话它只能记为 requested requirement——规格七章
// 明确禁止在这种情况下伪造一个调用来满足它。
func TestGuaranteesToolCall(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Profile)
		want   bool
	}{
		{
			"上游硬约束且返回原生调用",
			func(p *Profile) {
				p.ToolChoice = ToolChoiceEnforced
				p.UpstreamAcceptsNativeTools = true
				p.UpstreamReturnsNativeCalls = true
			},
			true,
		},
		{
			"声称硬约束但不返回原生调用",
			func(p *Profile) { p.ToolChoice = ToolChoiceEnforced },
			false,
		},
		{
			"只是建议",
			func(p *Profile) {
				p.ToolChoice = ToolChoiceAdvisory
				p.UpstreamAcceptsNativeTools = true
				p.UpstreamReturnsNativeCalls = true
			},
			false,
		},
		{
			"完全不支持",
			func(p *Profile) { p.ToolChoice = ToolChoiceUnsupported },
			false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := baseProfile()
			tc.mutate(&p)
			if got := p.GuaranteesToolCall(); got != tc.want {
				t.Errorf("GuaranteesToolCall() = %v，期望 %v", got, tc.want)
			}
		})
	}
}

func TestProfileValidate(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Profile)
		want   string
	}{
		{"合规", func(*Profile) {}, ""},
		{"缺协议", func(p *Profile) { p.ClientProtocol = "" }, "缺少 client_protocol"},
		{"工具数为负", func(p *Profile) { p.ToolsReceived = -1 }, "不能为负"},
		{"tool_choice 未设置", func(p *Profile) { p.ToolChoice = "" }, "支持程度非法"},
		{"tool_choice 拼错", func(p *Profile) { p.ToolChoice = "yes" }, "支持程度非法"},
		{
			// 会返回结构化调用却声称不接受工具声明，说明档案是拼凑的
			// 而不是观测出来的。
			"自相矛盾的原生能力",
			func(p *Profile) { p.UpstreamReturnsNativeCalls = true },
			"自相矛盾",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := baseProfile()
			tc.mutate(&p)
			err := p.Validate()
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("期望通过，却报错：%v", err)
			case tc.want != "" && err == nil:
				t.Errorf("期望报错包含 %q，却通过了", tc.want)
			case tc.want != "" && !strings.Contains(err.Error(), tc.want):
				t.Errorf("错误信息 %q 未包含 %q", err.Error(), tc.want)
			}
		})
	}
}

// Summary 会进日志，日志默认脱敏，所以它不得携带任何标识符。
func TestProfileSummaryIsRedacted(t *testing.T) {
	p := baseProfile()
	p.TenantID = "tenant-secret-12345"
	p.ClientProtocolVersion = "v1.2.3"
	p.UpstreamAcceptsNativeTools = true
	p.UpstreamReturnsNativeCalls = true

	s := p.Summary()
	if strings.Contains(s, "tenant-secret-12345") {
		t.Errorf("摘要泄露了租户标识：%s", s)
	}
	// 该有的诊断信息要在。
	for _, want := range []string{"protocol=chat", "tools=3", "level=", "native-tools"} {
		if !strings.Contains(s, want) {
			t.Errorf("摘要中缺少 %q：%s", want, s)
		}
	}
}

// Bridge 与 Managed Agent 是两种模式，摘要里必须能一眼看出是哪种——
// 它决定了执行器、策略与审批是否参与。
func TestProfileSummaryShowsMode(t *testing.T) {
	p := baseProfile()
	if !strings.Contains(p.Summary(), "bridge") {
		t.Errorf("未标出 bridge 模式：%s", p.Summary())
	}
	p.RuntimeMayExecute = true
	if !strings.Contains(p.Summary(), "managed") {
		t.Errorf("未标出 managed 模式：%s", p.Summary())
	}
}

// 可靠性的零值是「未知」而不是「良好」——默认乐观会让第一次请求
// 就撞上解析失败。
func TestReliabilityZeroValue(t *testing.T) {
	var r Reliability
	if r != ReliabilityUnknown {
		t.Errorf("零值为 %q，期望未知", r)
	}
	if r == ReliabilityGood {
		t.Error("零值不该等同于良好")
	}

	// 未知可靠性时走虚拟协议，而不是因为「大概没问题」就上原生透传。
	p := baseProfile()
	if level, _ := p.SelectLevel(); level != LevelVirtualProtocol {
		t.Errorf("未知可靠性 + 纯文本上游应走虚拟协议，实际为 %v", level)
	}
}
