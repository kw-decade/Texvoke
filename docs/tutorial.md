# 教程：从零跑通一个能调用工具的纯文本模型

> **状态提醒（2026-08-25 入库时补）**：这份教程写于本项目还包含执行层的时期。
> 第二到五章与第八章讲的是「Runtime 亲自执行工具」——工具注册表、HTTP 与
> 子进程 executor、多租户认证、人工审批、执行结果截断。**那一整层已于
> 2026-08-23（`4303ee5`）移出本项目，代码不复存在**，照着它找不到对应实现。
>
> 当前形态只做协议桥接：把工具定义编译进 prompt，把模型文本解析回结构化
> 调用，交回客户端执行。仍然对得上的是第零、一、六、十章。
>
> 其余部分保留作设计参考——里面的取舍理由（为什么 argv 不拼 shell、
> 为什么审批必须过期、为什么 allowlist 要连重定向一起管）在将来重做执行层
> 时仍然成立。要按当前代码学怎么接入，看 README 的「三种接入方式」。

这份教程用一个具体的例子走完整条链路：**一个只会聊天的上游模型，
最后能在 Claude Code 里正常使用工具**。

前置知识只需要「知道 HTTP 和 JSON 是什么」。每一步都会说明它在做什么、
为什么必须这么做。

---

## 零、先搞清楚要解决的是什么

假设你有这么一个局面：

- 客户端（Claude Code、你自己的 Agent、任何 SDK）按 OpenAI 或 Anthropic
  协议发请求，请求里带着一堆工具定义，期待回复里有 `tool_calls`；
- 你的上游模型只会输出纯文本，压根不认识 `tools` 这个字段。

中间缺的那一层，就是这个项目。它做三件事：

1. **翻译**：把工具定义编译进 system prompt，教模型用一种特定格式「说出」
   它想调用什么；再把模型说出来的东西解析回结构化的 `tool_calls`。
2. **把关**：模型说想调用某个工具，不代表就该执行。注册表、Schema、
   策略、审批、幂等账本挨个过一遍。
3. **执行**：真的去调那个 HTTP 接口或跑那个子进程，把结果喂回给模型。

> **贯穿全文的一句话**：模型提出建议，Runtime 决定是否执行。
> 模型永远不是权限来源。后面每一个看起来啰嗦的设计，根源都在这句话。

---

## 一、最小可跑：什么工具都不配

先跑起来，再谈工具。

### 1.1 编译

```bash
cd Texvoke
go build -o bridge ./cmd/bridge
```

### 1.2 写一份最小配置

新建 `my-bridge.json`：

```json
{
  "listen": "127.0.0.1:8756",
  "default_max_tokens": 4096,
  "targets": [
    {
      "name": "my-upstream",
      "base_url": "https://your-upstream.example.com/v1",
      "protocol": "chat",
      "api_key_env": "MY_UPSTREAM_KEY"
    }
  ],
  "routes": [
    { "model": "*", "target": "my-upstream" }
  ]
}
```

逐字段解释：

| 字段 | 含义 | 为什么这么设计 |
|---|---|---|
| `listen` | 监听地址 | 默认只听环回。要暴露到局域网必须同时打开 `allow_remote` 与 `auth_tokens_env`，否则拒绝启动 |
| `protocol` | **上游**说的协议 | 与客户端说的协议无关。客户端可以用 Anthropic 协议进来，上游用 Chat 协议出去 |
| `api_key_env` | 密钥所在的**环境变量名** | 不是密钥本身。配置文件会被复制、贴进工单、提交进仓库，环境变量不会跟着走 |
| `routes` | 模型名 → 上游 | 没有任何路由命中时**报错**，不会退回到「第一个上游」。一个没配路由的模型悄悄走到某个上游去，是最难查的一类事故 |

### 1.3 启动

```bash
export MY_UPSTREAM_KEY=sk-your-key
./bridge -config my-bridge.json
```

启动日志里会有一条警告：

```
未配置认证，任何能连上监听地址的人都能调用工具
```

这是对的——你现在确实没配认证。只听环回时可以接受，暴露出去之前必须配。

### 1.4 试一下

