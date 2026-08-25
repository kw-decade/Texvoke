// Package observability 提供脱敏摘要与审计事件骨架。
//
// 硬要求：
//   - 工具结果、Prompt、Authorization、Cookie、环境变量和文件内容默认脱敏，
//     日志只保留摘要、哈希和错误分类；
//   - 审计事件记录「谁、何时、提出了什么调用」，但不记录不必要的秘密；
//   - 上游错误与拒绝分类必须与普通模型文本分开上报。
//
// 当前状态要说清：只有 Digest 被生产路径用到（gateway 与响应渲染拿它算
// 会话摘要与确定性 ID）。审计事件（Event / ProposalEvent / Auditor）骨架
// 就绪但**未接线**——要接的话在 gateway 的编排循环里加两处 Record。
// 执行结果的审计事件随执行层一起删除（本项目不执行工具，看不到落定状态）。
//
// 依赖：ir。
package observability
