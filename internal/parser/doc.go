// Package parser 实现增量状态机解析器，把模型的文本输出还原成
// ToolCallProposal，并对解析失败做分类。
//
// 硬要求：
//   - 必须是真正的状态机，不得把完整响应拼接后用正则处理；
//   - 必须正确处理被拆开的 UTF-8 字节、SSE 行、CRLF/LF 差异和未知事件；
//   - 触发信号前只提交已确认安全的普通文本，保留至少
//     signal_length + safety_margin 的 tentative 窗口；
//   - 一旦进入工具候选状态，工具语法不得再作为普通文本发给客户端；
//   - 解析失败必须是显式的 parse_error，绝不生成可执行的 _raw 兜底参数。
//
// 负责：tentative/committed 提交边界、深度与大小限制、失败分类。
// 不负责：Schema 实例校验（尚未实现）、重试决策
// （internal/gateway 与 pkg/toolbridge）。
//
// 依赖：ir、vproto。
package parser
