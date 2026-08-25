package capability

import (
	"fmt"
	"strings"
)

// Level 是调用阶梯的层级，从高到低降级。规格六章。
type Level int

const (
	// LevelUnknown 是零值：还没做过能力判定。它不是「最低能力」——
	// 用 int 枚举时零值落在第一个常量上，所以这里刻意把零值留成无效值，
	// 免得一个忘记初始化的档案被当成某个真实层级放行。
	LevelUnknown Level = iota

	// LevelNativePassthrough：上游与客户端都支持原生结构化调用，尽量透传。
	// 只做字段归一化、ID 关联、Schema 与策略校验，不额外包一层虚拟协议。
	LevelNativePassthrough

	// LevelStructuredOutput：上游能约束结构但不返回标准 tool call，
	// 要求它输出单一 envelope。
	LevelStructuredOutput

	// LevelVirtualProtocol：纯文本上游，用会话级 nonce 加结构化 envelope。
	LevelVirtualProtocol

	// LevelSlotFilling：模型无法稳定输出完整结构，由 Runtime 控制状态机，
	// 每轮只让它做一个小决定。
	LevelSlotFilling

	// LevelControllerOnly：确定性控制器、人工确认或安全失败。
	LevelControllerOnly
)

// String 返回层级的可读名称。
func (l Level) String() string {
	switch l {
	case LevelNativePassthrough:
		return "L1-原生透传"
	case LevelStructuredOutput:
		return "L2-结构化输出"
	case LevelVirtualProtocol:
		return "L3-虚拟协议"
	case LevelSlotFilling:
		return "L4-槽位填充"
	case LevelControllerOnly:
		return "L5-确定性控制器"
	default:
		return "未判定"
	}
}

// Valid 报告 l 是否是已判定的层级。零值永远无效。
func (l Level) Valid() bool {
	return l >= LevelNativePassthrough && l <= LevelControllerOnly
}

// ToolChoiceSupport 描述上游对强制工具选择的支持程度。
//
// 这个区分是规格七章第 4 节的要求：如果只是虚拟 Prompt，
// tool_choice=required 只能记为 requested requirement，不是保证。
type ToolChoiceSupport string

const (
	// ToolChoiceUnsupported：上游完全不认这个字段。
	ToolChoiceUnsupported ToolChoiceSupport = "unsupported"
	// ToolChoiceAdvisory：上游接受字段但不保证遵守，只能当建议。
	ToolChoiceAdvisory ToolChoiceSupport = "advisory"
	// ToolChoiceEnforced：上游有硬约束，能保证至少一个调用。
	ToolChoiceEnforced ToolChoiceSupport = "enforced"
)

// Profile 是一次请求的能力档案。
//
// 规格六章的硬要求：不能只根据模型名称猜能力。所有字段记的都是**这次请求
// 实际观测到的事实**——模型名可以骗人（同一个名字背后的后端随时会换），
// 而「客户端到底发了几个工具」「上游到底返回了什么形状」不会。
type Profile struct {
	// ClientProtocol 是客户端使用的协议（chat / responses / anthropic）。
	ClientProtocol string
	// ClientProtocolVersion 是可选的版本标识。
	ClientProtocolVersion string

	// ToolsReceived 是实际从客户端收到的工具数。
	// 为 0 是一个必须向上报告的信号，不是可以用 Prompt 弥补的缺陷。
	ToolsReceived int

	// UpstreamAcceptsNativeTools 表示上游接受结构化 tools 字段，
	// UpstreamReturnsNativeCalls 表示它真的会返回结构化调用。
	// 两者要分开：有些上游接受字段却从不使用，那时透传是徒劳的。
	UpstreamAcceptsNativeTools bool
	UpstreamReturnsNativeCalls bool

	SupportsJSONMode     bool
	SupportsStrictSchema bool
	SupportsGrammar      bool
	SupportsParallelCall bool
	SupportsStreaming    bool

	ToolChoice ToolChoiceSupport

	// RouterMayRewrite 表示请求会经过可能改写字段的中间层（如 CCS）。
	// 为真时，工具数不一致要优先怀疑路由而不是模型。
	RouterMayRewrite bool

	// RuntimeMayExecute 区分两种模式：为真是 Managed Agent（Runtime 自己
	// 执行工具），为假是 Bridge（只把调用建议转回给客户端）。
	// 这个区分决定了执行器、策略与审批是否参与，不能靠猜。
	RuntimeMayExecute bool

	// TenantID 与 ApprovalRequired 供策略层使用。
	TenantID         string
	ApprovalRequired bool

	// ModelFormatReliability 是对该上游输出格式稳定性的观测评级。
	// 它来自实际的解析成功率统计，不是对模型名的印象。
	ModelFormatReliability Reliability
}

// Reliability 是对上游输出格式稳定性的评级。
type Reliability string

const (
	// ReliabilityUnknown：尚无观测数据。这是零值，会被当作「不确定」
	// 而非「良好」——默认乐观会让第一次请求就撞上解析失败。
	ReliabilityUnknown Reliability = ""
	// ReliabilityGood：能稳定输出合法结构。
	ReliabilityGood Reliability = "good"
	// ReliabilityShaky：经常格式出错，但能被有限修复挽回。
	ReliabilityShaky Reliability = "shaky"
	// ReliabilityPoor：无法稳定输出完整结构，需要逐槽引导。
	ReliabilityPoor Reliability = "poor"
)

