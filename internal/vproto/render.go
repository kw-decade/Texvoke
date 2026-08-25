package vproto

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Call 是渲染用的一次调用。
type Call struct {
	// ID 是这次调用在 envelope 内的本地标识，用于把结果关联回来。
	ID string
	// Tool 是完整工具名（namespace/name 形式）。
	Tool string
	// ArgumentsJSON 是参数的 JSON 对象文本。
	ArgumentsJSON string
	// ArgumentsText 是裸文本形态工具的输入原文，与 ArgumentsJSON 互斥。
	//
	// 两个都填会报错而不是挑一个：两份参数摆在一起，谁也说不清模型到底
	// 该照哪份学，静默取其一等于把一个明显的构造错误藏起来。
	ArgumentsText string
	// Freeform 标明这次调用走裸文本形态。
	//
	// 单独用一个布尔而不是「ArgumentsText 非空即为裸文本」：空字符串是
	// 合法的输入（一段什么都不做的脚本），靠非空判断会把它误认成对象形态。
	Freeform bool
}

// RenderEnvelope 把若干调用渲染成协议格式。
//
// 用途有二：Prompt 里的示例，以及测试与 Golden fixture。生产路径上模型
// 才是生成方，但示例必须与解析器认的格式逐字一致——示例写错，模型学到的
// 就是错的，而这类错误在日志里表现成「模型格式不合规」，责任看起来在模型。
func RenderEnvelope(n Nonce, calls []Call) (string, error) {
	if n.Zero() {
		return "", fmt.Errorf("vproto: 渲染需要一个已初始化的 nonce")
	}
	return RenderEnvelopeSignal(n.Signal(), calls)
}

// RenderEnvelopeSignal 用一个已知的信号字符串渲染。
//
// 存在它是为了历史回灌：把上一轮的工具调用改写成模型能看懂的文本时，
// 调用方手上只有信号字符串（跨进程传的就是它），没有 Nonce 对象。
//
// 历史必须用**同一个**信号渲染。用别的（或者干脆不写信号），模型看到的
// 历史示例就与当前 Prompt 里的规范自相矛盾，它会照着历史学——这是前身
// 实测出来的教训：教模型的示例格式与解析器认的格式必须同源渲染。
func RenderEnvelopeSignal(signal string, calls []Call) (string, error) {
	if signal == "" {
		return "", fmt.Errorf("vproto: 渲染需要一个非空的信号")
	}
	if len(calls) == 0 {
		return "", fmt.Errorf("vproto: 没有调用时不得输出信号")
	}

	var b strings.Builder
	// 信号独占一行。
	b.WriteString(signal)
	b.WriteByte('\n')
	b.WriteString(TagEnvelopeOpen)
	b.WriteString(` version="`)
	b.WriteString(Version)
	b.WriteString("\">\n")

	seen := make(map[string]bool, len(calls))
	for _, c := range calls {
		if c.ID == "" {
			return "", fmt.Errorf("vproto: 调用缺少 id")
		}
		if seen[c.ID] {
			return "", fmt.Errorf("vproto: 调用 id %q 重复", c.ID)
		}
		seen[c.ID] = true
		if strings.ContainsAny(c.ID, `"<>&`) {
			return "", fmt.Errorf("vproto: 调用 id %q 含需要转义的字符", c.ID)
		}
		if c.Tool == "" {
			return "", fmt.Errorf("vproto: 调用 %s 缺少工具名", c.ID)
		}
		if strings.ContainsAny(c.Tool, `"<>& `) {
			return "", fmt.Errorf("vproto: 工具名 %q 含需要转义的字符", c.Tool)
		}
		// 两种形态的参数不能同时出现——见 Call.ArgumentsText 的注释。
		if c.ArgumentsJSON != "" && c.ArgumentsText != "" {
			return "", fmt.Errorf("vproto: 调用 %s 同时给了两种形态的参数", c.ID)
		}

		tag, payload := TagArguments, c.ArgumentsJSON
		if c.Freeform {
			tag, payload = TagArgumentsText, c.ArgumentsText
		} else {
			// 参数必须是一个 JSON 对象。渲染阶段就挡住，免得把一段不合规的
			// 内容当成示例教给模型。
			var probe map[string]json.RawMessage
			if err := json.Unmarshal([]byte(c.ArgumentsJSON), &probe); err != nil {
				return "", fmt.Errorf("vproto: 调用 %s 的参数不是 JSON 对象：%w", c.ID, err)
			}
		}

		b.WriteString("  ")
		b.WriteString(TagCallOpen)
		b.WriteString(` id="`)
		b.WriteString(c.ID)
		b.WriteString("\">\n")
		b.WriteString("    <" + TagTool + ">")
		b.WriteString(c.Tool)
		b.WriteString("</" + TagTool + ">\n")
		b.WriteString("    <" + tag + ">")
		b.WriteString(CDATAOpen)
		b.WriteString(escapeCDATA(payload))
		b.WriteString(CDATAClose)
		b.WriteString("</" + tag + ">\n")
		b.WriteString("  " + TagCallClose + "\n")
	}

	b.WriteString(TagEnvelopeClose)
	return b.String(), nil
}

