package parser

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"strings"

	"github.com/kw-decade/Texvoke/internal/vproto"
)

// parseEnvelope 解析一个完整的 envelope。
//
// 用 encoding/xml 而不是正则：规格三章把「只用正则解析嵌套 XML/JSON」列为
// 必须纠正的做法。正则无法正确处理嵌套、属性顺序、CDATA 与实体，
// 而这些恰恰是模型输出里最容易出岔子的地方。
//
// 状态机负责找到 envelope 的边界（增量、有上限），这里负责解析边界内的
// 内容（一次性、输入已受 MaxEnvelopeBytes 约束）。两者分工，
// 既满足「真正的 parser」的要求，又不会无界累积。
func parseEnvelope(raw []byte, limits Limits) ([]vproto.Call, error) {
	dec := xml.NewDecoder(strings.NewReader(string(raw)))
	// 关掉实体展开：XML 实体是一条经典的放大攻击路径，而本协议不需要它。
	dec.Strict = true
	dec.Entity = map[string]string{}

	var (
		calls   []vproto.Call
		depth   int
		inEnv   bool
		cur     *vproto.Call
		curTag  string
		curText strings.Builder
		// sawArgs 记录本次 call 是否已经出现过参数标签。用它而不是判断
		// ArgumentsText 非空：空字符串是合法的裸文本参数，靠非空判断会把
		// 一个「什么都不做的脚本」误报成缺参数。
		sawArgs bool
		seenIDs = map[string]bool{}
	)

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("parser: envelope 不是合法 XML：%w", err)
		}

		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			if depth > limits.MaxDepth {
				return nil, fmt.Errorf("parser: XML 嵌套深度超过上限 %d", limits.MaxDepth)
			}

			switch t.Name.Local {
			case "tool_call_envelope":
				if inEnv {
					// 规格：多个 envelope 按歧义拒绝，不取第一个也不取最后一个。
					return nil, fmt.Errorf("parser: 出现了嵌套的 envelope")
				}
				inEnv = true
				if v := attr(t, "version"); v != "" && v != vproto.Version {
					return nil, fmt.Errorf("parser: envelope 版本为 %q，本 Runtime 使用 %q", v, vproto.Version)
				}

			case "call":
				if !inEnv {
					return nil, fmt.Errorf("parser: call 出现在 envelope 之外")
				}
				if cur != nil {
					return nil, fmt.Errorf("parser: 出现了嵌套的 call")
				}
				id := attr(t, "id")
				if id == "" {
					return nil, fmt.Errorf("parser: call 缺少 id 属性")
				}
				if seenIDs[id] {
					// 重复的 call id 会让结果无法唯一关联回调用。
					return nil, fmt.Errorf("parser: call id %q 重复", id)
				}
				seenIDs[id] = true
				if len(calls) >= limits.MaxCalls {
					return nil, fmt.Errorf("parser: 调用数超过上限 %d", limits.MaxCalls)
				}
				cur = &vproto.Call{ID: id}
				sawArgs = false

			case vproto.TagTool, vproto.TagArguments, vproto.TagArgumentsText:
				if cur == nil {
					return nil, fmt.Errorf("parser: %s 出现在 call 之外", t.Name.Local)
				}
				curTag = t.Name.Local
				curText.Reset()

			default:
				return nil, fmt.Errorf("parser: envelope 中出现未知标签 %q", t.Name.Local)
			}

		case xml.EndElement:
			depth--
			switch t.Name.Local {
			case "tool_call_envelope":
				inEnv = false

			case "call":
				if cur == nil {
					return nil, fmt.Errorf("parser: call 闭合标签没有对应的开始标签")
				}
				if cur.Tool == "" {
					return nil, fmt.Errorf("parser: call %q 缺少 %s", cur.ID, vproto.TagTool)
				}
				if !sawArgs {
					return nil, fmt.Errorf("parser: call %q 缺少 %s", cur.ID, vproto.TagArguments)
				}
				calls = append(calls, *cur)
				cur = nil

			case vproto.TagTool:
				name := strings.TrimSpace(curText.String())
				if name == "" {
					return nil, fmt.Errorf("parser: call %q 的工具名为空", cur.ID)
				}
				// 工具名逐字匹配，不做大小写折叠也不 trim 内部空白——
				// 模糊匹配会让模型输出的近似名字意外命中另一个工具。
				if strings.ContainsAny(name, " \t\n") {
					return nil, fmt.Errorf("parser: 工具名 %q 含空白字符", name)
				}
				cur.Tool = name
				curTag = ""

			case vproto.TagArguments:
				if sawArgs {
					return nil, fmt.Errorf("parser: call %q 出现了两份参数", cur.ID)
				}
				args := strings.TrimSpace(vproto.UnescapeCDATA(curText.String()))
				if len(args) > limits.MaxArgumentBytes {
					return nil, fmt.Errorf("parser: call %q 的参数 %d 字节超过上限 %d",
						cur.ID, len(args), limits.MaxArgumentBytes)
				}
				if err := validateArgumentsJSON(args, limits.MaxDepth); err != nil {
					// 解析失败必须是显式错误。规格三章：绝不把解析不出的
					// 内容塞进一个 _raw 兜底字段后继续执行。
					return nil, fmt.Errorf("parser: call %q 的参数无效：%w", cur.ID, err)
				}
				cur.ArgumentsJSON = args
				sawArgs = true
				curTag = ""

			case vproto.TagArgumentsText:
				// 裸文本参数不做 JSON 校验，也不做深度检查——它压根不是 JSON，
				// 那两项检查在这里没有意义。字节上限仍然要查：它防的是内存，
				// 与内容形态无关。
				//
				// 首尾空白仍然去掉：模型会把 CDATA 块缩进对齐，那些换行和空格
				// 是排版不是内容。对脚本类输入这一步是无害的。
				if sawArgs {
					return nil, fmt.Errorf("parser: call %q 出现了两份参数", cur.ID)
				}
				text := strings.TrimSpace(vproto.UnescapeCDATA(curText.String()))
				if len(text) > limits.MaxArgumentBytes {
					return nil, fmt.Errorf("parser: call %q 的参数 %d 字节超过上限 %d",
						cur.ID, len(text), limits.MaxArgumentBytes)
				}
				cur.ArgumentsText = text
				cur.Freeform = true
				sawArgs = true
				curTag = ""
			}

		case xml.CharData:
			if curTag != "" {
				curText.Write(t)
				if curText.Len() > limits.MaxArgumentBytes {
					return nil, fmt.Errorf("parser: 标签内容超过上限 %d", limits.MaxArgumentBytes)
				}
			}
			// envelope 与 call 之间的空白忽略；非空白内容也忽略而不报错，
			// 模型偶尔会在结构里插入换行和缩进。

		case xml.Comment, xml.ProcInst, xml.Directive:
			// 协议里不需要这些。出现了就说明输出被污染，直接拒绝比
			// 悄悄跳过安全——注释里可以藏任意内容。
			return nil, fmt.Errorf("parser: envelope 中不允许出现注释或处理指令")
		}
	}

	if inEnv {
		return nil, fmt.Errorf("parser: envelope 未闭合")
	}
	if cur != nil {
		return nil, fmt.Errorf("parser: call %q 未闭合", cur.ID)
	}
	if len(calls) == 0 {
		// 信号出现了却一个调用也没有：这是自相矛盾的输出。
		// 规格要求「无工具时不得输出信号」。
		return nil, fmt.Errorf("parser: envelope 中没有任何调用")
	}
	return calls, nil
}

