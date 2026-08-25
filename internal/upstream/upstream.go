// Package upstream 把「把消息发给纯文本上游、把回复拿回来」这件事标准化。
//
// Texvoke 的核心（协议编解码、虚拟协议、阶梯救援）都在 Go 侧有测试网保护，
// 但「怎么发请求」一直住在接入方的胶水里——前身实现里构造 payload、
// 发请求、手工隧道代理那一段，约两百行无测试代码，是三类事故的高发层。
//
// 这个包定义一个最小的 Adapter 接口：给它归一化后的 messages，它返回模型
// 的完整文本输出。SSE 解析、payload 形状、设备指纹、重试策略全部收进各
// 实现内部，编排层（第 3 步的 gateway）只面对接口编程。
//
// 接口刻意只暴露一个方法：上游适配的本质就是「一种 payload 形状 + 一种
// SSE 方言」，其余都是细节。新接一种上游 = 写一个几十行的实现加几个单测。
package upstream

import (
	"context"

	"github.com/kw-decade/Texvoke/internal/protocol"
)

// Reply 是一次上游调用的结果。
//
// Text 是模型输出的完整文本（含可能的信号与 envelope——解析是调用方的
// 事，适配器不做任何协议判断）。Err 区分两类失败：结构化错误（HTTP 4xx
// 带错误体）装进 *protocol.UpstreamError，供拒绝分类；传输层故障
// （连接断、超时）用普通 error。两者混在一起会让「可重试」变成猜谜。
type Reply struct {
	Text string

	// Status 是 HTTP 状态码。成功时为 2xx；Text 为空且 Err 非 nil 时它
	// 说明错在哪里。
	Status int
}

// Adapter 是一种纯文本上游的形状描述。
type Adapter interface {
	// Name 返回适配器名（如 openai-chat），用于日志与配置选择。
	Name() string

	// Complete 发一轮对话，返回模型的完整文本输出。
	//
	// messages 已经过 toolbridge 处理：工具定义在 system 文本里、content
	// 已压平成字符串——适配器不需要也不应该再碰协议语义。stream 参数
	// 表示客户端是否要求流式体验：伪流式架构下所有上游都按非流式取回
	// 全文，这个参数只留给个别支持同步整段返回的上游做提示。
	Complete(ctx context.Context, model string, messages []protocol.Message) (Reply, error)

	// Models 返回上游可用的模型名清单。
	//
	// 网关对模型是透明的：客户端问有哪些模型，答案来自上游而不是网关编的。
	// 有的自定义上游没有这个概念（payload 里 model 只是标签），实现可以
	// 返回配置时写死的那个名字。
	Models(ctx context.Context) ([]string, error)
}
