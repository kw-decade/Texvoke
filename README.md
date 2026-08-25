# Texvoke

**text + invoke** —— 让只会输出纯文本的大模型，学会发起结构化的工具调用。

不是又一个反代——它是你的反代可以接进去的一层能力。你写多少个反代，
就接多少次，每次二三十行胶水代码。

```
你的反代（任何语言）
     │
     ├── 请求进来：工具定义 ──► Texvoke ──► system prompt + 会话暗号
     │
     ├── 转发给你的纯文本上游（这一段完全是你自己的逻辑）
     │
     └── 回复出来：模型文本 ──► Texvoke ──► 结构化 tool_calls
```

---

## 它解决什么

你有一个只会聊天的上游模型，但客户端（Claude Code、Codex、你自己的 Agent）
按 OpenAI / Anthropic 协议发请求，期待拿到 `tool_calls`。

中间这层翻译工作看起来简单，实际有一堆坑：

- 触发信号被切在 chunk 边界上，漏判或误判
- 模型以为自己是网页助手，回答「我无法访问你的文件系统」
- 客户端的 hook 注入几十 KB 行为规则，协议说明被淹没
- 八十个工具挤进上下文，模型挑花眼
- 参数 JSON 被切碎，拼回来时少一截
- 多轮对话里 call_id 相互撞车
- 工具结果里藏着「忽略之前的指示」