```bash
curl http://127.0.0.1:8756/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"whatever","messages":[{"role":"user","content":"你好"}]}'
```

如果上游正常，你会拿到一个标准的 Chat Completions 响应。

**此刻发生了什么**：bridge 解码了请求 → 按 `routes` 选中 `my-upstream` →
把消息编成 Chat 协议发过去 → 解码响应 → 编回给你。工具还没参与。

---

## 二、加第一个工具：让模型能查东西

### 2.1 先配策略，再配工具

顺序不能反。**策略的零值是「什么都不允许」**——这是刻意的：忘记配置
应当导致失败，而不是放行。

在配置里加：

```json
  "network_policies": {
    "weather-api": {
      "name": "weather-api",
      "allowed_schemes": ["https"],
      "allowed_hosts": ["api.weather.example.com"],
      "max_redirects": 1,
      "max_response_bytes": 1048576,
      "timeout": 15000000000
    }
  }
```

几个容易踩的点：

- `timeout` 是**纳秒**（`15000000000` = 15 秒）。JSON 没有 duration 类型。
- `allowed_hosts` 为空表示**一个主机都不允许**，不是「不限制」。
- `max_redirects` 不能省：重定向是绕过 host allowlist 的经典手法——
  允许的域名回一个 302 指向内网地址。
- 没有 `allow_private_networks`，所以内网地址一律封锁。
  `169.254.169.254`（云 metadata 端点）一次请求就能吐出实例的临时凭证，
  它是 SSRF 的头号目标。

### 2.2 配工具

```json
  "tools": [
    {
      "namespace": "weather",
      "name": "current",
      "version": "1",
      "description": "查询某个城市的当前天气",
      "input_schema": {
        "type": "object",
        "additionalProperties": false,
        "required": ["city"],
        "properties": { "city": { "type": "string" } }
      },
      "risk": "read",
      "side_effect": "none",
      "idempotent": true,
      "enabled": true,
      "timeout_ms": 15000,
      "max_output_bytes": 65536,
      "http": {
        "method": "POST",
        "url": "https://api.weather.example.com/current",
        "policy_name": "weather-api",
        "header_env": { "Authorization": "WEATHER_API_TOKEN" }
      }
    }
  ]
```

几个必须理解的设计：

**`enabled` 默认是 `false`。** 注册不等于启用。你必须显式写 `true`。
这是「默认拒绝」在配置层的体现。

**`url` 在配置里，不在参数里。** 模型能影响的只有请求体，填不了 URL。
一旦允许模型指定 URL，任何一个「能发 HTTP 请求」的工具都变成了 SSRF
的入口——模型只要填个 `169.254.169.254` 就够了。

**`risk` 与 `side_effect` 决定要不要人工确认。** `read` + `none` 不需要；
`destructive` 或不可逆的副作用会被拦下来等审批（见第五节）。

**`idempotent` 决定超时后能不能重试。** 标 `false` 的工具一旦结果不明，
必须先对账才能再执行——直接重试就是在赌对方没收到。

### 2.3 三段式工具名

`namespace/name@version`，三段都必须有，且**精确匹配**：

- 不做模糊查找、不做大小写折叠、不做前缀补全——模糊匹配会让模型输出的
  近似工具名意外命中一个它无权调用的工具；
- 版本是必须的：同一个工具改了参数含义，就该是新版本。虚拟协议里模型
  只说 `namespace/name`（不带版本），版本由注册表补；同名有多个版本时
  **拒绝解析**而不是挑一个——挑哪个都是猜，猜错意味着用一份参数去调
  另一个版本的工具。

---

## 三、加一个子进程工具

HTTP 工具好理解。子进程工具是安全上最需要小心的，所以设计也最啰嗦。

```json
  "path_policies": {
    "workspace": {
      "name": "workspace",
      "roots": ["/srv/workspace"],
      "allow_write": false
    }
  },

  "command_policies": {
    "search": {
      "name": "search",
      "allowed_executables": ["/usr/bin/rg"],
      "allowed_env_keys": ["LANG"],
      "working_dir": "/srv/workspace",
      "max_args": 6
    }
  }
```

工具部分：

