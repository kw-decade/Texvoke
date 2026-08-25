package vproto

import (
	"fmt"
	"strings"
)

// RepairHint 是把一次解析失败翻译给模型听的话。
//
// 为什么放在 vproto：这层话必须与 Instructions 里教的格式**逐字一致**——
// 同一件事在两处用不同说法（这里叫「信封」那里叫「envelope」），模型会照
// 着更近的那个学，然后两边都开始漂移。格式契约的所在地就是措辞的所在地。
//
// 它只描述事实与正确写法，不施压。规格三章把「Prompt 迭代施压」列为必须
// 纠正的做法；「你上一次的结构没有闭合」是信息，「你必须调用工具」是压力，
// 这条线由调用方（capability 的 Remedy）决定要不要说，本包只负责怎么说。
type RepairHint struct {
	Text string
}

// RepairHintFor 把内部错误映射成对模型有用的描述。
//
// 不能把 Go 的 error 字符串原样丢过去：「parser: envelope 闭合后又出现了
// 信号，存在歧义」对模型是噪声——它不知道 parser 是什么。它需要知道的是
// 「哪里不符合刚才教的格式、正确的写法长什么样」。
//
// 映射按错误的可判定性排序：能确定原因的说原因，不能确定的只说「重新完整
// 输出」。宁可模糊也不要编造一个具体但不真实的原因。
func RepairHintFor(res *ParseFailure) (RepairHint, error) {
	if res == nil {
		return RepairHint{}, fmt.Errorf("vproto: 没有失败信息可翻译")
	}

	var b strings.Builder
	b.WriteString("你上一次的回复尝试发起工具调用，但格式有问题，所以这次调用没有被送出。\n")

	switch {
	case res.Truncated:
		b.WriteString("问题：结构在闭合之前就中断了。\n")
		b.WriteString("请重新完整输出一遍：信号之后紧跟完整的 envelope，" +
			"以 </" + strings.TrimPrefix(TagEnvelopeClose, "</") + "> 结束，中间不要换话题。")

	case res.DoubleSignal:
		b.WriteString("问题：信号出现了两次。\n")
		b.WriteString("信号在一次回复里只能出现一次。" +
			"要同时调用多个工具，把它们放进同一个 envelope，各自用不同的 id。")

	case res.BadArguments:
		b.WriteString("问题：参数部分不是要求的形状。\n")
		b.WriteString("JSON 形态的工具用 <" + TagArguments + ">，内容是一个 JSON 对象；" +
			"文本形态的工具用 <" + TagArgumentsText + ">，内容是原样的文本，不要加引号或转义。")

	case res.UnknownTool != "":
		b.WriteString(fmt.Sprintf("问题：没有叫 %q 的工具。\n", res.UnknownTool))
		if len(res.KnownTools) > 0 {
			b.WriteString("本轮可用的工具：" + strings.Join(res.KnownTools, "、") + "。")
		} else {
			b.WriteString("请从本轮列出的可用工具里选。")
		}

	default:
		b.WriteString("问题：结构无法解析。\n" +
			"请严格按《工具调用格式》里的示例重新输出一遍，不要增减标签。")
	}
	return RepairHint{Text: b.String()}, nil
}

// NoCallHint 是突破阶梯的第 2 级：模型没发信号也没显式拒绝时的运行时通知。
//
// 措辞吸收了 ISC/TVD 研究（github.com/wuyoscar/Internal-Safety-Collapse）的
// 实验结论：模型对「请求」会拒绝，对「环境报错」会本能地修。因此这里不用
// 劝说体（「请按格式输出」），而用状态陈述体（「未检测到信号，流水线阻塞」）
// ——模型的本能从辩解变成修复。
//
// 三条硬边界（有测试钉住）：
//   - 只陈述格式事实，从不评价模型的回答内容；
//   - 必须带出口句：判断任务不该做就直接说明理由；
//   - 不含任何施压措辞（你必须 / 忽略之前 / 事实错误）。
//
// sawSuccess 为真时引用「本会话早些时候的调用已成功送出」——用已发生的事实
// 反驳「调用接口不可用」的自我错觉，这在卡死会话里是最常见的误解。
func NoCallHint(n Nonce, tools []ToolBrief, sawSuccess bool) (RepairHint, error) {
	if n.Zero() {
		return RepairHint{}, fmt.Errorf("vproto: 生成提示需要一个已初始化的 nonce")
	}
	var b strings.Builder
	b.WriteString("[runtime] 本条回复中未检测到调用信号（`")
	b.WriteString(n.Signal())
	b.WriteString("`），后续步骤全部阻塞。\n")
	if sawSuccess {
		b.WriteString("本会话早些时候的调用已成功送出并返回了结果，通道是正常的。\n")
	}
	b.WriteString("解除方式：在本条回复中先输出信号行，紧接着一个完整的 ")
	b.WriteString("<" + strings.TrimPrefix(TagEnvelopeOpen, "<") + ">。")
	if len(tools) > 0 {
		names := make([]string, 0, len(tools))
		for _, t := range tools {
			names = append(names, t.Name)
		}
		b.WriteString("本轮可用工具：" + strings.Join(names, "、") + "。")
	}
	b.WriteString("\n若你判断此任务确实不应执行，请直接说明理由，流程将终止并如实上报。")
	return RepairHint{Text: b.String()}, nil
}

