// Package protocol 实现三种上游/下游线上协议与 Canonical Tool IR 之间的
// 双向转换：OpenAI Chat Completions、OpenAI Responses、Anthropic Messages，
// 以及各自的 SSE 流式渲染。
//
// 硬要求：真实 call_id / tool_use_id / item_id 必须往返保持一致；
// 流式事件中的 ID 与最终响应体中的 ID 必须相同，不得每次渲染重新生成。
// 不得把 Responses 的 function_call_output 降级成 name=unknown 的普通文本。
//
// 负责：请求解码、响应编码、SSE 事件序列、未知字段的保留或显式丢弃。
// 不负责：解析模型文本里的虚拟工具协议（parser）、上游选路（internal/gateway）。
//
// 上游侧的流式累积器有三份（Chat / Anthropic / Responses），但当前只有
// Chat 那份被 internal/upstream 用到；另两份是为 responses-native /
// anthropic-native 上游预留的对称面（零生产调用方，是有据保留，不是忘了删）。
//
// 依赖：ir。
package protocol
