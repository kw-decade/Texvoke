# ADR-0001：许可证选择与参考项目使用策略

- 日期：2026-08-22
- 状态：已接受

## 背景

Texvoke 的规格（`universal-tool-runtime-agent-prompt-final.md` 第二章）要求
参考三个开源项目，并明确「确认许可证和边界」，但没有给出结论。实际情况：

| 项目 | 许可证 | 规模 | 语言 |
|---|---|---|---|
| `HKUDS/AnyTool` | MIT | 27840 行 | Python |
| `funnycups/Toolify` | GPL-3.0 | 3009 行 | Python |
| `42xx42/newapi-tool-bridge` | GPL-3.0 | 2768 行 | Python |

其中两个是 GPL-3.0 强 copyleft。这直接决定了「能不能读它们的源码」这一条
操作纪律：如果新项目采用宽松许可证，逐行阅读 GPL 源码后写出的实现存在
构成衍生作品的风险，只能停留在读 README 和公开架构描述的层面，实现细节
必须从协议规范重新推导。

## 决策

**Texvoke 采用 GPL-3.0**，与两个 GPL 参考项目保持同许可证。

因此三个参考仓库的源码均可自由阅读和借鉴：GPL-3.0 之间同许可证兼容；
AnyTool 的 MIT 单向兼容 GPL-3.0，可并入本项目，但借鉴其代码时需在文件头
注明来源与原许可证。

三个参考仓库在本工作区内视为**只读**，不做任何修改，并通过 `.gitignore` 排除在
本仓库的版本控制之外（它们各自是独立上游仓库，丢失后重新 clone 即可）。
为保证结论可复现，记录本次分析所依据的 commit：

| 项目 | 仓库地址 | 分析时的 commit | 日期 |
|---|---|---|---|
| AnyTool | `https://github.com/HKUDS/AnyTool.git` | `506430fec13300853b2010e2604ae0c71b940502` | 2026-02-28 |
| newapi-tool-bridge | `https://github.com/42xx42/newapi-tool-bridge.git` | `e40dfdd5094a77452d867447f1b4adceeda2fd81` | 2026-05-30 |
| Toolify | `https://github.com/funnycups/Toolify.git` | `b934c9c218f5f400aecca61e26f2b83783c47812` | 2026-04-14 |


## 后果

正面：

- 消除了阅读参考实现的法律障碍，Toolify 的跨 chunk 流式检测、
  newapi-tool-bridge 的协议归一化、AnyTool 的工具检索这些已验证的做法
  可以直接研究实现细节，不必重新踩坑；
- 与生态中同类项目许可证一致，便于回馈上游。

负面：

- 任何基于本项目的分发都必须开源，闭源商用路径被排除；
- 若将来需要改成宽松许可证，已借鉴的 GPL 代码部分需要清理重写，成本很高。
  **这个决定实际上是不可逆的，改变前必须重新评估。**

## 备选方案

- **Apache-2.0 / MIT + 严格隔离 GPL 源码**：保留商用灵活性，代价是实现细节
  只能自己推导，开发变慢，且「读到什么程度算污染」的边界在实践中难以把握。
  已评估并放弃。