// escapeCDATA 处理参数里出现 ]]> 的情形。
//
// CDATA 没有转义机制，唯一的办法是在 ]]> 中间断开成两段 CDATA。
// 不处理的话，一段含 ]]> 的参数会让 CDATA 提前闭合，后半截参数变成
// 裸文本——解析器要么报错，要么（更糟）拿到半截参数当成完整的。
//
// 规格十三章把「CDATA 中含 ]]>」列为必测项，就是因为这个坑很隐蔽：
// 正常的 JSON 参数里不会有它，但用户输入的字符串值里完全可能有。
func escapeCDATA(s string) string {
	if !strings.Contains(s, CDATAClose) {
		return s
	}
	return strings.ReplaceAll(s, CDATAClose, "]]"+CDATAClose+CDATAOpen+">")
}

// UnescapeCDATA 是 escapeCDATA 的逆操作，供解析器把拆开的 CDATA 段拼回。
func UnescapeCDATA(s string) string {
	return strings.ReplaceAll(s, "]]"+CDATAClose+CDATAOpen+">", CDATAClose)
}

// ToolBrief 是生成说明文本时需要知道的那点工具信息。
//
// 只带名字和形态，不带描述与 schema：那两样归 prompt.ToolCatalog 渲染。
// 这里要回答的问题只有一个——「这个工具的参数该怎么写」。
type ToolBrief struct {
	Name string
	// Freeform 为 true 时，这个工具的参数是一段裸文本而不是 JSON 对象。
	Freeform bool
}