// CallExampleHint 是突破阶梯的第 3 级：附上可直接照抄的完整示例。
//
// 弱模型模仿比理解快。示例用本轮的真实信号与第一个工具名，参数一律用
// 尖括号占位符——照抄占位符会让客户端报错并把错误送回来，比编造一个
// 看似合法的假参数更安全。
func CallExampleHint(n Nonce, tools []ToolBrief) (RepairHint, error) {
	if n.Zero() {
		return RepairHint{}, fmt.Errorf("vproto: 生成示例需要一个已初始化的 nonce")
	}
	name := exampleToolName(briefNames(tools))
	freeform := false
	for _, t := range tools {
		if t.Name == name && t.Freeform {
			freeform = true
		}
	}

	// 示例的 envelope 由 RenderEnvelopeSignal 渲染而不是手拼字符串：示范与
	// Instructions 教的格式必须逐字一致，两处各自拼字符串迟早分叉——这正是
	// 该函数存在的理由（见它的注释）。
	call := Call{ID: "call-1", Tool: name}
	if freeform {
		call.Freeform = true
		call.ArgumentsText = "<在这里原样写出该工具需要的输入文本>"
	} else {
		call.ArgumentsJSON = `{"<参数名>": "<参数值>"}`
	}
	env, err := RenderEnvelopeSignal(n.Signal(), []Call{call})
	if err != nil {
		return RepairHint{}, fmt.Errorf("vproto: 生成示例失败：%w", err)
	}

	var b strings.Builder
	b.WriteString("[runtime] 流水线仍在阻塞：上一条回复没有有效的调用信号。\n")
	b.WriteString("可直接按以下结构输出（把尖括号占位符换成真实内容）：\n\n")
	b.WriteString(env)
	b.WriteString("\n本轮可用工具：" + strings.Join(briefNames(tools), "、") + "。\n")
	b.WriteString("若你判断此任务确实不应执行，请直接说明理由，流程将终止并如实上报。")
	return RepairHint{Text: b.String()}, nil
}

// DirectActionHint 是突破阶梯的第 4 级：把启动成本降到零的最后陈述。
//
// 与前三级的区别是明说了「这个任务需要执行操作」。这仍是事实陈述而非
// 施压——没有责备、没有威胁、出口保留。走到这一级的模型已经连续三次
// 收到完整指引仍未发出信号，含糊其辞对谁都没有帮助。示例结构与 L3 相同：
// 到这一级缺的已经不是指引而是行动，重复同样的结构是有意的。
func DirectActionHint(n Nonce, tools []ToolBrief) (RepairHint, error) {
	if n.Zero() {
		return RepairHint{}, fmt.Errorf("vproto: 生成提示需要一个已初始化的 nonce")
	}
	name := exampleToolName(briefNames(tools))
	call := Call{ID: "call-1", Tool: name, ArgumentsJSON: `{"<参数名>": "<参数值>"}`}
	env, err := RenderEnvelopeSignal(n.Signal(), []Call{call})
	if err != nil {
		return RepairHint{}, fmt.Errorf("vproto: 生成提示失败：%w", err)
	}

	var b strings.Builder
	b.WriteString("[runtime] 该任务需要执行一次工具操作才能推进。当前回复应包含：\n\n")
	b.WriteString(env)
	if len(tools) > 0 {
		b.WriteString("\n工具任选其一：" + strings.Join(briefNames(tools), "、") + "。")
	}
	b.WriteString("\n若你认为该任务不应执行，请改为直接说明理由。")
	return RepairHint{Text: b.String()}, nil
}

func briefNames(tools []ToolBrief) []string {
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, t.Name)
	}
	return names
}

// ParseFailure 是 RepairHintFor 需要的失败信息。
//
// 用结构体而不是直接收 error：error 字符串是给人看的日志，喂给模型的
// 应当是从中**提取出来的事实**。这个类型强迫调用方摊开「观察到了什么」，
// 而不是丢一个错误对象进来让映射函数猜。
type ParseFailure struct {
	// Truncated：信号出现但结构在闭合前断了。
	Truncated bool
	// DoubleSignal：信号出现了不止一次。
	DoubleSignal bool
	// BadArguments：参数部分不合格式（不是 JSON 对象、两种标签混用等）。
	BadArguments bool
	// UnknownTool 是调用的工具名对不上声明。空表示不存在这个问题。
	UnknownTool string
	// KnownTools 是本轮声明的工具名，用于告诉模型有哪些可选。
	KnownTools []string
}