// SelectLevel 依据档案选择调用阶梯。
//
// 判定顺序即降级顺序：先看能不能原生透传，不行再看能不能约束结构，
// 再不行才用虚拟协议，最后才是逐槽引导与确定性控制器。
//
// 每一档都返回选择理由，因为「为什么这次走了 L3 而不是 L1」是运维排查时
// 最先要问的问题，事后从代码里反推既慢又容易反推错。
func (p Profile) SelectLevel() (Level, []string) {
	// 没有工具就没有阶梯可言。这不是能力问题，是配置问题，
	// 交给拒绝分类去报 client_capability_missing。
	if p.ToolsReceived == 0 {
		return LevelControllerOnly, []string{
			"客户端未声明任何工具，无调用可编排",
		}
	}

	// 格式极不稳定时，无论上游支持什么都不该让它一次输出完整结构。
	if p.ModelFormatReliability == ReliabilityPoor {
		return LevelSlotFilling, []string{
			"观测到该上游无法稳定输出完整结构",
			"改由 Runtime 控制状态机，每轮只让模型做一个小决定",
		}
	}

	if p.UpstreamAcceptsNativeTools && p.UpstreamReturnsNativeCalls {
		reasons := []string{"上游原生接受工具声明并返回结构化调用"}
		if p.RouterMayRewrite {
			reasons = append(reasons, "路由可能改写字段，透传后需要核对工具数与 ID")
		}
		return LevelNativePassthrough, reasons
	}

	if p.UpstreamAcceptsNativeTools && !p.UpstreamReturnsNativeCalls {
		// 接受字段却从不返回调用：透传是徒劳的，得自己编译协议。
		return LevelVirtualProtocol, []string{
			"上游接受 tools 字段但未观测到它返回结构化调用",
			"透传无效，改用文本协议编译",
		}
	}

	if p.SupportsStrictSchema || p.SupportsGrammar {
		reasons := []string{}
		if p.SupportsStrictSchema {
			reasons = append(reasons, "上游支持严格 Schema 约束")
		}
		if p.SupportsGrammar {
			reasons = append(reasons, "上游支持 Grammar 约束")
		}
		reasons = append(reasons, "要求输出单一 envelope，解析后仍需完整校验")
		return LevelStructuredOutput, reasons
	}

	if p.SupportsJSONMode {
		return LevelStructuredOutput, []string{
			"上游支持 JSON mode，可约束输出为合法 JSON",
			"但不保证符合具体 Schema，解析后必须完整校验",
		}
	}

	if p.ModelFormatReliability == ReliabilityShaky {
		return LevelSlotFilling, []string{
			"上游无结构约束能力，且观测到格式经常出错",
			"逐槽引导比一次性输出更可靠，代价是请求次数增加",
		}
	}

	return LevelVirtualProtocol, []string{
		"上游是纯文本接口，无原生调用与结构约束能力",
		"使用会话级 nonce 加结构化 envelope",
	}
}

// Validate 检查档案自身是否自洽。
func (p Profile) Validate() error {
	if p.ClientProtocol == "" {
		return fmt.Errorf("capability: 档案缺少 client_protocol")
	}
	if p.ToolsReceived < 0 {
		return fmt.Errorf("capability: 工具数不能为负，实际为 %d", p.ToolsReceived)
	}
	switch p.ToolChoice {
	case ToolChoiceUnsupported, ToolChoiceAdvisory, ToolChoiceEnforced:
	default:
		return fmt.Errorf("capability: tool_choice 支持程度非法：%q", p.ToolChoice)
	}
	// 自相矛盾的声明必须挡住：会返回结构化调用，却声称不接受工具声明，
	// 说明档案是拼凑出来的而不是观测出来的。
	if p.UpstreamReturnsNativeCalls && !p.UpstreamAcceptsNativeTools {
		return fmt.Errorf("capability: 上游声称返回原生调用却不接受工具声明，档案自相矛盾")
	}
	return nil
}

// GuaranteesToolCall 报告在当前能力下，tool_choice=required 是否是硬保证。
//
// 规格七章第 4 节：如果只是虚拟 Prompt，required 只能记为
// requested requirement，不是保证；模型没调用时必须返回明确状态，
// 绝不能伪造一个调用来满足它。这个方法就是那条区分的判据。
func (p Profile) GuaranteesToolCall() bool {
	return p.ToolChoice == ToolChoiceEnforced && p.UpstreamReturnsNativeCalls
}

// Summary 返回一行摘要，供脱敏日志使用。
//
// 刻意不含 TenantID 与任何标识符——这行会进日志，而日志默认脱敏。
func (p Profile) Summary() string {
	level, _ := p.SelectLevel()
	var flags []string
	if p.UpstreamAcceptsNativeTools {
		flags = append(flags, "native-tools")
	}
	if p.UpstreamReturnsNativeCalls {
		flags = append(flags, "native-calls")
	}
	if p.SupportsStrictSchema {
		flags = append(flags, "strict-schema")
	}
	if p.SupportsStreaming {
		flags = append(flags, "stream")
	}
	if p.RouterMayRewrite {
		flags = append(flags, "router-rewrite")
	}
	if p.RuntimeMayExecute {
		flags = append(flags, "managed")
	} else {
		flags = append(flags, "bridge")
	}
	if len(flags) == 0 {
		flags = append(flags, "none")
	}
	return fmt.Sprintf("protocol=%s tools=%d level=%s choice=%s [%s]",
		p.ClientProtocol, p.ToolsReceived, level, p.ToolChoice, strings.Join(flags, ","))
}
