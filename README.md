# auto-harness

以 [pi](https://github.com/earendil-works/pi)（badlogic 的 "minimal, extensible agent harness"）为设计蓝本，用 Go 实现的基础 agent harness 框架。

**设计哲学（继承 pi）**：极简内核 + 一切皆扩展。核心只做一件事——把 LLM 补全、工具执行和事件通知跑成一个循环；其余所有能力（UI、统计、压缩、权限……）都通过扩展挂在事件总线上。

## 架构总览

```
cmd/harness/          CLI 入口（REPL + one-shot + RPC），UI 只是事件总线的一个消费者
tui/                  差分渲染终端 UI（对齐 pi-tui）                    ← pi-tui
  diff.go               帧差分：只重写变化的行
  components.go         Component 体系（Text/Container/VStack/HStack）+ 样式 + 宽度计算
  screen.go             屏幕抽象：主屏/备用屏、同步输出、渲染循环
  transcript.go         Transcript（对话滚动区）+ Editor（输入行）
agent/                核心 agent 循环（turn loop + 工具执行 + 全程事件）   ← pi-agent-core
llm/                  统一 LLM API：中立消息模型、流式事件、模型目录      ← pi-ai
  llm.go                Message / ToolCall / Provider / Stream 抽象
  openai.go             OpenAI 兼容协议（OpenAI/DeepSeek/Moonshot/Ollama/…）
  anthropic.go          Anthropic Messages API
  catalog.go            模型目录 + provider 推断/工厂
tools/                工具系统：JSON Schema 参数 + Execute + 活跃工具集   ← pi.registerTool
  bash.go / fs.go       内置工具：bash / read / write / edit
  render.go             工具渲染器契约：RenderCall / RenderResult + 解析器链
  builtin_renderers.go  内置工具的渲染器（read/write/edit/bash）
  diff.go               行级 diff（渲染器用来展示改动）
  mutation_queue.go     文件变更队列：同一文件的改动串行，不同文件并行
ext/                  扩展系统：事件总线 + Extension API + 加载器         ← pi.ExtensionAPI
  ext.go                事件总线、Extension API（hooks / 工具 / 命令 / 渲染器）
  loader.go             Manager：加载、卸载、热重载  scope.go              注册作用域：记录扩展注册了什么，重载时自动撤销
  protocol.go           进程扩展的 JSON-RPC 协议（stdio）
  process.go            进程扩展客户端：握手、工具桥接、事件转发
shellext/             `!` shell 快捷：用户直接跑命令，不经过模型
  shellext.go           解析 `!` / `!!`、user_bash 事件、结果格式化
prompts/              Prompt 模板：markdown 文件变成斜杠命令
  templates.go          模板解析（frontmatter）、目录发现、注册表
  expand.go             参数替换（$1 / $@ / ${1:-默认} / 切片）+ shell 式切分
projcontext/          项目上下文文件：AGENTS.md（项目指示）+ SYSTEM.md（系统提示词）
  context.go            从工作目录向上查找、折叠进系统提示词
skills/               Skills（Agent Skills 标准）：模型按 description 自己决定加载哪个 SKILL.md
  skills.go             SKILL.md 解析、校验、目录发现、名字冲突
  prompt.go             系统提示词里的渐进披露（只放 name/description/location）
  registry.go           注册表 + /skill:name 命令展开
  dirs.go               发现位置（.harness/skills、.agents/skills、~/.agents/skills）
frontmatter/          markdown 的 YAML frontmatter 解析（零依赖，供 prompts/ 和 skills/ 共用）
  frontmatter.go        标量 / 块标量（| 与 >）、转义、错误处理
rpc/                 RPC 模式：harness 当长驻子进程，被别的程序用 JSON 驱动  ← pi rpc-mode
  protocol.go          JSONL 分帧：命令 / 响应 / 事件、LF-only 切分
  commands.go          命令名、参数结构、返回载荷
  server.go            服务器：分发命令、把事件总线转成事件流
jsonout/             JSON 输出模式：一次性运行的完整事件流（--mode json）
  jsonout.go           会话头 + 事件记录、事件转发、答案提取
session/              会话持久化（JSONL 会话树，可 resume/list/fork）  ← pi-durable
  session.go           会话文件读写、v1 → v2 迁移
  tree.go              会话树：entry / 分支 / 上下文构建
  fork.go              分叉与克隆（fork / clone / 分支摘要）
mcp/                  Model Context Protocol 客户端
  protocol.go          JSON-RPC 2.0 消息类型与工具命名规则
  transport.go         传输层：stdio 子进程 / streamable HTTP
  client.go            连接、握手（initialize）、工具发现与调用
  resources.go         resources/list、resources/read、资源工具的载荷
  manager.go           多 server 管理：工具桥接进 tools.Registry
  config.go            mcp.json 读取、合并与校验
perm/                权限层（内核无内置权限，作为扩展挂在 tool_call 上）
  perm.go              规则模型：工具/参数/路径匹配，allow/ask/deny 判定
  config.go            内置规则（deny 底线）、permissions.json 读取与合并
  guard.go             Guard：tool_call 钩子、审批器、统计
  input.go             共享输入流（REPL 与审批提示共用一个 reader）
sandbox/             macOS Seatbelt 沙箱：写白名单 + 读黑名单 + 网络开关
telemetry/           厂商中立遥测契约（对齐 pi-telemetry）               ← pi-telemetry
  telemetry.go         Context/Span/Metrics/Propagation/Store 接口 + Noop 实现
  metrics.go           counter / upDownCounter / histogram + 类型化绑定
  schema.go            schema 定义（span / event / metric / 属性类型）
  harness_schema.go    harness 自己的 `pi.harness.*` 词汇表
  memory.go            内存参考实现（记录 + 查询 trace/metric）
  exporter.go          JSONL 导出器（按 schema 脱敏）
telemetryext/        遥测采集扩展：把事件总线的流量变成 span
  collector.go         Collector：订阅事件，括号式地开/关 span
telemetrybackend/    遥测后端：把契约的数据发到真正的 collector
  backend.go           Backend 接口 + Resource/Scope/MetricPoint
  otlp.go              OTLP/JSON 编码（trace / metric / 属性 / 标识符）
  client.go            OTLP/HTTP 客户端：批量、重试、统计
  aggregator.go        指标聚合：计数器求和、直方图分桶
  exporter.go          导出器：span 批量队列 + 指标定时刷新
  prometheus.go        Prometheus 名字/标签翻译 + 直方图展开
  prometheus_backend.go Prometheus remote write 客户端（protobuf + snappy）
  prompb/              remote write 的 protobuf 编码/解码（零依赖）
  snappy/              snappy 块压缩/解压（零依赖，与参考实现逐字节一致）
config/               配置：默认值 < ~/.harness/config.json < 环境变量
```

与 pi 的模块对应关系：

| pi (TypeScript)             | auto-harness (Go)            | 职责 |
|-----------------------------|------------------------------|------|
| `pi-ai`                     | `llm`                        | 多 Provider 统一 API |
| `pi-agent-core`             | `agent`                      | agent 循环、工具调用、状态 |
| `pi-durable`                | `session`                    | 会话持久化 |
| Extension API (`pi.on` 等)  | `ext`                        | hooks、注册工具/命令 |
| `pi-tui`                    | `tui`                        | 差分渲染终端 UI |
| `pi-telemetry`              | `telemetry` + `telemetryext` | 厂商中立遥测契约 + 采集扩展 |
| （pi 无内置后端）           | `telemetrybackend`           | OTLP/HTTP + Prometheus 后端（可插拔） |
| `pi-coding-agent`           | `cmd/harness`                | CLI |
| （pi 无内置权限）           | `perm` + `sandbox`           | 权限守卫（扩展）+ 沙箱执行 |
| `pi-coding-agent` 工具渲染器 | `tools/render.go`            | RenderCall / RenderResult + 解析器链 |
| `pi-coding-agent` `!` 快捷  | `shellext`                   | 用户直接跑 shell 命令 |
| `pi-coding-agent` 文件变更队列 | `tools/mutation_queue.go`  | 同一文件的改动串行化 |
| `pi-coding-agent` Prompt 模板 | `prompts`                   | markdown 文件变成斜杠命令 + 参数替换 |
| `pi-coding-agent` AGENTS.md  | `projcontext`               | 项目指示 + SYSTEM.md 替换系统提示词 |
| `pi-coding-agent` Skills   | `skills`                    | SKILL.md 技能库 + 渐进披露 + /skill:name |
| `pi-coding-agent` RPC 模式 | `rpc`                       | JSONL 命令/响应/事件，被别的程序驱动 |
| `pi-coding-agent` JSON 模式 | `jsonout`                  | 一次性运行的完整事件流（--mode json） |

## 快速开始

```bash
# 需要 Go 1.22+
go build -o bin/harness ./cmd/harness

# 设置 API key（二选一，provider 会自动从模型名推断）
export ANTHROPIC_API_KEY=sk-ant-...        # claude-* 模型
export OPENAI_API_KEY=sk-...               # 其他模型（可配合 OPENAI_BASE_URL）

# 交互式 REPL
./bin/harness

# 指定模型
./bin/harness -model claude-opus-5-5

# 走 OpenAI 兼容网关（DeepSeek/Moonshot/vLLM/…）
./bin/harness -model deepseek-chat -base-url https://api.deepseek.com/v1

# OpenCode Go 订阅（注意：Go 订阅走 /inference/go/ 路由，不是 /inference/）
# 按量付费路由会报 402 余额不足；harness 会自动带 x-opencode-session 会话头
export OPENAI_API_KEY=oc_sk-...
./bin/harness -model deepseek-v4.1-flash -base-url https://opencode.ai/inference/go/openai/v1

# one-shot 模式（脚本/自动化）
./bin/harness -print "解释一下这个项目的结构"

# JSON 输出模式：整次运行作为 JSON 事件流（会话头 → 事件 → agent_settled）
./bin/harness --mode json "解释一下这个项目的结构" | jq -r 'select(.type=="message_end") | .message.content'

# 显示推理模型的思考过程（stderr）
./bin/harness -show-thinking

# 差分渲染 TUI 模式（模型输出只重写变化的那一行，不刷屏）
./bin/harness -tui

# RPC 模式：stdin 收 JSON 命令，stdout 吐 JSON 记录，被别的程序驱动
./bin/harness -rpc

# 会话管理
./bin/harness -list-sessions
./bin/harness -session 20261111-153000.123456789   # 恢复会话
```

REPL 内置命令：`/help` `/tools` `/model <id>` `/models` `/sessions` `/compact` `/quiet` `/extensions` `/reload` `/tree` `/fork` `/clone` `/mcp` `/telemetry` `/prompts` `/skills` `/exit`。

`.harness/prompts/` 里的模板和 skill 也注册成斜杠命令，和上面这些并列出现在 `/help` 里。

REPL 里以 `!` 开头的行是 shell 快捷：`!cmd` 跑命令并把结果放进对话，`!!cmd` 只给你看、
模型看不到。

### Prompt 模板、项目上下文与 Skills

```bash
# 项目的斜杠命令：.harness/prompts/review.md → /review
mkdir -p .harness/prompts
cat > .harness/prompts/review.md <<'EOF'
---
description: Review the staged changes
argument-hint: "[focus]"
---
Review the staged changes. Focus on ${1:-correctness, security, and error handling}.
EOF

# 项目指示：AGENTS.md 追加到系统提示词
cat > AGENTS.md <<'EOF'
Prefer small, focused commits. Run the tests before you say you are done.
EOF

# Skills：模型按 description 自己决定读哪个 SKILL.md
mkdir -p .harness/skills/pdf-tools
cat > .harness/skills/pdf-tools/SKILL.md <<'EOF'
---
name: pdf-tools
description: Extract text and tables from PDF files. Use when reading, converting, or inspecting PDFs.
---
Read `references/formats.md` before converting a document.
EOF
```

模板和上下文文件都是**数据**：不改一行代码就能加命令、换提示词。

### 会话树（分支与分叉）

会话文件里的每条记录都是树上的一个节点（`id` / `parentId`），因此回到某个更早的点不会
抹掉你离开的那条分支——离开的分支仍然留在文件里。

| 命令 | 作用 | 什么时候用 |
|---|---|---|
| `/tree` | 在当前会话文件内移动，列出当前分支和可选的替代分支 | 相关备选方案放在一起 |
| `/tree <序号>` | 切到某个分支（模型上下文随之切换） | 换个思路继续 |
| `/fork [序号]` | 从某个更早的**用户消息**分叉出一个**新会话文件** | 替代方案是独立的工作 |
| `/clone [名字]` | 把当前分支整体复制成新会话 | 想要一份当前状态的副本 |

```
> /tree
session 20261111-170506.385055000 — 3 messages, 2 branch(es)

  [1] 000000025c88  BRANCH first question
  [2] 0000000539a0  BRANCH alternative answer
→ [3] 00000006d4c0  mockllm branch answer (user messages: 2)

alternative branches:
  [4] 00000003d240    mockllm branch answer (user messages: 1)

use /tree <index> to switch to a branch
```

分叉出的会话是独立文件，原会话不受影响；新会话的 header 里记着 `parentSession`，可以
用 `harness -session <id>` 继续。

会话文件格式（v2）在 pi 的格式基础上做了简化：entry 类型有 `message` / `model_change` /
`compaction` / `branch_summary` / `session_info`，外加一个 `leaf_pointer` 记录当前所在
分支的位置（重载会话时能回到你正在走的那条分支）。旧的 v1 线性格式在加载时自动迁移。


### 配置

优先级：内置默认 < `~/.harness/config.json` < 环境变量 < CLI flag。

```jsonc
// ~/.harness/config.json
{
  "provider": "anthropic",          // 可省略，按模型名推断
  "model": "claude-sonnet-5-5",
  "base_url": "",                   // 自定义网关
  "max_tokens": 8192,
  "max_turns": 25,
  "system_prompt": "",              // 覆盖内置系统提示词
  "session_dir": "",                // 默认 ~/.harness/sessions
  "catalog_url": "",                // 远程模型目录（启动时同步）
  "extension_dirs": [               // 进程扩展目录（扫描可执行文件）
    "~/.harness/extensions"
  ],
  "extension_command": "",          // 单个进程扩展命令，如 "python3 ext.py"
  "providers": [                    // 额外 provider：/model 可跨 provider 切换
    {
      "name": "go-anthropic",
      "base_url": "https://opencode.ai/inference/go/anthropic",
      "api_key": "oc_sk_...",
      "protocol": "anthropic"       // openai | anthropic | gemini
    }
  ],
  "permissions": {                  // 权限规则（排在用户级/项目级/内置之前）
    "default": "ask",
    "rules": [
      { "name": "allow my shell", "tools": ["bash"], "action": "allow" }
    ]
  },
  "sandbox": true,                  // macOS Seatbelt 限制 shell 执行
  "sandbox_write_paths": [],        // 写白名单，默认工作目录
  "sandbox_allow_network": false,   // 沙箱内是否允许网络
  "telemetry": false,               // 打开遥测采集（默认关）
  "telemetry_path": "",             // JSONL 输出路径，默认 ~/.harness/telemetry.jsonl
  "telemetry_endpoint": "",         // OTLP/HTTP collector，如 http://localhost:4318/v1/traces
  "telemetry_headers": {},          // OTLP 请求头（如 Authorization）
  "telemetry_interval_ms": 30000,   // 指标刷新间隔
  "telemetry_timeout_ms": 10000,    // 单次 OTLP 请求超时
  "telemetry_prometheus_endpoint": "", // Prometheus remote write，如 http://localhost:9090/api/v1/write
  "telemetry_prometheus_headers": {},  // remote write 请求头（如 X-Scope-OrgID）
  "no_permissions": false,          // 关掉整个权限层
  "no_mcp": false                   // 关掉 MCP
}
```

多 provider 的效果：`/models` 列出全部已知模型及其协议，`/model <id>` 切换时自动路由到能服务该模型的 provider（同一 harness 内可混用 OpenAI 兼容 / Anthropic / Gemini 三种协议）。

环境变量：`HARNESS_PROVIDER` / `HARNESS_MODEL` / `HARNESS_BASE_URL` / `HARNESS_SESSION_DIR` / `OPENAI_API_KEY` / `OPENAI_BASE_URL` / `ANTHROPIC_API_KEY` / `ANTHROPIC_BASE_URL`。

## 扩展开发

扩展 = 一个实现 `ext.Extension` 的单元（或用 `ext.Named` 包一个函数），在 `Setup(api)` 里注册 hooks、工具和命令。内置的 `stats` 扩展（[cmd/harness/ext_stats.go](cmd/harness/ext_stats.go)）是完整示例：

```go
ext.Named("my-ext", func(api *ext.API) error {
    // 1. hooks：观察/改写 harness 行为
    api.On(ext.EventToolExecutionEnd, func(e *ext.Event) error {
        log.Println("tool finished:", e.ToolCall.Name)
        return nil
    })

    // 2. 自定义工具（和内置工具完全同一套接口）
    api.RegisterTool(&tools.Tool{
        Name:        "current_time",
        Description: "Return the current local time in RFC3339 format.",
        Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
        Execute: func(ctx context.Context, callID string, args json.RawMessage, onUpdate func(tools.Update)) (tools.Result, error) {
            return tools.Result{Content: time.Now().Format(time.RFC3339)}, nil
        },
    })

    // 3. 自定义斜杠命令
    api.RegisterCommand("hello", &ext.Command{
        Description: "say hello",
        Handler: func(ctx context.Context, args string) error {
            fmt.Println("hello", args)
            return nil
        },
    })
    return nil
})
```

### 事件（hooks）一览

handler 签名 `func(e *ext.Event) error`，按注册顺序同步执行；出错不中断其他 handler（错误被收集返回）。可变字段就地修改（pi 同款语义）。

| 事件 | 时机 | 可变/可取消 |
|------|------|------------|
| `session_start` / `session_shutdown` | 会话开始/结束 | — |
| `before_agent_start` | 每次 Run 开始前 | 改 `e.Message`、`e.SystemPrompt`；`e.Cancel` 取消 |
| `agent_start` / `agent_end` | Run 开始/结束 | `agent_end` 携带 `e.Usage` |
| `turn_start` / `turn_end` | 每轮补全前后 | `turn_end`：`e.Continue` 强制续跑、`e.Messages` 注入消息 |
| `request_start` | 一次模型请求开始 | 只读：携带 `e.Request`（provider / model / 工具数），便于测量单次请求 |
| `message_start` / `message_update` / `message_end` | 助手消息流式产出 | `message_end` 可替换 `e.Message` |
| `tool_call` | 工具执行**之前** | 改 `e.ToolCall`（改参数）、`e.Block` + `e.BlockReason` 拦截 |
| `tool_execution_start` / `tool_execution_update` / `tool_result` / `tool_execution_end` | 工具执行生命周期 | — |
| `context` | 发给 Provider 前 | 替换 `e.Messages`（上下文改写） |
| `session_before_compact` / `session_compact` | 压缩历史 | 提供 `e.Messages` 扮演压缩器 |
| `input` | 用户输入 | 改写/接管输入 |
| `user_bash` | `!` 快捷执行 shell 命令前 | `e.Block` + `e.BlockReason` 拦截；携带 `e.BashCommand` 和 `e.ExcludeFromContext` |

### 进程扩展（任何语言）

Go 没有内置脚本解释器，所以扩展也可以是**独立的子进程**：harness 通过 stdio 上的
JSON-RPC 与它通信，扩展用任何语言写都行。

协议是换行分隔的 JSON：

```
harness → 扩展: {"id":1,"method":"manifest","params":null}
扩展 → harness: {"id":1,"result":{"name":"demo","tools":[{"name":"demo_echo",...}]}}
harness → 扩展: {"id":2,"method":"tool.execute","params":{"name":"demo_echo","arguments":{...}}}
扩展 → harness: {"id":2,"result":{"content":"...","is_error":false}}
扩展 → harness: {"method":"event","params":{"event":"input","text":"..."}}
```

扩展握手时返回自己的 manifest（名字 + 工具列表），harness 把这些工具注册成和内置
工具完全一样的工具；模型调用时，harness 把调用转发给子进程并把结果送回模型。扩展
也可以主动推送事件到 harness 的事件总线。

可执行文件放在 `~/.harness/extensions/`、`<项目>/.harness/extensions/` 或
`config.json` 的 `extension_dirs` 里（目录内的可执行文件都会被发现）。完整示例见
[examples/extensions/demo_extension.py](examples/extensions/demo_extension.py)：

```bash
chmod +x examples/extensions/demo_extension.py
./bin/harness -model deepseek-v4.1-flash \
  -base-url https://opencode.ai/inference/go/openai/v1
# 或在 config.json 里： "extension_dirs": ["examples/extensions"]
```

### 热重载

`/reload` 按 pi 的语义重载扩展：

1. 触发 `session_shutdown`（reason `reload`）
2. 撤销旧扩展注册的 hooks / 工具 / 命令，释放它持有的资源（子进程等）
3. 重新执行新扩展集的 `Setup`
4. 触发 `session_start`（reason `reload`）

扩展注册的内容由 harness 自动记录（[ext/scope.go](ext/scope.go)），所以扩展作者
不需要为重载写任何清理代码——只要把长期运行的资源（如子进程）实现
`ext.Teardown` 接口即可。旧闭包里捕获的状态在重载后即失效。`/extensions` 可以
查看当前加载的扩展。


## 内置工具

| 工具 | 参数 | 说明 |
|------|------|------|
| `bash` | `command`, `timeout_ms?`, `workdir?` | 执行 shell 命令（默认 120s 超时，输出截断 30k 字符） |
| `read` | `path`, `offset?`, `limit?` | 带行号读取文本文件 |
| `write` | `path`, `content` | 写文件（自动建目录） |
| `edit` | `path`, `old_string`, `new_string`, `replace_all?` | 精确字符串替换（不唯一时报错） |

工具可通过 `Registry.SetActiveTools` 控制暴露给模型的子集。

### 工具渲染器

工具的**呈现**与它的**执行**是分开的：一个工具可以自带渲染器，也可以把呈现交给别人。
渲染器只有两个钩子——画出一次调用（`RenderCall`）和画出一次结果（`RenderResult`）——
和 pi 的 `ToolRenderers` 一一对应：

```go
api.RegisterToolRenderer(func(name string, next func() *tools.Renderers) *tools.Renderers {
    if name != "mcp__docs__search" {
        return next()   // 不关心这个工具，问下一个解析器
    }
    return &tools.Renderers{
        RenderCall: func(args map[string]any, ctx tools.RenderContext) ([]string, error) {
            return []string{"正在检索文档…"}, nil
        },
        RenderResult: func(result tools.Result, opts tools.RenderResultOptions, ctx tools.RenderContext) ([]string, error) {
            return []string{result.Content}, nil
        },
    }
})
```

解析器按注册顺序串成一条链（pi 的 `resolveToolRenderers`）：每个解析器可以自己给渲染器，
也可以调 `next()` 把问题交给下一个；链的末端是工具自带的渲染器，再末端是内置渲染器。
所以扩展可以接管**任何**工具的呈现——包括不是它自己的 MCP 工具。

内置工具（`read` / `write` / `edit` / `bash`）各有自己的渲染器：调用行显示路径或命令，
结果按需折叠（`tools.DiffLines` 提供行级 diff）。

### `!` shell 快捷

在 REPL 里以 `!` 开头的行不发给模型，直接当 shell 命令跑：

```
> !ls -la              # 跑命令，输出进对话，模型下一轮能看到
> !!git status         # 跑命令，输出只给你看，模型看不到
```

- `!cmd`：命令跑在**同一个 bash 工具**里，所以沙箱照样生效；结果按 pi 的格式
  （``Ran `cmd` `` + 围栏输出）进入对话。
- `!!cmd`：输出不进对话。适合"看一眼结果但不想污染上下文"的场景。
- 跑之前会发 `user_bash` 事件，扩展可以拦下来（`e.Block = true`）。

## 文件变更队列

同一文件的改动必须串行——两个并发 edit 读到同一份旧内容、写回两份新内容，其中一份就被
覆盖了。`tools/mutation_queue.go` 给每个**真实路径**（`EvalSymlinks` 后的路径，
文件还不存在时用解析后的绝对路径）一条队列：

- 同一文件的改动排队执行，一个接一个，不交错；
- 不同文件的改动仍然并行。

`write` 和 `edit` 两个内置工具都走这条队列。

## Prompt 模板、项目上下文与 Skills

pi 的「自己可改」不只是扩展 API：模板、上下文文件和 skill 都是**数据**——markdown 文件，
不改一行代码就能加命令、换提示词、加一套专门的工作流程。

### Prompt 模板（[prompts/](prompts/)）

`.md` 文件变成斜杠命令：文件名就是命令名（`review.md` → `/review`）。

```markdown
---
description: Review the staged changes
argument-hint: "[focus]"
---
Review the staged changes. Focus on ${1:-correctness, security, and error handling}.
```

- 模板放在项目的 `.harness/prompts/` 或用户的 `~/.harness/prompts/`；
- `description` 省略时取正文第一行（非空行）；
- 参数按 **shell 引号规则**切分：`/review "API compatibility"` 是一个参数，不是两个。

替换语法：

| 写法 | 含义 |
|------|------|
| `$1` `$2` … | 第 n 个参数 |
| `$@` / `$ARGUMENTS` | 全部参数，用空格连接 |
| `${1:-默认值}` | 第 n 个参数，缺省时用默认值 |
| `${@:-默认值}` | 全部参数，没有时用默认值 |
| `${@:N}` / `${@:N:L}` | 从第 n 个起 / 从第 n 个起取 L 个 |

替换是**单遍**的：被替换进去的内容不会被再替换一遍——模型自己的输出变不成新的占位符。

模板注册在**和内置命令、扩展命令同一个注册表**上，所以 `/help` 直接列出它们，
`/prompts` 还会显示每个模板的来源路径（模板不对，多半是目录找错了）。

### 项目上下文文件（[projcontext/](projcontext/)）

| 文件 | 作用 |
|------|------|
| `AGENTS.md` | 项目指示，**追加**到系统提示词后面 |
| `SYSTEM.md` | **替换**系统提示词 |

`AGENTS.md` 从工作目录**向上**找：仓库根上的那份对所有子目录生效；最近的同名文件
优先（子目录的 AGENTS.md 盖住上层的）。`SYSTEM.md` 只从工作目录本身读——从父目录
继承一份系统提示词太意外了。

上下文文件里的内容**进不了遥测**：它是数据，不是可执行的东西，而且它只影响模型
看到什么。

### Skills（[skills/](skills/)）

模板是**用户敲命令**才用的；Skills 是**模型自己按 description 决定**要不要读的。
两者一起才是 pi 完整的「自己可改」。

Skill 是一个目录，里面放一个 `SKILL.md`：

```text
pdf-tools/
├── SKILL.md
├── scripts/extract.sh
├── references/formats.md
└── assets/template.json
```

```markdown
---
name: pdf-tools
description: Extract text and tables from PDF files. Use when reading, converting, or inspecting PDFs.
---

# PDF tools

Read `references/formats.md` before converting a document. Run scripts relative to this skill directory.
```

**渐进披露（progressive disclosure）**：启动时只有 `name`、`description`、`location`
进系统提示词（XML 格式），完整指示留在磁盘上；模型觉得任务对上了，才用 `read` 工具
去读 `SKILL.md`。所以一个 50 个 skill 的库，代价是 50 行提示词，不是 50 份文档。

发现位置（按此顺序，**先到先得**，重名时先发现的赢）：

| 位置 | 范围 |
|------|------|
| `<项目>/.harness/skills/` | 项目 |
| `<项目>/.agents/skills/` 及其各层父目录 | 项目（Agent Skills 标准目录） |
| `~/.harness/skills/` | 用户 |
| `~/.agents/skills/` | 用户（Agent Skills 标准目录） |

项目在前、用户在后：项目定义的 skill 归项目，用户自己的目录是项目没用到的名字的
兜底——和权限层「用户规则在前」的优先级是反的，方向一致。

Frontmatter 字段：

| 字段 | 必填 | 说明 |
|------|------|------|
| `name` | 否 | 缺省取目录名；只允许小写字母、数字、连字符，最长 64 字符 |
| `description` | **是** | 模型靠它路由。没有 description 的 skill **不会被加载** |
| `disable-model-invocation` | 否 | `true` 表示只允许 `/skill:name` 显式调用，不进系统提示词 |

其余字段（`license`、`compatibility`、`metadata`、`allowed-tools`）照收不误，只是
harness 不用。**不合规只警告不拒绝**：一个超长描述的 skill 还是能用的，只是不那么
可移植——这和 pi 的做法一致，坏掉一个文件不该拖垮整个库。

两个入口：

- **模型自己读**：描述对上了，模型调 `read` 读 `SKILL.md`；
- **`/skill:name`**：显式命令，指示直接进对话，不依赖模型选择。参数跟在后面，
  作为「把这些指示用在哪」的请求。

```
/skill:pdf-tools extract report.pdf
```

显式调用时 skill 的内容会包在一个 `<skill>` 块里，块里写明**是用户主动调用的**——
`disable-model-invocation` 管的是「模型自己能不能选这个 skill」，不是「用户点名了要不要
照做」。少了这一句，模型自己再去读 `SKILL.md` 就会看见那个 flag 然后拒绝执行。

`/skills` 列出所有 skill 和它们的来源路径——skill 做的不是描述里说的，多半是目录
找错了。skill 的指示**不能自己执行任何东西**：它就是模型的输入，模型要跑脚本仍然
得走工具，走同一套权限。

## RPC 模式（[rpc/](rpc/)）

REPL 是给人用的。要把 harness 嵌进 IDE、脚本（任何语言）或者自制的 UI 里，
`-rpc` 让它变成一个**长驻子进程**：stdin 收 JSON 命令，stdout 吐 JSON 记录。

```bash
./bin/harness -rpc
```

```json
{"id":"r1","type":"prompt","message":"解释一下这个项目的结构"}
{"id":"r1","type":"response","command":"prompt","success":true,"data":{"disposition":"started"}}
{"type":"agent_start","data":{"type":"agent_start"}}
{"type":"message_end","data":{"message":{"role":"assistant","content":"……"}}}
{"type":"agent_settled","data":{"aborted":false}}
```

**stdout 归协议所有**：RPC 模式下模型文本、工具提示、诊断全部走 stderr，stdout 上
只有 JSON 记录。一个读 stdout 的客户端永远不该看到别的东西。

分帧是严格的 JSONL：记录之间只隔一个 LF。记录里的字符串可以含 U+2028 / U+2029
（那在 JSON 里是普通字符），所以切分**只能**按 LF——按行切但也切 U+2028 的读法会把
一条记录劈成两半。CRLF 的客户端也接受。

**响应和事件共用一条流**，所以客户端要**按 ID 关联**，不能按顺序猜：

| 记录 | 形状 | 说明 |
|------|------|------|
| 响应 | `{"id":"…","type":"response","command":"…","success":true,"data":{…}}` | 回答一条命令；`error` 说明失败原因 |
| 事件 | `{"type":"…","data":{…}}` | 会话活动：`agent_start` / `turn_start` / `message_update` / `tool_execution_start` / `request_start` / `agent_end` / `agent_settled` … |

命令清单：

| 命令 | 参数 | 作用 |
|------|------|------|
| `prompt` | `message` | 发一句话给模型。**响应在运行开始时就回**，`data.disposition` 是 `started`，答案走事件流 |
| `abort` | — | 停掉当前运行 |
| `new_session` | — | 开一个新会话 |
| `get_state` | — | 当前模型、会话 id、消息数、是否在跑 |
| `get_messages` | — | 对话历史 |
| `set_model` | `model` | 换模型 |
| `get_available_models` | — | 已知模型列表 |
| `set_thinking` | `enabled` | 开关思考模式 |
| `compact` | — | 压缩历史 |
| `bash` | `command` | 跑一条 shell 命令（走同一个 bash 工具，沙箱和权限照样生效） |
| `get_session_stats` | — | token 用量统计 |
| `get_entries` / `get_tree` | — | 会话树的 entry |
| `get_last_assistant_text` | — | 最后一条助手消息 |
| `set_session_name` | `name` | 给会话命名 |
| `get_commands` | — | harness 知道的斜杠命令（内置 + 模板 + skill） |
| `fork` / `clone` | `entryId` | 分叉会话 |

**`prompt` 是异步的**：响应只说明「收下了」，答案在事件流里。客户端要等
`agent_settled`（而不是 `agent_end`——一次运行后面可能还跟着重试）才知道跑完了。

一条坏记录不会毁掉会话：JSON 解析失败回一条
`{"type":"response","command":"parse","success":false,"error":…}`，流继续。
未知命令同理。

**关掉 stdin 是有序关机**：harness 会等在跑的那次运行结束再退出——所以「发完 prompt
立刻关 stdin」的客户端仍然能拿到答案。

权限层在 RPC 模式下**不弹窗**：stdin 归协议所有，审批提示会吃掉客户端的记录。
于是模型侧的调用走「无人可问就拒绝」，客户端自己发的 `bash` 走扩展的 `user_bash`
拦截——那是用户自己的命令，不是模型的。

## JSON 输出模式（[jsonout/](jsonout/)）

`-print` 给的是答案，`--mode json` 给的是**整次运行**。

```bash
./bin/harness --mode json "解释一下这个项目的结构"
```

```json
{"type":"session","version":2,"id":"20261112-124721.536716000","timestamp":"2026-11-12T12:47:21.536Z","cwd":"/work"}
{"type":"agent_start"}
{"type":"turn_start"}
{"type":"request_start","request":{"provider":"openai","model":"deepseek-v4.1-flash","tools":5}}
{"type":"message_start"}
{"type":"message_update","text":"解释"}
{"type":"message_update","text":"一下"}
{"type":"message_end","message":{"role":"assistant","content":"解释一下……"}}
{"type":"turn_end"}
{"type":"agent_end","usage":{"inputTokens":969,"outputTokens":4}}
{"type":"agent_settled"}
```

**第一行是会话头**：消费者看到任何事件之前就知道这是哪个会话的——事件流可以存成
文件，靠头找回会话。**最后一行是 `agent_settled`**：这一行之前没有更多自动工作了，
读到它就可以停。

和 RPC 模式的区别：RPC 模式是**被驱动**的（stdin 收命令，可以来来回回），JSON 模式是
**一次性**的（给一句提示，跑完，退出）。两者共用 JSONL 分帧，但记录的形状不同——
RPC 的记录是「响应 + 事件」，JSON 模式只有「事件」。

事件名就是 harness 自己的事件名，消费者不用学第二套词汇。几个要点：

- **`message_update` 只带增量**，不带整条消息——消息本身在 `message_start` 和
  `message_end` 里。增量多了，整条消息就跟着膨胀。
- **推理的增量带 `"thinking": true`**，和答案的增量分开标记，否则同一个流里分不清
  哪段是模型在想、哪段是它在答。
- **工具调用带 `toolCall`**，工具结果带 `result`（含 `isError`）——渲染一次运行需要
  知道跑了什么、结果如何。
- **token 用量在 `agent_end` 的 `usage` 里**。

stdout 归事件流所有：模型文本、工具提示、诊断全走 stderr。退出码报告这次运行成功
没有，脚本不用解析流就知道该不该继续。

## MCP 接入

支持 [Model Context Protocol](https://modelcontextprotocol.io)：harness 连接 MCP server，
把它们的工具注册成普通工具，模型像调用内置工具一样调用它们。

**配置**：`~/.harness/mcp.json`（用户级）和 `<项目>/.harness/mcp.json`（项目级，
需 `trust_project`）——格式与其他 MCP 客户端一致：

```jsonc
{
  "mcpServers": {
    "filesystem": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-filesystem", "."],
      "env": { "TOKEN": "${MY_TOKEN}" }
    },
    "docs": {
      "url": "https://example.com/mcp",
      "headers": { "Authorization": "Bearer ${DOCS_TOKEN}" },
      "description": "Search the product documentation"
    }
  }
}
```

也支持在 `~/.harness/config.json` 里用 `mcp_servers` 声明（同名时以它为准）。

**传输**：stdio（`command`/`args`/`env`/`cwd` 子进程）和 streamable HTTP（`url`/`headers`）。
SSE 传输不支持。

**工具命名**：`mcp__<server>__<tool>`，非法字符替换为 `_`——不同 server 的工具不会冲突。

**MCP resources**：server 声明了 `resources` 能力时，harness 会额外注册三个工具，
沿用 pi 的命名（模型认得）：

| 工具 | 参数 | 说明 |
|------|------|------|
| `list_mcp_resources` | `server?`, `cursor?` | 列出所有（或指定）server 的资源 |
| `list_mcp_resource_templates` | `server?`, `cursor?` | 列出资源模板（RFC 6570 URI 模板） |
| `read_mcp_resource` | `server`, `uri` | 读取一个资源 |

不带 `server` 时覆盖所有带资源的 server；某个 server 列举失败不会挡住其他 server，
失败会出现在结果的 `errors` 里。二进制资源只描述、不下载（harness 的消息模型只有文本）。

**命令**：

```bash
./bin/harness -mcp-list          # 连接所有 server，打印状态和工具数
```

```
> /mcp                            # 查看已连接的 server
> /mcp add fs -- npx -y @modelcontextprotocol/server-filesystem .
> /mcp remove fs
```

**行为**：一个 server 连不上只会被报告，不影响其他 server；`enabled: false` 的条目保留
但不连接；单次请求超时默认 60 秒（`timeout` 可调）。

完整示例见 [examples/extensions/demo_mcp_server.py](examples/extensions/demo_mcp_server.py)：

```bash
./bin/harness -mcp-list   # 用 ~/.harness/mcp.json 里的配置连接
```

## 权限与沙箱

pi 的内核没有内置权限系统——「以什么权限读写执行」是留给扩展的一层空白。
harness 沿用这个设计：权限守卫是一个扩展，挂在 `tool_call` 钩子上，
可以在工具真正执行前放行、拦截或改写参数。

**判定流程**（[perm/](perm/)）：

1. **内置 deny 规则先判**——`rm -rf`、磁盘操作、`sudo`/`su`/`doas`、
   写 `.git/`、`.env`、`~/.ssh/`、`~/.aws/credentials` 一律拒绝。
   这几条是**安全底线**，配置里的 allow 规则抬不动它们。
2. 其余规则按顺序**首个命中即决定**。顺序按来源排：
   `config.json` 的 `permissions` → 用户级 `permissions.json` → 项目级
   `permissions.json` → 内置默认值——越具体的来源越靠前。
3. 都没命中就用 `default`（默认 `ask`）。

**三种结果**：`allow` 直接执行；`ask` 问用户；`deny` 拦截。被拦截的调用
会作为工具错误结果回给模型，模型能看到「为什么被拒」并换一条路。

**配置**：`~/.harness/permissions.json`（用户级）和
`<项目>/.harness/permissions.json`（项目级，需 `trust_project`）：

```jsonc
{
  "default": "ask",
  "rules": [
    { "name": "allow my project shell", "tools": ["bash"], "action": "allow" },
    {
      "name": "never touch the deploy script",
      "tools": ["write", "edit"],
      "paths": ["(^|/)deploy\\.sh$"],
      "action": "deny",
      "reason": "the deploy script is off limits"
    },
    {
      "name": "ask before force-pushing",
      "tools": ["bash"],
      "args": { "command": "(?i)git\\s+push\\s+.*--force" },
      "action": "ask"
    }
  ]
}
```

规则的每个字段都是可选的：空 `tools` 匹配所有工具，`args` 按参数名做正则
匹配，`paths` 对目标路径做正则匹配。

**交互确认**：需要确认时 REPL 会提示 `allow? [y/N]`。REPL 和提示框共用
同一个输入流，所以提示框不会吃掉你下一条命令；TUI 模式下提示框直接进
转录区，不和差分渲染打架。**没有交互会话时（`-print`、管道输入）默认拒绝**
——pi 的原则是「没人能确认就拦下来」，绝不静默放行。

**命令**：

```
> /permissions                    # 打印当前生效的规则、sandbox 状态和计数
> /permissions allow ./build      # 把某个路径加入 allow 规则（写回用户级文件）
```

**沙箱**（[sandbox/](sandbox/)）：macOS 上用 Seatbelt (`sandbox-exec`) 限制
shell 命令。默认**写白名单**（工作目录）+ **读黑名单**（`~/.ssh`、`~/.aws`、
`~/.gnupg`、`~/.config/gh`、`~/.kube`、`~/.docker`），网络默认关闭。
规则顺序是「先 deny 后 allow」——Seatbelt 是最后匹配者胜出。

```jsonc
{
  "sandbox": true,
  "sandbox_write_paths": ["/Users/me/project"],  // 默认是工作目录
  "sandbox_allow_network": false                 // 默认关闭
}
```

沙箱不可用时（非 macOS 或没有 `sandbox-exec`）工具输出会明确标注
`[no sandbox available: the command ran unrestricted]`，不会假装受控。

## 遥测契约

对齐 [pi-telemetry](https://github.com/earendil-works/pi) 的**厂商中立遥测契约**
（[telemetry/](telemetry/)、[telemetryext/](telemetryext/)）。

pi 把遥测定义成一套契约而不是一个后端：适配器负责落地，契约只规定
「记什么、怎么记」。harness 沿用这个分工——契约在内核之外，采集器是
又一个扩展，内核不知道遥测存在。

**契约的三块**（[telemetry/telemetry.go](telemetry/telemetry.go)）：

- **`Context`**：`StartSpan(options, callback)` 在回调周围开一个 span，
  回调返回时 span 落定。回调同步、恰好执行一次，错误原样返回。
  契约里**没有公开的 `end()`**——pi 用 promise 表达 span 的生命周期，
  Go 的回调是同步的，所以 `telemetry.Deferred` 换了个等价的写法：
  在自己的 goroutine 上跑 `StartSpan`，回调里阻塞，释放函数负责唤醒。
- **`Metrics` / `Propagation`**：可选能力，但适配器交出的每个 context
  都带着它们，嵌套操作不必再配置一次。
- **`Store`**：读回落定的 trace 和 metric 行，不绑定任何存储后端。

**pi 刻意不做的三件事，harness 也不做**：不发包（导出器由宿主决定）、
没有全局「当前 span」状态（上下文显式传递）、不校验调用方输入
（适配器信任类型化的输入）。

**harness 自己的词汇表**（[telemetry/harness_schema.go](telemetry/harness_schema.go)）：

| span | 含义 |
|------|------|
| `pi.harness.session` | 一次会话，从启动到关闭 |
| `pi.harness.request` | 一次模型请求，从上下文组装到最后一次流事件 |
| `pi.harness.tool` | 一次工具执行 |
| `pi.harness.compaction` | 一次上下文压缩 |

| metric | 类型 | 含义 |
|--------|------|------|
| `pi.harness.request.duration` | histogram (ms) | 单次请求耗时 |
| `pi.harness.first_chunk.duration` | histogram (ms) | 首字节延迟 |
| `pi.harness.tokens` | counter | token 用量（input/output/cache_read/cache_write） |
| `pi.harness.tool.calls` | counter | 工具调用次数 |

**隐私策略（由 schema 强制，不是靠自觉）**：只记**事实**，不记**内容**。
prompt、completion、工具参数、工具输出、文件路径、provider 原始载荷
一律不进属性——只有 harness 本来就知道的名字、计数、耗时、token 数
才成为属性。属性值只允许标量或标量数组；metric 属性一律低基数。

这条策略是被测试守着的，不是写在文档里就算数：

- `telemetryext` 的测试用带特征串的 prompt / 工具参数 / 工具输出跑一遍
  会话，然后断言这些串**没有出现在任何属性里**；
- `e2e/telemetry_test.go` 用真实模型再验一遍。

导出器还会按 schema 把标记为 `Sensitive` 的属性丢掉（默认开启脱敏）。

**开启**：

```jsonc
// ~/.harness/config.json
{
  "telemetry": true,
  "telemetry_path": "~/.harness/telemetry.jsonl"   // 默认就是这个
}
```

```
> /telemetry            # 看当前状态和已记录的 span
> /telemetry path       # 看输出文件路径
> /telemetry endpoint   # 看 OTLP collector 地址
> /telemetry prometheus # 看 Prometheus remote write 地址
> /telemetry on|off     # 改开关（下次运行生效）
```

**输出格式**：JSONL，一行一个 span：

```json
{"traceId":"a1b2:1","spanId":"3","parentId":"2","name":"pi.harness.tool","startedMs":1794422378004.3,"endedMs":1794422378004.4,"attributes":{"pi.harness.tool":"bash","pi.harness.tool_error":true},"status":{"status":"error","error":{"Name":"ToolError","Message":"the tool returned an error result"}}}
```

### 遥测后端：OTLP/HTTP

[telemetrybackend/](telemetrybackend/) 把契约里的数据发到真正的 collector。
协议是 **OTLP over HTTP + JSON 载荷**——所有 OpenTelemetry collector 都收，
而且不需要 protobuf 依赖。

契约只说「记什么」，后端说「发到哪」，两者分开是故意的：契约保持厂商中立，
后端是契约到某个协议的翻译。

**接口**（[backend.go](telemetrybackend/backend.go)）：

```go
type Backend interface {
	SendTraces(ctx context.Context, spans []telemetry.StoredSpan) error
	SendMetrics(ctx context.Context, points []MetricPoint) error
	Flush(ctx context.Context) error
	Close() error
}
```

再加一个后端就是实现这四个方法，不碰契约。

**OTLP/JSON 编码有两个坑**（[otlp.go](telemetrybackend/otlp.go)），手写编码器
必须自己处理：

- **64 位整数是十进制字符串**，不是数字（`"startTimeUnixNano":"1794422378004327000"`）。
  这是 protobuf JSON 映射的规定；collector 两种都收，但字符串才是规范。
- **trace / span 标识符是 hex 字符串**，不是 base64。这是 OTLP 规范里
  protobuf JSON 映射**唯一的偏离**——通用 protobuf JSON 编码器会给你
  base64，那不是合法的 OTLP/JSON。

契约里的标识符是**不透明的字符串**（`"96a8555455163d4c:1"`），而 collector
只收 16 字节 / 8 字节的 hex。所以编码器会把不合规的标识符**哈希**成正确
长度的 hex——同一个输入永远得到同一个结果，trace 的形状因此跨请求保持稳定。

**批处理**：span 进队列，后台 goroutine 攒批发送；一次 run 几百个 span，
一个一个发会把 collector 打爆。指标按间隔刷新：counter 每条测量发一次
就是一次请求，没有意义——先聚合再发。

**聚合**（[aggregator.go](telemetrybackend/aggregator.go)）：计数器按
「名字 + 属性集」求和，直方图按 schema 的边界分桶。这正是 collector 想要的
形态（每个区间一个速率或一个分布），而不是原始测量。

**可靠性**：5xx 重试一次（collector 只是暂时忙）；4xx 不重试（载荷不对，
重试还是会失败）；失败的批次计入 `droppedTraces` / `droppedMetrics`，
不会拖慢 agent——慢的 collector 不该让 agent 变慢。

**配置**：

```jsonc
// ~/.harness/config.json
{
  "telemetry": true,
  "telemetry_endpoint": "http://localhost:4318/v1/traces",  // OTLP/HTTP collector
  "telemetry_headers": { "Authorization": "Bearer ..." },  // 每次请求都带
  "telemetry_interval_ms": 30000,   // 指标刷新间隔
  "telemetry_timeout_ms": 10000,    // 单次请求超时
  "telemetry_file": true            // 同时保留本地 JSONL（默认开）
}
```

metrics 端点从 traces 端点推导（`/v1/traces` → `/v1/metrics`）。
本地 JSONL 和远端 collector 可以同时开。

### 遥测后端：Prometheus remote write

第二个后端（[prometheus.go](telemetrybackend/prometheus.go)、
[prometheus_backend.go](telemetrybackend/prometheus_backend.go)）把指标发到
Prometheus。协议是 **remote write**：protobuf 载荷 + snappy 压缩。

选它不是为了多一个后端，而是为了证明 `Backend` 接口真的是**协议无关**的：
Prometheus 的数据模型和契约差得很远——它存的是时间序列，没有 trace。

**它没有 trace，于是它就不假装有。** `SendTraces` 是空操作，但会记进
`DroppedTraces`：把一个 counter 当 trace 存是编造契约里没有的数据。

**名字翻译**（[prometheus.go](telemetrybackend/prometheus.go)）：

- 契约用点分名（`pi.harness.request.duration`），Prometheus 只认
  `[a-zA-Z_:][a-zA-Z0-9_:]*`，所以点变下划线：`pi_harness_request_duration`。
  翻译是**稳定的**——同一个契约名永远得到同一个 Prometheus 名，dashboard
  跨 run 不会失效。
- 指标名放在 `__name__` 标签里，这是 Prometheus 自己的做法（不是字段）。
- counter 加 `_total` 后缀（Prometheus 的约定），upDownCounter 是 **gauge**
  ——它会降，就不是 counter。

**直方图展开**成 Prometheus 的经典三件套：每个桶一条 `<metric>_bucket`
（带 `le` 标签），外加 `<metric>_count` 和 `<metric>_sum`。桶的计数是
**累加**的：Prometheus 的一个桶装的是「所有不超过这个上界」的观测，而
聚合器把每次观测记在唯一一个桶里——不累加的话 `le` 标签就是错的。

**零依赖**：remote write 要 protobuf + snappy，而 harness 的 `go.mod` 是空的。
所以两个编码器都是手写的（[prompb/](telemetrybackend/prompb/)、
[snappy/](telemetrybackend/snappy/)），**上游实现只作为测试里的 oracle**——
测试把它们从 module cache 编译出来，逐字节比对，所以两者不可能悄悄分叉。
oracle 在**自己的 module** 里编译，harness 的 `go.mod` 依然是空的。

载荷还带 `MetricMetadata`（family 名、help、unit、类型）——协议里有这个字段，
填上总比不填好：Prometheus 自己的 v1 接收端目前忽略它，但 v2 路径会读，
别的 remote write 接收端用它显示指标的 help 文本。

**配置**：

```jsonc
// ~/.harness/config.json
{
  "telemetry": true,
  "telemetry_prometheus_endpoint": "http://localhost:9090/api/v1/write",
  "telemetry_prometheus_headers": { "X-Scope-OrgID": "harness" },
  "telemetry_interval_ms": 30000
}
```

OTLP 和 Prometheus 可以同时开：span 去 OTLP，指标两边都去。

## 测试

```bash
go vet ./...
go test ./...            # 单元 + 离线集成（不联网）
```

### 真实模型端到端测试

[e2e/](e2e/live_test.go) 里的测试打真实网关，模型固定为 `deepseek-v4.1-flash`（OpenCode Go 路由）。它们用 `e2e` build tag 隔离，默认 `go test ./...` 不会执行：

```bash
export OPENCODE_API_KEY=<你的 key>
go test -tags e2e -timeout 600s -v ./e2e/
```

覆盖：文本补全、bash 工具调用闭环、文件工具读写改往返、会话持久化与恢复、上下文压缩后信息保真、推理流透传且不污染转录、工具失败后的恢复、上下文超时中止、进程扩展工具、MCP 工具、MCP resources、会话分叉与分支、权限拦截与放行、遥测落盘、OTLP 投递、Prometheus remote write 投递、工具渲染器接管呈现、`!` 快捷（含 `!!` 不进上下文）、文件变更队列串行化、Prompt 模板参数替换、项目上下文文件（AGENTS.md / SYSTEM.md）、Skills（渐进披露、模型自己读 SKILL.md、`/skill:name` 显式调用、参数传递、`disable-model-invocation` 隐藏、skill 不能自己执行命令）、RPC 模式（子进程协议：prompt 出答案、状态与历史、bash 命令与退出码、未知命令不毁会话、skill 走 RPC 也生效、关掉 stdin 后仍等运行结束）、JSON 输出模式（会话头在前、事件流完整、增量拼出答案、工具调用与结果在流上、非法模式值报错）。

未设置 key 时这些测试自动跳过（`t.Skip`），所以 CI 上安全。

### CI

[.github/workflows/ci.yml](.github/workflows/ci.yml) 跑两个 job：

- **build**：`ubuntu-latest` 和 `macos-latest` 两个平台上跑 `go build ./...`、`go vet ./...`、`go test ./...`、`go test -race ./...`，外加一个 gofmt 检查。工具链跟着 `go.mod` 走（`go-version-file: go.mod`），所以 CI 用的版本就是模块声明的那个。Linux 能跑是因为终端代码按平台拆开了：`tui/term_bsd.go` 用 BSD 的 termios 布局，`tui/term_linux.go` 用 Linux 的（控制字符在前，标志位在后，顺序相反）。**tag 上也会跑**（`tags: ["v*"]`）。
- **e2e**：真实模型的端到端测试。key 从仓库 secret `OPENCODE_API_KEY` 读，**没有 key 时这一步跳过而不是失败**（`if: env.OPENCODE_API_KEY != ''`）——所以公开仓库克隆下来也不会红。模型固定 `deepseek-v4.1-flash`，和测试源码里写死的一致。

## Roadmap（深度拓展方向）

按 pi 的演进路径，预留的深化方向：

已完成的深化项：

- **JSON 输出模式**（[jsonout/](jsonout/)、[cmd/harness/json.go](cmd/harness/json.go)）：`-print` 给的是答案，`--mode json` 给的是**整次运行**——这是 harness 层最后一个缺口（pi 里剩下的两个缺口：RPC 模式与 JSON 模式，都已补上）。输出是 JSONL 事件流：**第一行是会话头**（消费者看到任何事件之前就知道是哪个会话的，事件流存成文件能靠头找回会话），**最后一行是 `agent_settled`**（这一行之前没有更多自动工作，读到它就可以停）。**事件名就是 harness 自己的事件名**，消费者不用学第二套词汇——`agent_start` / `turn_start` / `request_start` / `message_start` / `message_update` / `message_end` / `tool_execution_start` / `tool_execution_end` / `agent_end`。两个设计点：`message_update` **只带增量**不带整条消息（增量多了整条消息跟着膨胀，消息本身在 `message_start` / `message_end` 里）；**推理的增量带 `"thinking": true`**，和答案的增量分开标记，否则同一个流里分不清哪段在想哪段在答。和 RPC 模式的分工：RPC 是**被驱动**的（stdin 收命令，来来回回），JSON 模式是**一次性**的（给一句提示、跑完、退出），两者共用 JSONL 分帧但记录形状不同（RPC 是「响应 + 事件」，JSON 只有「事件」）。stdout 同样归事件流所有，退出码报告运行成不成功
- **RPC 模式**（[rpc/](rpc/)、[cmd/harness/rpc.go](cmd/harness/rpc.go)）：harness 从「人用的 REPL」变成**长驻子进程**，被别的程序用 JSON 驱动——IDE、任何语言的脚本、自制 UI 都行，这是把 harness 嵌进别人产品里的唯一入口。协议是严格的 JSONL：stdin 收命令，stdout 收响应和事件；**stdout 归协议所有**（模型文本、工具提示、诊断全改走 stderr），一个读 stdout 的客户端永远不该看到别的东西。分帧**只按 LF 切**——记录里的字符串可以含 U+2028 / U+2029（JSON 里是普通字符），按它们切会把一条记录劈成两半；CRLF 客户端也接受。**响应和事件共用一条流**，所以客户端按 **ID 关联**而不是按顺序猜（一次运行的十几个事件会插在命令的响应之间）。`prompt` 是异步的：响应只说明「收下了」（`data.disposition` = `started`），答案走事件流，客户端要等 `agent_settled`（不是 `agent_end`——一次运行后面可能还跟着重试）；**响应必须先于运行写出**，否则一个等整轮跑完再读的客户端会在写满的管道上死锁。一条坏记录不毁会话：解析失败回一条 `command: "parse"` 的失败响应，流继续；未知命令同理。**关掉 stdin 是有序关机**：harness 等在跑的那次运行结束再退出，所以「发完 prompt 立刻关 stdin」的客户端仍然拿得到答案（这里踩过一个坑：run 的 context 原本挂在 server 的 context 上，stdin 一 EOF 就把运行取消了，于是加了 `sync.WaitGroup` 单独跟踪在跑的任务）。权限层在 RPC 模式下**不弹窗**——stdin 归协议所有，审批提示会吃掉客户端的记录；模型侧的调用走「无人可问就拒绝」，客户端自己发的 `bash` 走 `user_bash` 拦截（那是用户自己的命令，不是模型的）。这个模式还逼出了两处真实缺陷：agent 的 `history` 和 `cfg` 原本**没有任何锁**，而 RPC 会在模型还在跑的时候读历史、换模型，于是补上了 mutex 和「配置按快照读」；`isTerminal(stdin)` 在 RPC 模式下为真会让权限提示去读协议流，于是 RPC 一律视为非交互
- **Skills（Agent Skills 标准）**（[skills/](skills/)、[frontmatter/](frontmatter/)）：模板是**用户敲命令**才用的，Skills 是**模型自己按 description 决定**要不要读的——两者一起才是 pi 完整的「自己可改」。一个 skill 是一个目录 + 一个 `SKILL.md`；**渐进披露**是这一层的核心：启动时只有 `name`/`description`/`location` 进系统提示词（XML 格式），完整指示留在磁盘上，模型觉得对上了才用 `read` 去读，所以 50 个 skill 的库代价是 50 行提示词而不是 50 份文档。发现位置有四个（项目 `.harness/skills`、项目 `.agents/skills` 及其各层父目录、用户 `~/.harness/skills`、用户 `~/.agents/skills`），**先到先得**：项目在前、用户在后，项目定义的 skill 归项目，用户的目录是项目没用到的名字的兜底。`description` **必填**——它是唯一的路由信号，没有它 skill 根本不会被加载；`name` 缺省取目录名，`disable-model-invocation: true` 让 skill 只走 `/skill:name` 不进提示词；**不合规只警告不拒绝**（超长描述、非法名字照样加载），坏掉一个文件不该拖垮整个库。两个入口：模型自己 `read` 读 `SKILL.md`，或 `/skill:name [参数]` 显式把指示塞进对话（不依赖模型选不选对）。`/skills` 列出所有 skill 和来源路径。skill 的指示**跑不了任何东西**——它就是模型的输入，模型要跑脚本仍然得走工具，走同一套权限。`frontmatter/` 是抽出来的共用解析器（`prompts/` 和 `skills/` 共用一套 YAML 语义，标量 + `|`/`>` 块标量 + 转义；集合类型拒绝而不是猜），`prompts/` 里原先那份重复实现因此删掉了
- **Prompt 模板与项目上下文**（[prompts/](prompts/)、[projcontext/](projcontext/)）：pi 的「自己可改」不止扩展 API——模板和上下文文件都是**数据**，不改一行代码就能加命令、换提示词。**Prompt 模板**：`.md` 文件变成斜杠命令（文件名即命令名），可选 frontmatter（`description` / `argument-hint`），正文按 **shell 式**语法替换参数（`$1` / `$@` / `${1:-默认}` / `${@:N:L}`），参数本身按 shell 引号规则切分（`/review "API compatibility"` 是一个参数）；替换是**单遍**的，被替换进去的内容不会再被替换（模型自己的输出变不成新的占位符）；模板注册在**和内置命令、扩展命令同一个注册表**上，`/help` 直接列出，`/prompts` 还能显示来源路径（模板不对，多半是目录找错了）。**项目上下文**：`AGENTS.md` 是项目指示，**追加**到系统提示词；`SYSTEM.md` **替换**系统提示词；`AGENTS.md` 从工作目录**向上**找（仓库根那份对所有子目录生效，最近的同名文件优先），`SYSTEM.md` 只从工作目录本身读——从父目录继承一份系统提示词太意外了。两者都是数据，跑不了任何东西
- **遥测后端 · Prometheus remote write**（[telemetrybackend/prometheus.go](telemetrybackend/prometheus.go)、[telemetrybackend/prometheus_backend.go](telemetrybackend/prometheus_backend.go)、[telemetrybackend/prompb/](telemetrybackend/prompb/)、[telemetrybackend/snappy/](telemetrybackend/snappy/)）：第二个后端，证明 `Backend` 接口**真的与协议无关**——Prometheus 存的是时间序列，没有 trace，所以 `SendTraces` 是空操作并计入 `DroppedTraces`（把 counter 当 trace 存是编造契约里没有的数据）；契约的点分名翻译成 Prometheus 的合法名（稳定，dashboard 跨 run 不失效），指标名放在 `__name__` 标签里，counter 补 `_total`、upDownCounter 降级为 gauge；直方图展开成 `_bucket`（带 `le`，计数**累加**——Prometheus 的桶装的是「所有不超过上界」的观测）/`_count`/`_sum` 三件套；载荷是 protobuf + snappy，但 harness 的 `go.mod` 仍然是空的——两个编码器手写，**上游实现只作测试里的 oracle**，逐字节比对且 oracle 在自己的 module 里编译，所以零依赖和「不会悄悄分叉」两件事同时成立；`telemetry_prometheus_endpoint` / `telemetry_prometheus_headers` 配置
- **工具生态**（[tools/render.go](tools/render.go)、[shellext/](shellext/)、[tools/mutation_queue.go](tools/mutation_queue.go)、[mcp/resources.go](mcp/resources.go)）：四件事都落在扩展点上，核心没有多一分逻辑——**工具渲染器**对齐 pi 的 `ToolRenderers` / `ToolRendererResolver`：`RenderCall` / `RenderResult` 两个钩子 + 一条按注册顺序串起来的解析器链（链尾是工具自带的渲染器，再尾是内置渲染器），所以扩展可以接管任何工具（包括不是它自己的 MCP 工具）的呈现；内置工具各有自己的渲染器，`tools.DiffLines` 提供行级 diff。**`!` shell 快捷**：`!cmd` 直接跑命令并把结果按 pi 的格式放进对话，`!!cmd` 只给你看、模型看不到；命令走同一个 bash 工具所以沙箱照样生效，跑之前发 `user_bash` 事件供扩展拦截。**文件变更队列**：按真实路径（`EvalSymlinks`）给每个文件一条队列，同一文件的改动串行、不同文件并行，`write` / `edit` 都走队列，避免并发改动互相覆盖。**MCP resources**：`resources/list`、`resources/read`、`resources/templates/list` 落地，并注册 `list_mcp_resources` / `list_mcp_resource_templates` / `read_mcp_resource` 三个工具（沿用 pi 的命名，模型认得）；不带 `server` 时覆盖所有带资源的 server，单个 server 列举失败不挡其他并进 `errors`；二进制资源只描述不下载
- **遥测后端**（[telemetrybackend/](telemetrybackend/)）：把契约的数据发到真正的 collector——OTLP over HTTP + JSON 载荷，所有 OpenTelemetry collector 都收且不需要 protobuf 依赖；`Backend` 接口只有四个方法（SendTraces / SendMetrics / Flush / Close），再加一个后端不碰契约；编码器处理了 OTLP/JSON 的两个坑（64 位整数是十进制字符串、trace/span 标识符是 hex 而非 base64——这是 protobuf JSON 映射唯一的偏离），契约里的不透明标识符被哈希成合规长度且跨请求稳定；span 批量队列 + 指标定时刷新（计数器和直方图按「名字 + 属性集」聚合，bucket 按 schema 边界分桶）；5xx 重试一次、4xx 不重试、失败批次计数但不拖慢 agent；span 和指标的**属性**都按 schema 的敏感标记脱敏（脱敏在后端做——那是数据离开进程前的最后一道闸）；`telemetry_endpoint` / `telemetry_headers` / `telemetry_interval_ms` / `telemetry_timeout_ms` 配置
- **遥测契约**（[telemetry/](telemetry/)、[telemetryext/](telemetryext/)）：对齐 `pi-telemetry` 的厂商中立契约——`Context.StartSpan` 围绕回调开 span（回调同步、恰好一次、错误原样返回，契约里没有公开的 `end()`，Go 用 `telemetry.Deferred` 在自己的 goroutine 上表达 pi 的 promise 语义）、`Metrics`/`Propagation` 作为适配器的可选能力挂在每个 context 上、`Store` 读回 trace 与 metric 行；`NoopContext` 是没有遥测时的惰性实现，`MemoryContext` 是内存参考适配器；schema 是纯数据（span / event / metric / 属性类型 / 基数 / 敏感标记），harness 自带 `pi.harness.*` 词汇表（session / request / tool / compaction + 四个 metric）；采集器是又一个扩展，挂在事件总线上，靠事件开/关 span；JSONL 导出器按 schema 脱敏；隐私策略由测试守住——prompt、工具参数、工具输出、文件路径一律不进属性，只有名字、计数、耗时、token 数成为属性；`config.json` 的 `telemetry` / `telemetry_path` 开关与 `/telemetry` 命令

- **权限与沙箱**（[perm/](perm/)、[sandbox/](sandbox/)）：权限守卫作为扩展挂在 `tool_call` 钩子上（内核仍无内置权限，遵循 pi 的设计）；规则按「内置 deny 底线 → 首个命中即决定」判定，来源顺序为 config.json → 用户级 → 项目级 → 内置默认；allow/ask/deny 三态，ask 在无交互会话时默认拒绝（fail-safe）；`permissions.json` 用户级/项目级配置、`/permissions` 查看与 `/permissions allow <path>`；沙箱用 macOS Seatbelt 限制 shell（写白名单 + 读黑名单 + 网络开关），不可用时明确标注未受控
- **MCP 工具接入**（[mcp/](mcp/)）：Model Context Protocol 客户端——stdio 子进程与 streamable HTTP 两种传输、`initialize` 握手与能力协商、`tools/list` 分页发现、`tools/call` 调用、工具以 `mcp__<server>__<tool>` 命名桥接进 `tools.Registry`；`mcp.json` 配置读取与用户级/项目级合并、`-mcp-list` 与 `/mcp` 命令；一个 server 失败不影响其他
- **会话树与分叉**（[session/tree.go](session/tree.go)、[session/fork.go](session/fork.go)）：会话文件升级为树格式（entry 带 `id`/`parentId`），`/tree` 列出并切换分支（模型上下文随分支切换），`/fork` 从指定用户消息分叉出独立会话文件、`/clone` 复制当前分支；当前位置（`leaf_pointer`）持久化，重载会话能回到正在走的分支；压缩写成 `compaction` entry（摘要替代被压缩的消息，原文保留在文件里）；旧 v1 线性格式加载时自动迁移
- **扩展动态加载与热重载**（[ext/protocol.go](ext/protocol.go)、[ext/process.go](ext/process.go)、[ext/scope.go](ext/scope.go)）：进程扩展协议（stdio 上的 JSON-RPC，扩展可用任何语言写）、manifest 握手、工具桥接、事件转发、扩展发现（`~/.harness/extensions`、项目目录、`config.json` 的 `extension_dirs`）；`/reload` 按 pi 语义重载（`session_shutdown` → 撤销注册 → 重新 `Setup` → `session_start`），注册项由 harness 自动记录并撤销，扩展只需实现 `ext.Teardown` 释放子进程等资源
- **TUI 差分渲染**（[tui/](tui/)）：帧差分只重写变化的行、CSI 2026 同步输出防闪烁、ANSI 感知的显示宽度（含 CJK 宽字符）、Component 体系（Text/Container/VStack/HStack/Transcript/Editor）、主屏与备用屏两种渲染器。`-tui` 启用；模型输出流式更新单行，编辑器行不抖动
- **上下文压缩**：内置 LLM 摘要压缩器（`agent.NewSummarizer`），REPL `/compact` 直接可用；扩展仍可通过 `session_before_compact` 钩子接管（钩子优先）
- **推理透传**：OpenAI 兼容流的 `reasoning_content`、Anthropic 流的 `thinking` 块都会作为 thinking 增量流向 UI（`-show-thinking` 显示），且不会污染转录内容。**同一个 chunk 同时带 reasoning 和正文时，两个都要出来**——早期实现遇到 reasoning 就直接 return，正文被整段吞掉，于是模型的回答缺了开头几个字（`HARNESS_SKILL_HIDDEN` 变成 `KILL_HIDDEN`）；现在多出来的事件进队列，下一次 `Recv` 取出来
- **网关兼容**：Anthropic provider 同时发送 `x-api-key` 与 `Authorization: Bearer`（OpenCode Go 等网关只认后者）
- **模型注册表与多 provider 路由**：`llm.Registry` 把「模型 → 协议 → 端点」解耦，`/model` 切换模型时自动换到对应 provider（跨协议、跨网关），`/models` 列出可用模型
- **远程模型目录**：`config.json` 的 `catalog_url` 指向网关目录，启动时自动同步（`llm.Registry.SyncCatalog`），并按目录里的 `api` 字段判定每个模型实际使用的协议
- **请求级 hooks**：`llm.Hooks` 提供 `BeforeHeaders` / `BeforeRequest` / `AfterResponse`，扩展可改写 HTTP 头、请求体并观察响应（对齐 pi 的 `before_provider_headers` 等）
- **Gemini 协议**：新增 `llm.GeminiProvider`（generateContent / streamGenerateContent，支持工具调用）

## License

MIT
