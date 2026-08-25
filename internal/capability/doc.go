// Package capability 判断「模型为什么没有发起调用」，并给出下一步该怎么问。
//
// 核心是拒绝分类（`Classify`）：七类根因——client_capability_missing、
// router_mutation、format_noncompliance、persona_refusal、
// upstream_policy_refusal、runtime_policy_denied、transport_failure——
// 处置完全不同，混为一谈会让排查指向错误方向（该修客户端配置的去改 Prompt，
// 该停手的继续追问）。判定顺序从硬证据（传输层、HTTP 状态、上游错误类型）
// 到软证据（文本关键词），关键词命中只值 Weak 置信度。
//
// 硬拒绝（upstream_policy_refusal）必须安全失败：不换措辞、不改工具名、
// 不调整封装，也不改走其它通道。对这种情况伪装成功是设计缺陷，不是功能。
//
// 本包还持有能力说明的文案（HandshakeMessage）。「同一会话只说明一次」这条
// 不变量由服务端会话状态实现（internal/serving），不在这里——早先的五态
// 状态机表达同一件事但从未被生产路径推进，2026-08-25 删除。
//
// Profile 与 SelectLevel（调用阶梯的入口选择，Level 1 原生透传 →
// Level 5 确定性控制器）当前零生产调用方：只实现了 Level 3 虚拟协议，
// 入口无条件走它。保留的理由见 ADR-0004——Level 2 结构化输出约束是接
// OpenAI 兼容中转时最高价值的下一步，这是给那件事预留的接口。
// 它记录的永远是实际观测到的事实，不凭模型名猜能力。
//
// 依赖：ir。
package capability