```json
    {
      "namespace": "code",
      "name": "search",
      "version": "1",
      "description": "在工作区里搜索文本",
      "input_schema": {
        "type": "object",
        "additionalProperties": false,
        "required": ["pattern", "root"],
        "properties": {
          "pattern": { "type": "string" },
          "root": { "type": "string" }
        }
      },
      "risk": "read",
      "side_effect": "none",
      "idempotent": true,
      "enabled": true,
      "timeout_ms": 20000,
      "max_output_bytes": 131072,
      "process": {
        "path": "/usr/bin/rg",
        "fixed_args": ["--json", "--max-count", "50"],
        "arg_fields": ["pattern", "root"],
        "policy_name": "search",
        "path_policy_name": "workspace",
        "path_fields": ["root"]
      }
    }
```

### 为什么这么啰嗦

**没有 shell。** 直接 exec `/usr/bin/rg`，不经过 `/bin/sh` 或 `cmd.exe`。
所以模型在参数里写 `a.txt; rm -rf /` 或 `` `id` ``，那些字符只是普通
字符——没有任何东西会去解释它们。

**`arg_fields` 而不是让模型给 argv。** 模型能填每个位置上的**值**，
填不了参数名。因为参数的**形状**本身就是攻击面：`--upload-file`、
`--exec`、`-o` 输出重定向，都是靠新增一个参数实现的。

**`path_fields` 必须显式声明。** Runtime 看到的只是一个字符串，
它分不出 `"a.txt"` 是路径还是搜索关键词。猜错的两个方向都很糟：
把关键词当路径会让正常调用失败，把路径当关键词则等于没有路径检查。
所以由你来说清楚哪个是路径。

**`allowed_env_keys` 默认为空。** 子进程拿到的是一个干净的环境。
继承整个环境是最常见的凭证泄露路径，而它通常发生在没人注意的地方——
你的 `ANTHROPIC_API_KEY` 会跟着传给每一个子进程。

**长内容走 `stdin_field`，不走命令行。** 命令行参数在整机范围内可见
（`ps`、`/proc/*/cmdline`、Windows 的进程列表），stdin 不会。

> **Windows 用户注意**：上面的路径按 Linux 写法给出。Windows 上必须用
> 带盘符的绝对路径，且 JSON 里的反斜杠要转义：
> `"C:\Tools\rg.exe"`、`"C:\workspace"`。
> 「绝对路径」的判定交给 `filepath.IsAbs`，它在 Windows 上不认 `/usr/bin/rg`
> 这种形式——依赖 PATH 查找意味着一个被改写的 PATH 就能让命令指向别的东西，
> 所以这里必须是明确的绝对路径。

---

## 四、配上认证

只要不是纯本地单用户，就必须配：

```json
  "auth_tokens_env": "MY_BRIDGE_TOKENS",
  "allow_remote": true,
  "listen": "0.0.0.0:8756"
```

```bash
export MY_BRIDGE_TOKENS='token-for-alice,token-for-bob'
```

客户端带 `Authorization: Bearer token-for-alice` 即可。

两条硬规则：

1. **`allow_remote` 与 `auth_tokens_env` 必须同时出现**，否则拒绝启动。
   对外暴露又不认证，比两个危险选项单独出现都严重。
2. **配了变量名却取不到值也拒绝启动**。这几乎总是部署时漏了设环境变量，
   而静默降级成「不认证」是最危险的失败方式。

---

## 五、危险工具与人工确认

给工具标上 `"risk": "destructive"` 或不可逆的 `side_effect` 之后，
它就需要人工确认才能执行。

**当前的实际状态**：审批账本已经实现（有效期、可用次数、常量时间比较
token、撤销、审计），但**还没有发放入口**——没有确认界面。所以这类工具
在 `bridge` 里会一直停在「等待人工确认」，实际不可用。

这是正确的默认：宁可用不了，也不要在没人看着的时候把它们放行。

如果你现在就需要跑高风险工具，只能把 `risk` 降级——但那意味着你自己
承担了后果，而不是系统帮你挡住了。

---

## 六、模型不肯调用工具时怎么排查