func attr(e xml.StartElement, name string) string {
	for _, a := range e.Attr {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

// validateArgumentsJSON 校验参数是一个深度受限的 JSON 对象。
//
// 深度限制不是形式主义：深嵌套 JSON 是一条经典的栈耗尽路径，
// 而 encoding/json 本身对深度没有硬上限。
func validateArgumentsJSON(s string, maxDepth int) error {
	if s == "" {
		return fmt.Errorf("参数为空，无参数应写成 {}")
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal([]byte(s), &probe); err != nil {
		return fmt.Errorf("不是 JSON 对象：%w", err)
	}
	if d := jsonDepth([]byte(s), maxDepth); d > maxDepth {
		return fmt.Errorf("JSON 嵌套深度超过上限 %d", maxDepth)
	}
	return nil
}

// jsonDepth 计算 JSON 的嵌套深度，超过 limit 就提前返回。
//
// 不用递归解析：递归本身就会吃掉调用栈，而这里要防的恰恰是栈耗尽。
// 扫描括号的做法是常数栈空间的。
func jsonDepth(b []byte, limit int) int {
	depth, max := 0, 0
	inString, escaped := false, false

	for _, c := range b {
		if escaped {
			escaped = false
			continue
		}
		if inString {
			switch c {
			case '\\':
				escaped = true
			case '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{', '[':
			depth++
			if depth > max {
				max = depth
				if max > limit {
					return max
				}
			}
		case '}', ']':
			depth--
		}
	}
	return max
}
