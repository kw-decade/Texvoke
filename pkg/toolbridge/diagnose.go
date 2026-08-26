package toolbridge

import (
	"fmt"
	"strings"

	"github.com/kw-decade/Texvoke/internal/capability"
	"github.com/kw-decade/Texvoke/internal/vproto"
)

// 这一层是「为什么没拿到调用」的诊断与恢复。
//
// 它存在的直接原因写在 git 历史里：砍掉 Agent 执行层时，capability 包的
// 唯一调用方（internal/agent 的 Loop）一起被删了。那个包里有七类拒绝
// 分类器、一次性能力说明状态机和五级调用阶梯——66 个测试、95% 覆盖率，
// 却在生产路径上零调用。接真实客户端时模型说「我不能直接读取文件系统」，
// 而那句话逐字躺在 personaRefusalMarkers 词表里，没有任何人问过它。
//
// 本文件只做接线：诊断逻辑在 internal/capability，措辞在 internal/vproto
// 与 HandshakeMessageFor。这里不发明新的判断。

/* ---------- 证据 ---------- */

// Evidence 是诊断需要的观测事实，全部是基础类型（ADR 0003：公开面不泄漏
// internal 类型）。字段与 capability.Evidence 一一对应，由调用方摊开填写——
// 强迫接入方明确「我观测到了什么」，而不是丢一个对象进来让框架猜。
//
// 三个必填项用注释标出；其余零值表示「没观测到」，分类器会把它们当成
// 缺证据而不是假数据——这正是用指针语义相反的普通字段 + 显式约定换来的简单。
type Evidence struct {
	// ToolsDeclared 是客户端声明的工具数。必填：它是区分「客户端没给工具」
	// 与「模型不肯用」的第一道分岔。
	ToolsDeclared int

	// ToolsSent 是实际发给上游的工具数。与 ToolsDeclared 不等说明中间有
	// 环节删改了请求。反代通常两者相等；拿不准就填与 ToolsDeclared 相同的值。
	ToolsSent int

	// ToolChoice 是客户端要求的模式："auto" / "required" / "named" / "none"。
	ToolChoice string

	// HTTPStatus 是上游响应状态码，0 表示没拿到响应。
	HTTPStatus int

	// UpstreamErrType / UpstreamErrCode 来自上游的结构化错误，没有就留空。
	UpstreamErrType string
	UpstreamErrCode string

	// TransportError 表示连接层失败（超时、断流、DNS）。置位时其余内容
	// 字段都会被忽略——连响应都没拿到，谈不上模型拒绝。
	TransportError bool

	// HandshakeDone 表示本会话已经做过一次能力说明。
	//
	// sidecar 是无状态的，这个状态由调用方保管：上一轮 Recovery 给出的值，
	// 下一轮原样带回来。与 nonce 的往返方式一致。
	HandshakeDone bool

	// ModelText 是模型输出的可读文本。
	//
	// 它是**弱证据**：分类器只拿它做人格拒绝的关键词匹配，命中也只给
	// weak 置信度。留这个字段是因为「我不能直接读取文件系统」这类话
	// 目前没有任何硬证据能替代它。
	ModelText string

	// Attempt 是调用方当前的第几次尝试（1 起）。突破阶梯用它选强度：
	// 1 → 能力说明，2 → 运行时通知，3 → 完整示范，≥4 → 明示行动。
	// 0 视为 1。
	Attempt int

	// HasSuccessfulHistory 表示本请求的历史里已有成功送出的调用
	// （反代检测 input 中有无 tool call item）。L2 用它反驳
	// 「调用接口不可用」的自我错觉——用已发生的事实，不用评价语气。
	HasSuccessfulHistory bool

	// AgentMode 表示会话正处于 agent 工作循环：历史里有成功调用，
	// 或客户端要求必须调用。置位时零调用直接走阶梯，文本内容不再是
	// 判定输入——自然语言词表永远在补措辞漂移的漏，结构性证据不会。
	// 调用方通常直接传 HasSuccessfulHistory || (ToolChoice != "") 的结果；
	// 单独提供是为了让「模式判断」这个决策可以被显式测试。
	AgentMode bool

	// SystemRecovery 决定追问消息的投递角色：置位用 system，否则 user。
	// 网关直连上游时置位（上游方言是 chat，允许多条 system）；sidecar
	// 保持 false——它的调用方可能把追问编码回 Anthropic，那里只允许一条。
	SystemRecovery bool
}

