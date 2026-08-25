// examples/go_inprocess.go 演示 Go 项目的接入方式：直接用库，不走网络。
//
// 与 HTTP sidecar 的区别只在于少了一跳序列化——语义、错误分类、
// 提交边界的行为完全一致，因为两者是同一套核心。
//
// 跑法：go run ./examples/go_inprocess.go
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"strings"

	"github.com/kw-decade/Texvoke/pkg/toolbridge"
)

func main() {
	// Bridge 可以被多个 goroutine 共享，通常整个进程一个。
	bridge, err := toolbridge.New(toolbridge.Config{
		Upstream: toolbridge.UpstreamProfile{
			// 上游的品牌噪声由你按自己的上游填——把某一家的正则硬编码进
			// 框架，等于宣告它只服务那一家。
			NoiseFilters: []*regexp.Regexp{
				regexp.MustCompile(`(?m)^👋\s*你好！我是[^\n]*助手\n?`),
			},
			// 0 表示用默认值（24 个工具 / 200 字节描述），这两个数来自实测：
			// 真实的 Claude Code 带 80 多个工具，全量描述几十 KB
			// 足以把协议指令稀释到模型不再遵守。
			MaxToolsInPrompt: 0,
		},
	})
	if err != nil {
		log.Fatal(err)
	}

	// 一个会话对应客户端的一次请求。协议信号绑在它上面，整个会话内不变——
	// 提示词注入与历史回灌必须用同一个信号，否则模型看到的历史示例
	// 与当前规范自相矛盾。
	sess, err := bridge.NewSession("sess-demo", "req-demo")
	if err != nil {
		log.Fatal(err)
	}

	tools := []toolbridge.Tool{
		{
			Name:        "get_weather",
			Description: "查询指定城市的当前天气",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {"city": {"type": "string"}},
				"required": ["city"]
			}`),
		},
		{
			Name:        "read_file",
			Description: "读取一个文件的内容",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {"path": {"type": "string"}},
				"required": ["path"]
			}`),
		},
	}

	// ---- 接入点 1：工具定义 → system prompt ----
	compiled, err := sess.Compile(tools, toolbridge.CompileOptions{
		Query: "旧金山天气怎么样", // 用于候选排序
	})
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println("=== 编译结果 ===")
	fmt.Printf("信号：%s\n", compiled.Signal)
	fmt.Printf("进入 prompt 的工具：%v\n", compiled.ToolsIncluded)
	fmt.Printf("被筛掉的工具数：%d\n", compiled.ToolsDropped)
	fmt.Printf("prompt 长度：%d 字节\n\n", len(compiled.SystemPrompt))

	// 这段 prompt 要拼进发给上游的 system 消息。
	// 中间省略：把 compiled.SystemPrompt + 用户消息发给你的纯文本上游。

	// ---- 接入点 2：模型输出 → 工具调用 ----
	//
	// 这里手工拼一段"模型的回复"来演示。真实场景里它来自上游。
	// 注意开头那行品牌噪声——它会被 NoiseFilters 剥掉。
	modelOutput := "👋 你好！我是天气助手\n" +
		"我来查一下旧金山的天气。\n" +
		compiled.Signal + "\n" +
		`<tool_call_envelope version="1">
  <call id="c1">
    <tool>get_weather</tool>
    <arguments_json><![CDATA[{"city":"San Francisco"}]]></arguments_json>
  </call>
</tool_call_envelope>`

	res, err := sess.Parse(modelOutput)
	if err != nil {
		// 错误可以按分类处理：格式错误值得把具体问题反馈给模型重试一次，
		// 截断通常是上游断流或达到 token 上限，该查链路而不是调 Prompt。
		switch toolbridge.KindOf(err) {
		case toolbridge.ErrParseFailed:
			log.Printf("模型格式写错了，可以反馈后重试：%v", err)
		case toolbridge.ErrTruncated:
			log.Printf("上游断流或达到 token 上限：%v", err)
		default:
			log.Fatal(err)
		}
	}

	fmt.Println("=== 解析结果 ===")
	fmt.Printf("结局：%s\n", res.Outcome)
	fmt.Printf("要转发给客户端的正文：%q\n", res.Text)
	for _, c := range res.Calls {
		fmt.Printf("调用：id=%s name=%s args=%s\n", c.ID, c.Name, c.Arguments)
	}

	// 验证两件事：噪声被剥掉了，协议内容一个字都没漏进正文。
	fmt.Println()
	fmt.Printf("噪声已过滤：%v\n", !strings.Contains(res.Text, "天气助手"))
	fmt.Printf("正文无协议残留：%v\n",
		!strings.Contains(res.Text, compiled.Signal) &&
			!strings.Contains(res.Text, "tool_call_envelope"))

	// ---- 流式版本 ----
	//
	// 上游是流式的话用这条路径：信号出现之前的文本能边到边转发，
	// 不必等模型把话说完。
	fmt.Println("\n=== 流式解析 ===")
	sp, err := sess.NewStreamParser()
	if err != nil {
		log.Fatal(err)
	}

	var forwarded strings.Builder
	data := []byte(modelOutput)
	for i := 0; i < len(data); i += 7 { // 模拟上游按任意边界切分
		end := min(i+7, len(data))
		safe, err := sp.Write(data[i:end])
		if err != nil {
			log.Printf("解析中断：%v", err)
			break
		}
		// safe 里的字节确定不属于协议，可以立即写给客户端。
		forwarded.Write(safe)
	}
	// 配置了噪声过滤时，缓冲里可能还有最后不足一行的内容。
	forwarded.Write(sp.Flush())
	streamRes := sp.Close()

	fmt.Printf("结局：%s，调用数：%d\n", streamRes.Outcome, len(streamRes.Calls))
	fmt.Printf("流式转发出去的内容：%q\n", forwarded.String())
	fmt.Printf("转发内容无协议残留：%v\n",
		!strings.Contains(forwarded.String(), compiled.Signal))
}
