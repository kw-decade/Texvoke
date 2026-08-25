# ADR-0004：调用阶梯的现状与缺口

- 日期：2026-08-23
- 状态：已接受
- 关联：ADR-0003（公开 API 边界）

## 背景

`internal/capability/profile.go` 定义了五级调用阶梯（规格六章）：

| Level | 名称 | 状态 |
|---|---|---|
| 1 | NativePassthrough 原生透传 | 未实现（当前没有原生支持工具的上游接入） |
| 2 | StructuredOutput 结构化输出约束 | **未实现，且无法验证** |
| 3 | VirtualProtocol 虚拟文本协议 | ✅ 唯一实现（pkg/toolbridge 全部能力都在这级） |
| 4 | SlotFilling 槽位填充 | 未实现（`ir.SourceSlotFilled` 有常量无实现） |
| 5 | ControllerOnly 确定性控制器 | 未实现（同上） |

`Profile.SelectLevel()` 写好了，生产路径没有任何调用方——与 capability 包
曾经的孤岛状态同病。2026-08-23 的闭环恢复工作把诊断层接回了公开面
（`pkg/toolbridge/diagnose.go`），但阶梯选择本身仍然没有接。

## 决策

### 1. 为什么现在不实现其余层级

**Level 2 无法验证。** 它依赖上游 API 支持 `response_format: {type:"json_schema"}`
之类的结构化输出约束——这是唯一能把格式可靠性从概率变成约束的手段，
价值最高。但当前唯一的上游是自定义 API（payload 只有
messages/provider/model 三个字段），不支持。写一份无法在真实上游上验证的
实现，等于交付一段不知道对不对的代码。**不写无法验证的东西。**

接 OpenAI 兼容中转时应当优先做这一级：先探测 `response_format` 是否被接受，
接受则走 JSON schema 约束 + 直接解析，跳过整个虚拟协议。那会把协议可靠性
从「教模型写格式」变成「上游 API 保证格式」。

**Level 4/5 投入产出不匹配。** 它们服务的是「连 envelope 都写不稳」的极弱
模型。当前上游在指令去重 + 闭环恢复之后，协议层失败已大幅收敛；剩余失败
集中在任务语义层（权限犹豫、任务理解），换更弱的降级路径救不了那些。
等出现一个真实的高频需求再动工。

### 2. 现在做了什么

诊断与恢复（阶梯的「失败后怎么办」半边）已经接上：

```
解析结果 + 证据 → Classify → Remedy
                              ├─ repair_format → 针对性格式提示（一次）
                              ├─ capability_handshake → 能力说明（一次）
                              └─ 其余 → 不重试，如实上报
```

缺的是「入口处怎么选」的半边：目前无条件走 Level 3。将来接新上游时的
正确顺序是：先用 `Profile` 记录观测事实（是否收到 tools、上游是否原生返回
结构化调用、response_format 是否被接受），再让 `SelectLevel()` 决定走哪级。

### 3. 后果

- 接支持结构化输出的上游时，有一个明确的、高价值的下一步（Level 2）。
- 在那之前，`SelectLevel()` 保持零调用方是**已知且可接受的**状态，
  不算孤儿代码——它是给那个未来预留的接口，且有完整测试钉住语义。
- 本 ADR 就是那个「TODO」的正式形态：比散落在代码里的注释可查。
