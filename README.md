# Texvoke

**text + invoke** —— 让只会输出纯文本的大模型，发起结构化的工具调用。

```
你的客户端（Claude Code / Codex / 任何 OpenAI 兼容 Agent）
     │
     │  按标准协议发请求：OpenAI Chat / OpenAI Responses / Anthropic Messages
     ▼
┌───────────── Texvoke ─────────────┐
│  ① 把工具定义编译进 system prompt   │
│  ② 转发给你的纯文本上游             │
│  ③ 把模型回复解析回结构化 tool_calls │
│  ④ 模型不肯调用时，按阶梯逐级救援    │
└───────────────────────────────────┘
     │
     ▼
  你的上游（任何只能聊天的模型 API）
```

## 它解决什么问题

你有一个只会聊天的上游模型——可能是自建服务、某个网页前端的接口、或者任何
没实现 function calling 的 API——但客户端（Claude Code、Codex、你自己的
Agent）按标准协议发请求，期待拿到 `tool_calls`。

中间这层翻译看起来简单，实际有一堆坑：

- 触发信号被切在 chunk 边界上，漏判或误判
- 模型以为自己是网页助手，回答「我无法访问你的文件系统」而不调用
- 客户端的 hook 注入几十 KB 行为规则，协议说明被淹没在上下文里
- 八十个工具挤进 prompt，模型挑花眼
- 参数 JSON 被 SSE 切碎，拼回来时少一截
- 多轮对话里 call_id 相互撞车
- 工具结果里藏着「忽略之前的指示」

Texvoke 对每一类都有对应机制，且每类都有真实客户端抓包的回归样本盯着。

## 快速开始

```bash
git clone https://github.com/kw-decade/Texvoke.git
cd Texvoke
go build -o texvoke ./cmd/texvoke

texvoke serve \
  --listen 127.0.0.1:8756 \
  --upstream https://your-upstream.example.com/v1/chat/completions \
  --format openai-chat \
  --max-tools 10 \
  --always-include "Bash,Read,Write,Edit,Glob,Grep"
```

然后把任何 OpenAI 兼容客户端指到 `http://127.0.0.1:8756/v1`。三个端点同时可用：

| 端点 | 协议 |
|---|---|
| `POST /v1/chat/completions` | OpenAI Chat Completions |
| `POST /v1/responses` | OpenAI Responses |
| `POST /v1/messages` | Anthropic Messages |
| `GET /v1/models` | 模型清单（透传上游）|

客户端发什么协议，就按什么协议回应；工具调用以客户端原本期待的形状返回，
客户端感知不到中间有一层。默认只监听环回——本服务没有认证机制，暴露到
网络必须显式加 `-allow-remote` 并自行做好访问控制。

### 上游适配

`--format openai-chat` 覆盖标准 Chat Completions 上游，零配置。
其它形状的上游实现一个接口即可接入：

```go
type Adapter interface {
    Name() string
    Complete(ctx context.Context, model string, messages []protocol.Message) (Reply, error)
    Models(ctx context.Context) ([]string, error)
}
```

SSE 解析、payload 形状、浏览器头伪装全部收进实现内部，编排层只面对接口。
`internal/upstream/openai.go` 是现成的参考实现（含代理出口支持：
`HTTPS_PROXY` 环境变量对按 IP 做地区风控的上游是刚需）。

### 作为 Go 库使用

不想要网关进程的话，核心能力可以直接 import：

```go
b, _ := toolbridge.New(toolbridge.Config{})
sess, _ := b.NewSession("sess-1", "req-1")

// 请求进来时：把工具定义编译进 prompt
compiled, _ := sess.Compile(tools, toolbridge.CompileOptions{})

// 模型回复后：解析出结构化调用
res, _ := sess.Parse(modelOutput)
// res.Calls → []Call{Name, Arguments, ...}，交回客户端执行
```

完整示例见 [`examples/`](examples/)（Go / Node / Python 三种语言）。

## 核心机制

### 会话级随机信号 + XML envelope