这些问题 Texvoke 都有对应的机制，而且每一类都有真实客户端的抓包回归样本盯着
（见 [评测标尺](#评测标尺)）。

## 实战效果

用真实客户端抓包做标尺（`utr-eval`，每份跑 6 次）：

| 场景 | 改造前 | 现在 |
|---|---|---|
| Claude Code（hook 注入 31KB 行为规则 + 80 个工具） | 反复声称「没有工具」 | **100% 发起调用** |
| Codex 卡死会话（历史污染最重形态） | ~50% | **100%，且耗时减半** |

测量条件要看清：这两个数字是 2026-08-25 经 HTTP sidecar 那条链路
（Node 反代 + `utr-server`）打一个具体上游得到的，样本是 6 次。它们说明
**协议可靠性**——模型是否发出了格式合法、ID 关联正确的调用——不说明模型
选对了工具或填对了参数，也不构成对其它上游、其它模型的承诺。换上游、
换客户端、换模型都需要重新用 `utr-eval` 立一次尺子。

覆盖的客户端实测：**OpenAI Codex CLI**（Responses 协议 + 自定义裸文本工具）、
**Claude Code**（Anthropic 协议经 CC Switch）。

---

## 三种接入方式

| 方式 | 适合 | 接入成本 |
|---|---|---|
| **HTTP sidecar** | 已有反代，想补上工具能力 | 几个 HTTP 调用 |
| **Go 库** | Go 写的反代 | `import`，少一跳网络 |
| **一体化网关** `texvoke serve` | 不想写任何反代 | 一条命令 |

三种共用同一套核心，语义完全一致。

## 快速上手：HTTP sidecar

### 1. 启动

```bash
go build -o utr-server ./cmd/utr-server
./utr-server -addr 127.0.0.1:8757
```

默认只监听环回。要暴露到局域网必须显式加 `-allow-remote`——这个服务没有
认证机制，绑到 `0.0.0.0` 等于把工具调用能力开放给整个网络。

### 2. 两个接入点

**请求进来时**，把工具定义换成 system prompt：

```bash
POST /v1/compile
{
  "session_id": "sess-1",
  "request_id": "req-1",
  "tools": [{
    "name": "get_weather",
    "description": "查询天气",
    "input_schema": {"type":"object","properties":{"city":{"type":"string"}}}
  }],
  "query": "旧金山天气怎么样"
}
```

```json
{
  "system_prompt": "## 可用工具\n...",
  "nonce": "4bbdb7b6902aca7081d3057b957d4bca",
  "signal": "[[UTR-CALL:4bbdb7b6...]]",
  "tools_included": ["get_weather"],
  "tools_dropped": 0
}
```

把 `system_prompt` 拼进 system 消息，**把 `nonce` 存下来**。

**模型回复后**，把文本换回工具调用：

```bash
POST /v1/parse
{
  "session_id": "sess-1",
  "request_id": "req-1",
  "nonce": "4bbdb7b6902aca7081d3057b957d4bca",
  "text": "<模型的完整输出>"
}
```

```json
{
  "text": "我来查一下。\n",
  "calls": [{"id":"c1","name":"get_weather","arguments":{"city":"SF"}}],
  "outcome": "calls_parsed"
}
```

`text` 是可以原样转发给客户端的正文——协议标记一个字都不会在里面。

### 3. 流式

上游是流式的话用 `/v1/parse/stream`：请求体是模型输出的字节流，
响应是 NDJSON 事件流。

```
POST /v1/parse/stream?session_id=s&request_id=r&nonce=<nonce>
（请求体：模型输出流）

{"type":"text","text":"我来查一下。\n"}      ← 可以立即转发给客户端
{"type":"done","outcome":"calls_parsed","calls":[...]}
```

用 chunked HTTP 而非 WebSocket，任何语言的 HTTP 客户端都能收发。

### 完整示例

- [`examples/fastapi_proxy.py`](examples/fastapi_proxy.py)：Python / FastAPI，非流式
- [`examples/node_proxy.mjs`](examples/node_proxy.mjs)：Node，流式
- [`examples/go_inprocess.go`](examples/go_inprocess.go)：Go 库直连，两种都有

> 接真实的 Codex / Claude Code 还需要三协议渲染与恢复循环，见下文
> 「高级端点」与路线图。

## 高级能力

### 三协议直通（adapt / render）

反代要同时伺候 Codex（Responses）、Claude Code（Anthropic）、普通 OpenAI
客户端时，用 `/v1/adapt` 与 `/v1/render`：把客户端原始请求体整个递过来，
sidecar 负责解码、工具编译、协议翻译；回复时再把解析结果编回客户端来时的
协议。Codex 的 `custom` 裸文本工具、`additional_tools` item、Claude Code 把
system 塞进 messages 数组等现实怪癖都已处理。

### 突破阶梯（五级救援）

纯文本模型最常见的失败不是格式错，而是**不肯调用**：「我无法访问文件系统」
「请你自己运行」「我会先读取再创建」却迟迟不发信号。Texvoke 的处置随尝试
次数逐级加强：

| 级 | 手段 |
|---|---|
| L1 | 能力说明：「你不需要亲自执行，只需提出结构化调用建议」 |
| L2 | 运行时通知：「[runtime] 未检测到调用信号，流水线阻塞」，并引用本会话已有的成功调用反驳「接口不可用」的错觉 |
| L3 | 完整示范：附上可直接照抄的信号行 + envelope（参数为占位符）|
| L4 | 明示行动：直接给出该输出的内容模板 |

理论依据是 ISC/TVD 研究（[Internal-Safety-Collapse](https://github.com/wuyoscar/Internal-Safety-Collapse)）：
模型对「请求」会拒绝，对「环境报错」会本能地修。每一级都保留出口句
（「判断任务不应执行就直接说明理由」）——这是诚实的信息陈述，不是无限施压。

配套的识别网覆盖：显式拒绝措辞、否定×能力共现、行动承诺型、踢皮球型、
错格式意图型。共现判据支持中英日韩法德西俄八种语言——其中中文与英文的
形态来自真实抓包，另外六种只有单测里的合成样本，没有真实上游回复作为依据。

### 工具清单治理

agent 客户端动辄声明几十个工具（含大量长描述插件工具），全塞进 prompt 会把
协议指令稀释到模型不再遵守。治理手段：按查询相关度筛选 + 数量上限 +
`always_include` 名单保证核心文件/终端工具永远可见。这些由接入方按自己的
客户端调参，框架不内置任何厂商名单。

---

## 错误怎么处理

解析失败**不会**返回 HTTP 错误——那是一次成功的分析，结论在 `outcome` 里。
只有服务本身出问题才是非 200。

| `outcome` | 含义 | 该做什么 |
|---|---|---|
| `plain_text` | 模型没调用工具，正常回答 | 原样返回 |
| `calls_parsed` | 解析出调用 | 执行它们 |
| `malformed` | 模型格式写错了 | 走恢复循环，把具体错误反馈给模型 |
| `truncated` | 结构在闭合前断掉 | 通常是上游断流或 token 上限，查链路而不是调 Prompt |

`malformed` 与 `truncated` 分开是有用的：前者该调 Prompt，后者该查网络。
混为一谈会指错排查方向。

---

## 诚实边界

**能解决的**：协议缺失（上游没有 function calling）、输出格式不稳、
工具过多导致的选择困难、模型因自我认知而拒绝调用。

**不能解决的**：

- **上游明确禁止工具调用**。供应商用错误码或政策声明禁止时，本项目不做任何
  变通——对这种情况伪装成功是设计缺陷，不是功能。
- **被中间层捂住嘴的上游**。如果上游网站会用代码过滤或改写模型的原始输出，
  调用信号在到达本框架之前就没了。判据一句话：模型的原始文字能不能完整流到
  你手里。
- **模型选错工具或填错参数**。协议层保证「格式合法、ID 关联正确」，
  保证不了「模型理解对了任务」。这两件事要分开报告，不能用前者的通过率
  冒充后者的。

**流式是伪流式**：事件形状正确，但要等整段文本到齐才开始发。这与闭环恢复
互斥——边收边发的字节收不回来，就没法追问重来。

---

## 评测标尺

改措辞、调结构之前先立尺子——Prompt 工程的直觉命中率约等于抛硬币，
三次「看起来更好」的改写曾让成功率暴跌（100%→38%）。仓库自带两份真实
客户端抓包的脱敏 fixture：

```bash
go run ./cmd/utr-eval -n 6          # 打本地反代，输出按根因分类的分布
go run ./cmd/utr-redact -in <抓包> -out tests/fixtures/eval/<名字>.json   # 添加你自己的场景
```

任何影响 Prompt 或分类器的改动，提交前必须附前后对比。

## 验证

这六条命令由 CI 在每次 push / PR 时自动执行（`.github/workflows/verify.yml`），
本地改动后也建议照跑一遍：

```bash
gofmt -l ./cmd ./internal ./pkg ./examples
go build ./...
go vet ./...
go test ./...
go test -race ./...
```

fuzz 与基准：

```bash
go test -run=Fuzz -fuzz=FuzzParse -fuzztime=30s ./internal/parser/
go test -run=Fuzz -fuzz=FuzzSSEDecode -fuzztime=30s ./internal/protocol/
```

一次只能 fuzz 一个目标，这是 Go 的限制。`testdata/fuzz/` 下的失败输入是
回归语料，必须入库——普通 `go test` 会自动重跑它们。

## 路线图

三步走已全部完成（2026-08-25）：

| 步 | 内容 | 状态 |
|---|---|---|
| 1 | 会话状态下沉 sidecar（阶梯进度/配额随 session_key 自动管理） | ✅ |
| 2 | 上游适配器接口 internal/upstream（`openai-chat` 内置零适配） | ✅ |
| 3 | `texvoke serve` 一体化网关 + internal/gateway 编排器单测 | ✅ |

现在就能用：

```bash
go build -o texvoke ./cmd/texvoke
texvoke --listen 127.0.0.1:8756   --upstream https://your-upstream.example.com/v1/chat/completions   --format openai-chat   --max-tools 10   --always-include "Bash,Read,Write,Edit,Glob,Grep"
```

`--format` 当前支持 `openai-chat`（标准 Chat Completions 上游）。
仓库里还有一个为某类「网页前端自带接口」写的适配器示例（自定义 payload、
SSE 方言、浏览器头校验），作为写你自己的适配器时的参考——它不随仓库分发。

之后 Codex / Claude Code / 任何 OpenAI 兼容客户端直连即可用工具。
workflow 网站的 AI 反代成 chatcompletion 端点 → 一条命令 → 直连。

默认只监听环回。要暴露到局域网必须显式加 `-allow-remote`——**这个服务没有
认证机制，而且持有上游凭据**，绑到 `0.0.0.0` 等于把你的 API Key 变成公共代理。

一件该说清的事：三步走的代码与单元测试都完成了，但 `texvoke serve` 到
2026-08-25 为止只在本地假上游上端到端验过；上面「实战效果」那两个数字是经
HTTP sidecar 链路测的。真实上游直连的实测是下一步。

后续方向：真流式（当前伪流式与闭环恢复互斥）、更多上游 format、
能力否定型拒绝的专项 fixture。

## 文档

- [`docs/adr/`](docs/adr/)：架构决策记录（许可证与参考项目、技术栈与包边界、
  公开 API 边界、调用阶梯现状）
- [`docs/threat-model.md`](docs/threat-model.md)：信任边界与攻击面
- [`docs/tutorial.md`](docs/tutorial.md)：中文白话教程

教程与威胁模型写于本项目还包含执行层的时期，两份文件顶部都有状态提醒说明
哪些章节仍然对得上代码——**别照着它们去找执行层的实现**，那一层 2026-08-23
就移出去了。

## 许可证

GPL-3.0。