这是最常见的问题，而且**根因往往不在模型**。bridge 会在日志里给出诊断：

```json
{"level":"INFO","msg":"upstream","tools_declared":8,"tools_sent":0,...}
{"level":"WARN","msg":"上游链路异常","detail":"工具数在发出前被改动：查中间代理与 CCS 重写规则"}
```

按这个顺序排查：

| 现象 | 根因 | 该去查什么 |
|---|---|---|
| `tools_declared: 0` | 客户端压根没发工具 | SDK 用法、模型目录、`/v1` 路径。**不是 Prompt 问题** |
| `tools_declared` ≠ `tools_sent` | 中间层删改了请求 | 代理、CCS 重写规则、认证头 |
| `http_status: 0` | 连不上 | 网络、DNS、上游地址。**不要去调 Prompt** |
| 上游返回 403 + 明确的错误码 | 供应商政策禁止 | 换上游或换模型。**不要试图绕过** |
| 模型说「我只是一个网页助手」 | 事实性误会 | bridge 会自动做**一次**诚实的能力说明 |
| 解析不出调用 | 格式不稳 | bridge 会做**一次**格式修复 |

注意那两个「一次」。循环施压与「诚实地说明一次运行时环境」的区别，
恰恰就在于次数。第二次会直接停手。

**关于 403 那一行**：判定为供应商政策拒绝后，bridge 会立刻停止一切
Prompt 施压和格式变通，返回 `upstream_capability_denied`。这不是能力
不足，是刻意的——继续换措辞、改工具名去试，就是在绕过供应商的安全策略。

---

## 七、看看它到底在做什么

### 日志里有什么

- `upstream`：每次上游往返的字段摘要（协议、模型、工具数、首条消息类型、
  状态码、耗时）。**没有任何需要脱敏的字段**——因为需要脱敏的东西压根
  没被收集。「先收集再脱敏」总有漏网的一天。
- `call_settled` / `call_denied`：每次工具调用的结局。参数只有**摘要**，
  没有原文；错误只有**错误码**，没有正文。

摘要足以回答审计要问的问题——「模型当时提出的是不是这一份参数」——
而原文除了泄露之外不提供额外的证明力。

### 健康检查

```bash
curl http://127.0.0.1:8756/healthz
```

只报活着、工具数量、artifact 占用。不报地址、不报密钥变量名、
不报工具定义——健康检查端点常常是唯一不需要认证就能访问的东西。

---

## 八、结果太大怎么办

工具返回 50 MB 的 JSON 时，`max_output_bytes` 会把回给模型的部分截断，
**完整内容存进 artifact**，句柄告诉模型。

两个上限是两件事，不要混：

- `network_policies.*.max_response_bytes`：最多**从网络读**多少；
- `tools.*.max_output_bytes`：最多**回填给模型**多少。

取两者的小值会让超出模型预算的部分连读都不读，artifact 里也只剩半截。

artifact 目前是纯内存的，**进程重启就没了**。这是刻意的取舍：落盘会
引入路径、权限、清理与磁盘配额四组新问题。

---

## 九、还差什么

按诚实边界，下面这些**是没做，不是做了一半**：

- **JSON Schema 实例校验**——参数只验「是合法 JSON 对象」，类型、必填、
  枚举、范围都没验。要引入第一个第三方依赖，尚未决定。
- **多租户隔离**——字段存在，没有任何地方据它隔离。
- **审批发放入口**——见第五节。
- **真流式**——现在是伪流式：事件形状正确，但要等 Agent loop 跑完才开始发。
- **沙箱**——子进程只有环境与 argv 约束，没有 namespace / seccomp。
- **全局并发上限**——单请求有预算，总量没有。生产部署需要在前面放限流。

完整清单见 [threat-model.md](threat-model.md) 第四节。

---

## 十、如果你只想要「翻译」，不要「执行」

有两条更轻的路：

- **HTTP sidecar**（`cmd/utr-server`）：两个 HTTP 调用，任何语言都能接。
  它只做协议翻译，不执行任何工具。
- **Go 库**（`pkg/toolbridge`）：同一套核心，少一跳网络。

见 [README](../README.md) 的「三种接入方式」。