每个会话生成一次性的随机触发信号（如 `[[UTR-CALL:4bbdb7b6...]]`）。固定串
（如 `<function_calls>`）大量存在于训练语料和客户端系统提示里，极易误触发；
随机 nonce 让「输出了这个串」成为「读过本次协议说明」的可信证据。

同一 nonce 一值三用：

1. 解析器认的协议信号；
2. 历史回灌时渲染 envelope 的同一标记——模型看到的过去与当下规范逐字一致；
3. call_id 的摘要种子——无状态服务产出跨轮唯一、重渲染幂等的 ID，
   解决 Codex 每轮重发历史导致的 call_id 撞车。

### 突破阶梯（四级救援）

纯文本模型最常见的失败不是格式错，而是**不肯调用**：「我无法访问文件系统」
「请你自己运行」「我会先读取再创建」却迟迟不发信号。处置随尝试次数逐级加强：

| 级 | 手段 |
|---|---|
| L1 | 能力说明：诚实解释「你不需要亲自执行，只需提出建议」 |
| L2 | 运行时通知：`[runtime] 未检测到调用信号，流水线阻塞`，并引用本会话已有的成功调用反驳「接口不可用」的错觉 |
| L3 | 完整示范：附上可直接照抄的信号行 + envelope |
| L4 | 明示行动：直接给出该输出的内容模板 |