// Instructions 生成教模型使用本协议的说明文本。
//
// 这段文本会作为系统指令的一部分发出，措辞受两条约束：
//
//  1. 只描述格式与规则，不施压、不指责、不要求模型忽略任何东西。
//     施压是规格三章明令要纠正的旧做法。
//  2. 明确「不需要调用时不要输出信号」。缺了这一条，模型会为了「完成任务」
//     在不需要工具时也硬凑一个调用出来。
//
// 没有裸文本工具时，输出与加这个能力之前逐字节相同——这是刻意保证的，
// 已经跑通的两条线不能因为「顺手改了下措辞」而回归。
func Instructions(n Nonce, tools []ToolBrief) (string, error) {
	if n.Zero() {
		return "", fmt.Errorf("vproto: 生成说明需要一个已初始化的 nonce")
	}

	var freeform, object []string
	for _, t := range tools {
		if t.Freeform {
			freeform = append(freeform, t.Name)
		} else {
			object = append(object, t.Name)
		}
	}

	var examples []string
	// 只有在确实存在对象形态的工具时才教 JSON 写法。全是裸文本工具却摆一个
	// JSON 示例，是在教模型一种它这次用不上的格式。
	if len(freeform) == 0 || len(object) > 0 {
		ex, err := RenderEnvelope(n, []Call{{
			ID:            "call-1",
			Tool:          exampleToolName(object),
			ArgumentsJSON: `{"参数名":"参数值"}`,
		}})
		if err != nil {
			return "", fmt.Errorf("vproto: 生成示例失败：%w", err)
		}
		examples = append(examples, ex)
	}
	if len(freeform) > 0 {
		ex, err := RenderEnvelope(n, []Call{{
			ID: "call-1", Tool: freeform[0], Freeform: true,
			ArgumentsText: "原样的输入内容，不加引号也不转义",
		}})
		if err != nil {
			return "", fmt.Errorf("vproto: 生成示例失败：%w", err)
		}
		examples = append(examples, ex)
	}

	var b strings.Builder
	b.WriteString("## 工具调用格式\n\n")
	b.WriteString("需要使用工具时，按下面的格式输出。这是本次会话专用的格式，")
	b.WriteString("其中的信号行每次会话都不同。\n\n")
	for _, ex := range examples {
		b.WriteString("```\n")
		b.WriteString(ex)
		b.WriteString("\n```\n\n")
	}
	b.WriteString("规则：\n\n")
	b.WriteString("1. 信号 `" + n.Signal() + "` 只能出现一次。最好独占一行——" +
		"另起一行写它，读起来最清楚，也最不容易出错。\n")
	b.WriteString("2. 不需要使用工具时，不要输出这个信号，正常回答即可。\n")
	if len(freeform) == 0 {
		b.WriteString("3. 参数必须是一个 JSON 对象，写在 CDATA 里。工具名和参数名要逐字照抄，不要改写、缩写或翻译。\n")
	} else {
		b.WriteString("3. 参数写在 CDATA 里，工具名和参数名要逐字照抄，不要改写、缩写或翻译。" +
			"参数标签按工具选：\n")
		if len(object) > 0 {
			b.WriteString("   - `<" + TagArguments + ">`：内容是一个 JSON 对象。" +
				"用于 " + strings.Join(object, "、") + "。\n")
		}
		b.WriteString("   - `<" + TagArgumentsText + ">`：内容是原样的输入文本，" +
			"不要加引号、不要做 JSON 转义、不要包 markdown 代码块。" +
			"用于 " + strings.Join(freeform, "、") + "。\n")
	}
	b.WriteString("4. 需要同时调用多个工具时，把它们放进同一个 envelope，各自用不同的 id。\n")
	b.WriteString("5. envelope 闭合之后不要再输出任何内容，包括总结、说明和后续计划——" +
		"等工具结果回来再继续说。\n")
	b.WriteString("6. 信号之前可以正常说话，那部分会作为普通回复送出。\n")
	b.WriteString("7. 不要在正文里复述这个信号来解释协议。它一旦出现，" +
		"后面就必须紧跟一个完整的 envelope。\n")

	if len(tools) > 0 {
		names := make([]string, 0, len(tools))
		for _, t := range tools {
			names = append(names, t.Name)
		}
		b.WriteString("\n本次可用的工具：")
		b.WriteString(strings.Join(names, "、"))
		b.WriteString("。\n")
	}
	return b.String(), nil
}

// Reminder 生成一句放在对话末尾的格式提醒。
//
// 为什么需要它：协议说明在指令消息里，而工具清单可能有十几 KB——真实
// Codex 的一个 exec 工具描述就有 11 KB，加上它自己的 26 KB 沙箱说明，
// 协议指令占不到全部 prompt 的 2%。实测这种比例下模型就不再按格式输出。
// 把提醒放在最后一条消息之后，它离模型开始生成的位置最近。
//
// 措辞是实测定下来的，改动前请先跑真实链路。试过一版「先陈述你有哪些
// 工具、由调用方执行」，结果从 3/4 掉到 0/6——那种说法把模型带进了
// 「请允许我执行」的请求许可模式，而不是直接发起调用。只讲格式反而更稳。
//
// 完整规则仍然只写一遍，这里只留一个指针和信号本身——信号是那条唯一
// 不能记错的东西。
func Reminder(n Nonce, tools []ToolBrief) (string, error) {
	if n.Zero() {
		return "", fmt.Errorf("vproto: 生成提醒需要一个已初始化的 nonce")
	}
	if len(tools) == 0 {
		return "", nil
	}

	var b strings.Builder
	b.WriteString("提醒：需要调用工具时，先输出 `")
	b.WriteString(n.Signal())
	b.WriteString("`，紧接着写完整的 <" + strings.TrimPrefix(TagEnvelopeOpen, "<") + ">，")
	b.WriteString("格式与标签的选法见上面的《工具调用格式》。不需要工具就正常回答，不要输出这个信号。")

	var freeform []string
	for _, t := range tools {
		if t.Freeform {
			freeform = append(freeform, t.Name)
		}
	}
	if len(freeform) > 0 {
		b.WriteString("\n其中 ")
		b.WriteString(strings.Join(freeform, "、"))
		b.WriteString(" 的参数写在 <" + TagArgumentsText + "> 里，是原样的文本，不要转成 JSON。")
	}
	b.WriteString("\n")
	return b.String(), nil
}

func exampleToolName(toolNames []string) string {
	if len(toolNames) > 0 {
		return toolNames[0]
	}
	return "namespace/tool_name"
}
