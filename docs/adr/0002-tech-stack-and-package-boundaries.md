# ADR-0002：技术栈、包边界与依赖方向

- 日期：2026-08-22
- 状态：已接受

## 背景

规格第四章建议用 Go + 标准库实现，并给出 13 个 `internal/` 包的目录边界，
但没有定义包之间的依赖方向。三个参考项目都是 Python 单体或大型多包结构
（Toolify 的核心逻辑集中在 2750 行的 `main.py`，newapi-tool-bridge 集中在
2220 行的 `tool_bridge.py`），规格明确要求「不要一次写一个几千行的单文件」。

没有明文的依赖方向，包边界会在实现过程中被逐步侵蚀，最终退化成互相 import
的大泥球——这正是参考项目单文件形态的根因。

## 决策

1. **Go 1.26+，标准库优先**：`net/http`、`encoding/json`（含 `json.RawMessage`）、
   `bufio`、`context`、`crypto`、`log/slog`、`testing`。
2. **两个例外**：JSON Schema 校验使用成熟第三方库（规格明确禁止自写校验器）；
   Tokenizer 做成可插拔接口，不把具体实现塞进核心。
3. **不使用 Go plugin** 作为插件机制，改用显式接口 + 隔离进程，便于跨平台、
   升级和审计。
4. **依赖方向单向，禁止循环**，以 `internal/ir` 为唯一根节点：
   `ir` 不 import 任何其他 internal 包；`observability`、`schema`、`capability`、
   `policy`、`session`、`parser`、`protocol` 只依赖 `ir`；`prompt` 依赖
   `ir + capability`；`executor` 依赖 `ir + policy`；`router` 依赖
   `ir + protocol + capability`；`agent` 位于顶层，可依赖其余全部。
   完整依赖树见仓库根目录的 `CLAUDE.md` 第四节（该文件不入库，仅本地参考；
   简版：`ir` 与 `vproto` 是两个叶子，其余包只能向下依赖）。
5. **每个包一个 `doc.go`**，写明「负责什么、不负责什么、依赖谁」，
   作为包边界的可执行约定——新增文件前先读它。
6. **Module path 暂用本地路径 `texvoke`**，发布前改为实际仓库路径。

## 后果

正面：

- `ir` 作为根意味着所有协议差异必须先归一化再流转，从结构上防止
  「Anthropic 的 `tool_use_id` 泄漏进 Chat Completions 渲染路径」这类问题；
- `executor` 只能经由 `policy` 到达，安全门无法被绕过——这是编译期而非
  约定层面的保障；
- `doc.go` 让包职责在代码里可读，不必翻文档。

负面：

- 严格分层会让某些跨层的便捷写法变得繁琐，需要显式传参或定义接口；
- 13 个包对当前 0 行业务代码而言偏多，早期会有一批只有 `doc.go` 的空包。
  这是刻意的：先立边界再填内容，比先写单文件再拆分代价低。

## 备选方案

- **先写单体再拆包**：迭代快，但参考项目已经证明了终局是几千行单文件，
  且拆分时机通常永远不会到来。放弃。
- **按协议分包（openai/、anthropic/）而非按职责分包**：会导致解析、校验、
  策略逻辑在每个协议下重复实现，与「Canonical IR 优先」的核心设计冲突。放弃。