func (e Evidence) toInternal(res *Result, parseErr error) capability.Evidence {
	calls := 0
	parseFailed := false
	parseKind := ""
	if res != nil {
		calls = len(res.Calls)
	}
	if parseErr != nil {
		parseFailed = true
		parseKind = string(KindOf(parseErr))
	}
	return capability.Evidence{
		ToolsDeclaredByClient: e.ToolsDeclared,
		ToolsSentUpstream:     e.ToolsSent,
		ToolChoiceRequested:   e.ToolChoice,
		HTTPStatus:            e.HTTPStatus,
		UpstreamErrType:       e.UpstreamErrType,
		UpstreamErrCode:       e.UpstreamErrCode,
		TransportError:        e.TransportError,
		ToolCallsParsed:       calls,
		ParseFailed:           parseFailed,
		ParseErrorKind:        parseKind,
		ModelText:             e.ModelText,
		HandshakeDone:         e.HandshakeDone,
		AgentMode:             e.AgentMode,
	}
}

/* ---------- 诊断 ---------- */

// Diagnosis 是「为什么没拿到工具调用」的结论。
//
// 字段名与 capability.Classification 对应但全部降为基础类型。Terminal 为真
// 时必须立即停手——那是上游政策或本地策略在拒绝，任何追问、格式变通、
// 换措辞都是绕过安全策略的尝试。
type Diagnosis struct {
	Kind       string   // 七类根因之一，见 pkg 文档
	Confidence string   // certain / probable / weak
	Remedy     string   // 六种处置之一
	Reasons    []string // 判定依据，进日志与审计

	Terminal bool
}

// String 返回一行摘要，供日志使用。
func (d Diagnosis) String() string {
	if d.Kind == "" {
		return "无拒绝"
	}
	return fmt.Sprintf("%s（%s，建议 %s）：%s",
		d.Kind, d.Confidence, d.Remedy, strings.Join(d.Reasons, "；"))
}

/* ---------- 恢复动作 ---------- */

// Recovery 是据诊断算出的下一步动作。
//
// ShouldRetry 为假时不该再问上游——要么 Terminal（政策拒绝），要么模型
// 正常回答了不需要工具的问题，要么配额用完了。调用方应当尊重它：
// 循环施压是规格明令禁止的做法，这里给出的边界就是全部允许的范围。
type Recovery struct {
	ShouldRetry bool

	// Messages 是要追加进对话的内容。针对性追加，不是重发同一个 prompt。
	Messages []RecoveryMessage

	// HandshakeDone 是更新后的说明状态。ShouldRetry 为真且本轮发的是
	// 能力说明时为 true，下一轮要带进 Evidence。
	HandshakeDone bool

	// Reason 说明为什么给这个动作，供日志。
	Reason string
}

// RecoveryMessage 是一条要追加的消息。
type RecoveryMessage struct {
	Role string // "user" 或 "system"
	Text string
}

/* ---------- 会话方法 ---------- */

// Diagnose 判断「为什么没拿到工具调用」。
//
// res 与 parseErr 来自上一次 Parse 的返回；两者都为空表示连解析都没跑到
// （比如上游直接报错），此时证据里的传输层字段就是全部事实。
func (s *Session) Diagnose(res *Result, parseErr error, ev Evidence) Diagnosis {
	cls := capability.Classify(ev.toInternal(res, parseErr))
	return Diagnosis{
		Kind:       string(cls.Kind),
		Confidence: string(cls.Confidence),
		Remedy:     string(cls.Remedy),
		Reasons:    cls.Reasons,
		Terminal:   cls.Kind.Terminal(),
	}
}