理论依据是 ISC/TVD 研究（[Internal-Safety-Collapse](https://github.com/wuyoscar/Internal-Safety-Collapse)）：
模型对「请求」会拒绝，对「环境报错」会本能地修——所以 L2 起用状态陈述体
而不是劝说体。每一级都保留出口句（「判断任务不应执行就直接说明理由」），
这是诚实的信息陈述，不是无限施压；非施压措辞有测试钉住。

配套的拒绝识别网覆盖七类根因：显式拒绝措辞、否定×能力共现、行动承诺型、
踢皮球型、错格式意图型等，判定顺序从硬证据（传输层错误、HTTP 状态码）到
软证据（文本关键词），关键词命中只值低置信度。共现判据支持中英日韩法德西俄
八种语言——其中中英文形态来自真实抓包，其余六种只有合成测试样本。

### Prompt 工程，但用回归测量的纪律

Prompt 措辞的直觉命中率约等于抛硬币——本项目实测记录里有三次「看起来更好」
的改写让成功率从 100% 跌到 38%。因此：

- 仓库自带真实客户端抓包脱敏后的评测 fixture（`tests/fixtures/eval/`），
  `cmd/utr-eval` 按**根因分类**输出成功率分布，改措辞前后必须跑对比；
- 协议示例只能由同一个渲染函数产出——「教给模型的格式」与「解析器认的
  格式」物理上不可能分叉；
- 工具过多时的治理手段（相关度筛选 + 数量上限 + `always_include` 名单）
  全部由接入方调参，框架不内置任何厂商词表。

### 安全边界

不可信数据永不升级为指令：工具结果、历史文本拼进 prompt 时被显式的不可信
边界标记夹起来并声明其中的指令无效。解析失败必须是显式错误，绝不生成带
兜底参数的可执行调用。本项目只把调用提案交回客户端——执行发生在客户端的
权限边界内，模型永远不是权限来源。

## 与同类项目的差异

| | Texvoke | 常见反代方案 |
|---|---|---|
| 部署形态 | 单二进制网关 / Go 库，零依赖 | Node 服务 + 依赖树 |
| 不肯调用时的处置 | 四级递进救援，措辞经实验校准 | 重发 prompt 或放弃 |
| Prompt 措辞变更 | 强制评测对比，有回归 fixture | 无测量手段 |
| call_id 跨轮一致性 | 确定性摘要，无状态可复算 | 注册表或计数器 |
| 协议示例与解析器 | 同源渲染，不可能分叉 | 两处手写 |

## 实测效果

用真实客户端抓包做标尺（`utr-eval`，每份跑 6 次，2026-08-25 测量）：

| 场景 | 改造前 | 现在 |
|---|---|---|
| Claude Code（hook 注入 31KB 规则 + 80 个工具） | 反复声称「没有工具」 | **100% 发起调用** |
| Codex 卡死会话（历史污染最重形态） | ~50% | **100%，耗时减半** |

这两个数字说明**协议可靠性**（模型是否发出了格式合法、ID 关联正确的调用），
不说明模型选对了工具或填对了参数，也不构成对其它上游的承诺。换上游、换
客户端都需要重新立尺子。

## 诚实边界

**能解决的**：上游没有 function calling、输出格式不稳、工具过多导致的选择
困难、模型因自我认知而拒绝调用。

**不能解决的**：

- **上游明确禁止工具调用**。供应商用错误码或政策声明禁止时，不做任何变通
  ——对这种情况伪装成功是设计缺陷，不是功能。
- **被中间层捂住嘴的上游**。如果上游网站会用代码过滤或改写模型的原始输出，
  调用信号在到达本框架之前就没了。判据一句话：模型的原始文字能不能完整流到你手里。
- **模型选错工具或填错参数**。协议层保证格式合法，保证不了语义正确。
- **真流式**。事件序列形状完全正确，但要等整段文本到齐才开始发——这与
  闭环恢复互斥（边收边发的字节收不回来），是有意的取舍。

## 项目结构

```
cmd/texvoke/          一体化网关（推荐入口）
pkg/toolbridge/       Go 库门面
internal/gateway/     编排：解码→编译→上游→解析→救援循环→渲染
internal/upstream/    上游适配器接口与 openai-chat 实现
internal/serving/     会话阶梯状态与监听守卫
internal/protocol/    三协议编解码 + SSE（Golden fixture 回归）
internal/parser/      增量状态机解析（fuzz 覆盖超两千万次执行）
internal/capability/  拒绝分类、突破阶梯文案
internal/prompt/      Prompt 编译、候选筛选、不可信边界
internal/vproto/      虚拟协议格式定义（生成侧与解析侧的唯一契约源）
docs/adr/             架构决策记录
docs/tutorial.md      中文白话教程
docs/threat-model.md  信任边界与攻击面
```

依赖方向严格单向：`ir` 与 `vproto` 是两个叶子（只依赖标准库），其余包只能
向下依赖。全项目零第三方依赖——`encoding/json`、`net/http`、`log/slog`
加标准库解决一切，这也是 fuzz 与 race 测试能在任意环境直接跑的前提。

## 验证

六条命令由 CI 在每次 push / PR 自动执行（`.github/workflows/verify.yml`），
本地改动后也建议照跑：

```bash
gofmt -l ./cmd ./internal ./pkg ./examples   # 输出为空才算通过
go build ./...
go vet ./...
go test ./...
go test -race ./...
```

fuzz 目标四个，累计执行超过两千万次；`testdata/fuzz/` 下的失败输入是回归
语料，普通 `go test` 会自动重跑它们。

## 致谢

本项目在设计过程中研究并借鉴了以下开源项目与研究的思路，感谢原作者：

| 项目 | 许可证 | 借鉴之处 |
|---|---|---|
| [AnyTool](https://github.com/HKUDS/AnyTool) (HKUDS) | MIT | 大规模工具集的自检索架构；「把工具描述当数据而非代码」的边界意识 |
| [Toolify](https://github.com/funnycups/Toolify) (funnycups) | GPL-3.0 | 跨 chunk 流式信号检测的工程细节 |
| [newapi-tool-bridge](https://github.com/42xx42/newapi-tool-bridge) (42xx42) | GPL-3.0 | 三协议归一化的取舍经验 |
| [Internal-Safety-Collapse](https://github.com/wuyoscar/Internal-Safety-Collapse) | — | 「模型对请求拒绝、对环境报错本能修复」的实证结论，突破阶梯的理论依据 |

Texvoke 从它们的踩坑记录里吸收了教训，也刻意避开了它们的缺点（过大依赖、
隐式安装、把 LLM 评分当安全决策、只用正则解析嵌套格式）。许可证沿用
GPL-3.0，与两个同许可证参考项目保持生态一致。

## 许可证

GPL-3.0。基于本项目的分发必须同样开源。