// Recover 据诊断算出下一步动作。
//
// 对人格类拒绝（模型误以为自己不能执行 / 给了计划却没发调用），追问强度
// 按 **阶梯** 递进——诊断只决定「要不要追」，深度由 attempt 与 handshake
// 状态共同决定。这是 2026-08-24 的设计升级，理论依据是 ISC/TVD 研究
// （github.com/wuyoscar/Internal-Safety-Collapse）：模型对「请求」会拒绝、
// 对「环境报错」会本能地修，所以手段从劝说体逐级换成运行时通知体：
//
//	L1 能力说明（规格七章文案）→ L2 运行时通知 → L3 完整示范 → L4 明示行动
//
// attempt 来自调用方的尝试计数；handshakeDone 为真（能力说明已在本会话
// 发过）时整体前移一级，保证同一会话内 L1 只出现一次。走到哪级都保留
// 出口句——「判断任务不该做就直接说明理由」，这不是无限施压循环。
//
// RemedyRepairFormat（发了信号但写错）不走阶梯：模型已理解协议，把具体
// 错误翻译给它即可。其余 Remedy 一律不重试：FixClient 要人改配置，
// Backoff 是传输层的事（红线 8），FailSafe 与 Downgrade 意味着到此为止。
//
// 追问的投递角色由 ev.SystemRecovery 决定：置位时用 system——「运行时通知」
// 必须真的是运行时的嗓门。2026-08-26 长程实测发现 user 角色会毁掉这套设计：
// 结果回填轮的历史以 user 消息（工具结果）结尾，追问再以 user 追加，
// 弱模型把整个序列读成「用户在连续说话」，继续对话续写而不是响应系统事件，
// L1-L4 五连全部无效。system 角色让「环境在报错」从内容变成结构。
// 默认 false 保持 sidecar 的无状态兼容（它的调用方可能编码回 Anthropic，
// 那里只允许一条 system）；网关直连上游时置位。
func (s *Session) Recover(d Diagnosis, res *Result, parseErr error, tools []string, ev Evidence) (Recovery, error) {
	role := "user"
	if ev.SystemRecovery {
		role = "system"
	}
	switch d.Remedy {
	case "capability_handshake", "no_call_hint":
		// 阶梯深度就是调用方传来的尝试计数，这里不做二次推算：单请求内
		// attempt 自然递增；跨请求的「从上次进度继续」由调用方（反代）的
		// 会话状态负责——它记住上次用到的级数，把新请求的 Attempt 从那里
		// 接着编。sidecar 无状态，不该也不能替调用方记这件事。
		level := ev.Attempt
		if level < 1 {
			level = 1
		}
		switch {
		case level <= 1:
			msg := capability.HandshakeMessageFor(tools)
			return Recovery{
				ShouldRetry: true,
				Messages:    []RecoveryMessage{{Role: role, Text: msg}},
				// 角色由 ev.SystemRecovery 决定（见函数注释）：网关直连上游时
				// 用 system 让能力说明以运行时身份出现。
				HandshakeDone: true,
				Reason:        "L1：模型误以为自己不能执行，做一次诚实的能力说明",
			}, nil

		case level == 2:
			hint, err := vproto.NoCallHint(s.nonce, briefTools(tools), ev.HasSuccessfulHistory)
			if err != nil {
				return Recovery{}, err
			}
			return Recovery{
				ShouldRetry: true,
				Messages:    []RecoveryMessage{{Role: role, Text: hint.Text}},
				Reason:      "L2：运行时通知——陈述信号缺失的事实",
			}, nil

		case level == 3:
			hint, err := vproto.CallExampleHint(s.nonce, briefTools(tools))
			if err != nil {
				return Recovery{}, err
			}
			return Recovery{
				ShouldRetry: true,
				Messages:    []RecoveryMessage{{Role: role, Text: hint.Text}},
				Reason:      "L3：附完整可照抄的调用示例",
			}, nil

		default:
			hint, err := vproto.DirectActionHint(s.nonce, briefTools(tools))
			if err != nil {
				return Recovery{}, err
			}
			return Recovery{
				ShouldRetry: true,
				Messages:    []RecoveryMessage{{Role: role, Text: hint.Text}},
				Reason:      "L4：明示该任务需要一次工具操作",
			}, nil
		}

	case "repair_format":
		failure := failureFrom(res, parseErr, tools)
		hint, err := vproto.RepairHintFor(failure)
		if err != nil {
			return Recovery{}, err
		}
		return Recovery{
			ShouldRetry: true,
			Messages:    []RecoveryMessage{{Role: role, Text: hint.Text}},
			Reason:      "把具体的格式错误反馈给模型",
		}, nil

	default:
		return Recovery{Reason: "这类根因没有本地可做的恢复动作：" + d.Remedy}, nil
	}
}

// briefTools 把工具名列表转成 vproto 需要的形态。全部按 JSON 形态处理：
// 名单在这里只用于「告诉模型有哪些可选」，裸文本与否由 Instructions 那
// 边负责教，追问里不需要重复。
func briefTools(tools []string) []vproto.ToolBrief {
	out := make([]vproto.ToolBrief, 0, len(tools))
	for _, n := range tools {
		out = append(out, vproto.ToolBrief{Name: n})
	}
	return out
}

// failureFrom 把解析结果翻译成 RepairHint 需要的事实。
func failureFrom(res *Result, parseErr error, tools []string) *vproto.ParseFailure {
	f := &vproto.ParseFailure{KnownTools: tools}
	if parseErr != nil {
		k := KindOf(parseErr)
		if k == ErrTruncated {
			f.Truncated = true
		}
	}
	if res == nil {
		return f
	}
	if res.Outcome == OutcomeTruncated {
		f.Truncated = true
	}
	for _, c := range res.Calls {
		if len(res.UnknownTools) > 0 && c.Name == res.UnknownTools[0] {
			f.UnknownTool = c.Name
		}
	}
	// 双信号：malformed 且错误文本提到第二次出现。字符串匹配不理想，
	// 但内部包用的是 fmt.Errorf 而非哨兵错误（errors.go 同处注释），
	// 在门面里收口正是那层注释说的去处。
	if parseErr != nil && strings.Contains(parseErr.Error(), "出现了第二次") {
		f.DoubleSignal = true
	}
	if !f.Truncated && !f.DoubleSignal && f.UnknownTool == "" {
		f.BadArguments = parseErr != nil || res.Outcome == OutcomeMalformed
	}
	return f
}
