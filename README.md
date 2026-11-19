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
  agent.go              Run / 队列 / settle 边界 / 工具循环
  retry.go              请求级重试：错误分类 + 指数退避
agentprofile/         Agent 环境描述：模型 / 提示词 / 工具 / 插件 / MCP / Skills / 压缩策略
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
gitkit/               内置 Git 与逐步回退
  git.go                非交互 Git 执行、worktree 定位、环境隔离
  git_tool.go           模型直接调用的 git 工具（参数数组 + stdin）
  checkpoint.go         工作树 / 精确 index / HEAD / refs / stash 检查点
  checkpoint_tool.go    模型侧 checkpoint 工具（create / list / restore）
orchestrator/         委派子代理：并发、深度、超时、取消、持久化任务记录
ext/                  扩展系统：事件总线 + Extension API + 加载器         ← pi.ExtensionAPI
  ext.go                事件总线、Extension API（hooks / 工具 / 命令 / 渲染器）
  loader.go             Manager：加载、卸载、热重载  scope.go              注册作用域：记录扩展注册了什么，重载时自动撤销
  protocol.go           进程扩展的 JSON-RPC 协议（stdio）
  process.go            进程扩展客户端：握手、工具桥接、事件转发
  supervisor.go         进程级重启：生命周期上限、指数退避和健康状态
plugins/              目录插件：plugin.json、签名 registry、依赖解析、安全安装、锁文件校验、旧插件兼容
envsubst/             严格 ${NAME} / ${NAME:-默认} 展开，未定义变量直接报错
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
contextmgr/           上下文预算与自动压缩：估算 / 摘要 / 工件化 / 硬裁剪
  estimator.go         可替换的 token 估算器（默认保守启发式）
  summarizer.go        用当前模型生成“上下文胶囊”
  artifact.go          超大工具输出的内容寻址存储
  contextmgr.go        预算、分段摘要、递归合并与安全兜底
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
| （pi 上下文压缩）            | `contextmgr`                 | 请求级预算、自动摘要、工件化 |
| Extension API (`pi.on` 等)  | `ext`                        | hooks、注册工具/命令 |
| `pi-tui`                    | `tui`                        | 差分渲染终端 UI |
| `pi-telemetry`              | `telemetry` + `telemetryext` | 厂商中立遥测契约 + 采集扩展 |
| （pi 无内置后端）           | `telemetrybackend`           | OTLP/HTTP + Prometheus 后端（可插拔） |
| `pi-coding-agent`           | `cmd/harness`                | CLI |
| （pi 无内置权限）           | `perm` + `sandbox`           | 权限守卫（扩展）+ 沙箱执行 |
| `pi-coding-agent` 工具渲染器 | `tools/render.go`            | RenderCall / RenderResult + 解析器链 |
| `pi-coding-agent` `!` 快捷  | `shellext`                   | 用户直接跑 shell 命令 |
| `pi-coding-agent` 文件变更队列 | `tools/mutation_queue.go`  | 同一文件的改动串行化 |
| （pi 无内置 Git 检查点）     | `gitkit`                     | 内置 Git 工具 + 工作区逐步回退 |
| （pi 无内置委派编排）        | `orchestrator`               | 隔离子代理、独立上下文、持久化任务 |
| `pi-coding-agent` Prompt 模板 | `prompts`                   | markdown 文件变成斜杠命令 + 参数替换 |
| `pi-coding-agent` AGENTS.md  | `projcontext`               | 项目指示 + SYSTEM.md 替换系统提示词 |
| `pi-coding-agent` Skills   | `skills`                    | SKILL.md 技能库 + 渐进披露 + /skill:name |
| `pi-coding-agent` RPC 模式 | `rpc`                       | JSONL 命令/响应/事件，被别的程序驱动 |
| `pi-coding-agent` JSON 模式 | `jsonout`                  | 一次性运行的完整事件流（--mode json） |

### 与 pi 的设计差异

模块对应关系说"哪里像"，这一节说"哪里不一样"——每一条都是刻意的选择，不是漏做。

**1. 裁决是改字段，不是返回值。** pi 的 handler **返回**裁决：`return { block: true,
reason: "..." }`，`terminate` 还能让 agent 提前收工；`session_before_*` 则返回
`{ cancel: true }`。auto-harness 的 handler **改事件**：`e.Block` / `e.BlockReason` /
`e.Cancel`。区别在于 pi 的返回值天然是「短路」的——一个 handler 回了 `block`，后面的
handler 根本不跑（`if (result.block) return result`）；auto-harness 的事件总线是广播，
所有 handler 都会跑，改字段的结果按顺序叠加。代价是 auto-harness 少一个 `terminate`
语义（工具批次提前收工），换来的是 Go 里不用为每个事件定义一套结果类型——一个
`Event` 结构体带 `Block` / `Cancel` / `Continue` 三个裁决字段，够所有事件用。

**2. 进程扩展是 auto-harness 独有的。** pi 的扩展只能是 TypeScript——`pi.on(...)` 是
一个函数调用，扩展进程活在 harness 进程里，共享内存，没有跨进程这回事。auto-harness
多了一层：扩展可以是**独立子进程**，任何语言写，走 stdio 上的 JSON-RPC。这让扩展能
用 Python 写、能在容器里跑、能崩了不影响 harness。

**3. 反向通道（`event.consult`）是两边都要解决的问题，但解法不同。** 进程内扩展和
pi 一样，改字段就行；**跨进程就没法改字段了**——harness 得把事件送出去、等回答、
再把裁决拿回来。这条通道在 pi 里不存在（因为不需要），在 auto-harness 里是必需的。

具体做法是问一次、答一次（request/response），不是订阅：harness 把要问的事件发过去，
扩展答 `{"block":true,"reason":"..."}`。

| | pi（进程内扩展） | auto-harness（进程扩展） |
|---|---|---|
| 提问方式 | handler 直接收事件对象 | JSON-RPC 请求 `event.consult` |
| 裁决载体 | 返回值 `{block, reason, terminate}` | 回答体 `{block, reason}` / `{cancel}` / `{text}`（input） |
| 谁被问 | 所有注册了这个事件的扩展 | manifest 里声明了 `consults` 的扩展 |
| 答不上来 | 抛异常 → **拦下**（fail-closed） | 不回答/报错/超时 → **拦下**（fail-closed）；`input` 例外，保持原样 |
| 扩展卡住 | 挂住整个 harness | 5 秒超时，拦下 |

失败策略两边一致，都是 **fail-closed**：pi 文档原话是 "gate handlers should fail closed
when their safety decision cannot be made"，`tool_call` 出错就拦工具，`user_bash` 出错
就拦命令。auto-harness 沿用这个策略，但多了两层保障——**等待有上限**（pi 的进程内
handler 卡住会把 harness 一起卡住，这里 5 秒就放手）和**没声明就不问**（扩展要在
manifest 里写 `"consults": ["tool_call"]` 才会被问到；没声明的扩展它的沉默不是拒绝，
不然通道出现之前写的扩展会把每一次工具调用都弄挂）。唯一的例外是 `input`：它是改写
不是门，算不出来就用用户的原话，否则一个坏扩展会吃掉每条消息。

**4. 被否决的调用不再问用户。** pi 的 `tool_call` 和权限层是两条独立的路，扩展否决
之后权限层照样问用户。auto-harness 让权限层看到 `e.Block` 就跳过——用户不会为一
个根本不会执行的调用做决定。

**5. pi 的 `user_bash` 是"接管"，auto-harness 是"否决"。** pi 的 `user_bash` handler
返回 `{ operations }`（换执行后端，比如走 SSH）或 `{ result }`（整个替换执行结果），
**没有单独的 block 标志**——想拒绝执行，扩展得自己返回一个 `result`（比如
`{ output: "denied", exitCode: 1 }`）。auto-harness 的 `user_bash` 直接有
`e.Block` / `e.BlockReason`，拒绝是一条命令的事，不用伪造执行结果。

**6. 四个可取消的会话事件都做了，语义是「动手之前问」。** `session_before_switch`
（RPC 的 `new_session`）、`session_before_fork`（`/fork`、`/clone` 和 RPC 的 fork）、
`session_before_tree`（`/tree` 切换分支）和 `session_before_compact` 都在**写任何
东西之前**发事件：`Cancel` 一出，操作就不发生，会话保持原样——不会留下写了一半的
分叉文件，叶子也不会移动，命令只打一行 `cancelled: <原因>` 就回到提示符。这和
pi 的问题一样是"切换被拒之后 UI 怎么办、分叉失败怎么收拾"，答案是把问的时间点放在
副作用之前。差别在载荷：pi 的 `session_before_tree` 还能让扩展提供自定义分支摘要，
auto-harness 的 `/tree` 不做摘要，所以只支持取消；pi 的 fork 分 `position: before/at`，
auto-harness 的 `/fork` 和 `/clone` 都从某个 entry 开始，所以用 `reason: fork/clone`
区分命令，`session_before_switch` 目前只有 `reason: "new"`（没有切换会话的命令）。

**7. 事件集：auto-harness 多两个，pi 多的是十五个。** auto-harness 有 pi 没有的
`request_start`（一次模型请求的事实：provider / model / 工具数，用来测量单次请求而
不改 provider）和 `retry`（失败请求即将重试：原因、第几次、退避多久）。pi 多出来的
15 个大致分三类：
**provider 挂钩**（`before_provider_headers` / `before_provider_request` /
`after_provider_response` / `provider_stream_event`）、**UI 与选择**
（`ui_prompt_start` / `ui_prompt_end` / `model_select` / `thinking_level_select` /
`cache_warming_decision`）、**生命周期补集**（`session_compact_failed` /
`session_tree` / `session_info_changed` / `context_with_system` /
`mcp_servers_change` / `resources_discover`）。
生命周期那类里 pi 有的 `agent_settled` / `agent_before_settle`，auto-harness 也补齐了
（见下面「重试、排队与 settle 边界」）。

auto-harness 的 provider 挂钩不在事件总线上，而在 `llm.Hooks`（`BeforeHeaders` /
`BeforeRequest` / `AfterResponse`）——它们服务的是 HTTP 层（改头、改请求体、读响应），
事件总线上那份是给扩展看的生命周期，混在一起反而两种都难用。

**8. `input` 事件的时机不同：pi 在 skill 展开之前，auto-harness 在之后。**
两边要解决的问题一样，裁决的写法不同：pi 的 handler 返回
`{action:"continue"|"transform"|"handled"}`，auto-harness 沿用"改事件字段"——
改 `e.Text` 就是 transform，置 `e.Cancel` 就是 handled。auto-harness 在
`Agent.Run` 开头发这个事件，所有入口（REPL、一次性、JSON、RPC、skill、模板）都从
那里过，所以没有哪条路径会漏掉它；代价是扩展看到的是**最终要发给模型的 prompt**，
不是用户敲的那一行——`/skill:foo` 在 auto-harness 里就是一个命令，input 事件看到
的是它展开后的全文（pi 看得到 `/skill:foo`，因为 pi 先查扩展命令、再发 input、最后
才展开 skill）。pi 的 `images` 和 `streamingBehavior` 字段 auto-harness 也没有：
它没有图片输入，也没有"边流式边发"的输入模型。

这个事件和 `before_agent_start` 有重叠，区别是三处：input 先发；载荷是一行文本而
不是 `llm.Message`；以及进程扩展**能**被问到 input（`before_agent_start` 不在被问
之列）。

**9. 项目信任：顺序照抄 pi，但决定者只能是进程内扩展。** pi 的 `project_trust` 由
用户级/全局/CLI 扩展处理（项目自己的扩展这时还没加载），返回
`{trusted:"yes"|"no"|"undecided", remember}`；auto-harness 照抄判定顺序（扩展 →
`trust.json` → `trust_project` 配置 → 问用户）和 `remember` 语义，但**进程扩展
（Python 之类）不参与**：它们在信任范围内先加载了，consult 通道本来可以问，但信任
决定要发生在项目资源装配之前，这条路上只开放给进程内扩展。另一个差别是
`defaultProjectTrust` 这个三态开关 auto-harness 没有——`trust_project: true` 就是
"总是信任"，没配置时交互式问、非交互式拒绝。信任覆盖的范围比 pi 文档点名的
（`.pi` 和 `.agents/skills`）更明确：项目级扩展/插件清单、agent profile、MCP
server、权限规则、prompt 模板、skill 都算，`AGENTS.md` / `SYSTEM.md` 不算（它们是
给模型的指示，不会自己运行）。

**10. 工具并行：pi 默认全并行、由工具声明"必须串行"；auto-harness 默认串行、由工具
声明"只读"。** pi 的 `executeToolCalls` 在 `toolExecution` 不是 `sequential` 且没有哪个
调用来自 `executionMode: "sequential"` 的工具时，把整批调用一起 `Promise.all`——并行是
默认，串行是工具的声明。auto-harness 反过来：只有**连续的** `ReadOnly` 调用并行，其余
一律串行，未注册的工具按"有副作用"处理。差别在于默认值朝哪边错：pi 的默认要求每个
工具作者判断自己是否安全并发，漏写就是并发；auto-harness 的默认要求工具作者声明自己
只读，漏写就是串行。代价是 auto-harness 少一点吞吐（一个写把前后两段读隔开），换的是
一个没声明的新工具不会突然和别的调用竞争同一个文件。另外 pi 有 `terminate`（一批工具
里某个调用可以让剩下的不跑），auto-harness 没有——见第 1 条。

**11. 换 provider：pi 换的是"整条 provider"，auto-harness 换的是"线上协议"。**
pi 的 `registerProvider(name, config)` 一次接管一个 provider 名：baseUrl、apiKey、模型
列表、自定义 `streamSimple`、OAuth 登录流程、`refreshModels` 全在一起，还能覆盖内置
provider 的 baseUrl 做代理。auto-harness 的 `RegisterProtocol(name, build)` 更窄：只注册
"怎么建一个 `llm.Provider`"，模型和 provider 条目分开注册（`RegisterModel` /
`RegisterProvider`），没有 OAuth、没有远程刷新模型列表、也不能覆盖内置协议名（重名
直接拒绝）。窄是刻意的——harness 的登录和凭据是 config 和环境的职责，不该在扩展
生命周期里重写；换来的是一条清楚的边界：扩展提供**协议实现**，身份、凭据和选择仍是
内核的。

同理，pi 用 `context` / `context_with_system` 两个事件让扩展改每次请求的消息（前者不含
system，后者连 system 和工具声明一起交给 handler），auto-harness 用
`UseContextPolicy` 包 builder：不是"改这一份 messages"，而是"替换生成这份 messages 的
那层"。差别是 pi 的 handler 每次请求跑一遍、看不到预算和压缩，auto-harness 的策略在
预算和压缩的外面，可以决定要不要压缩、压完再改。

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

# 工作区回退：默认在每次修改型工具前建立 Git 检查点
./bin/harness -checkpoint-limit 200
./bin/harness -no-git-checkpoints          # 只关闭自动快照，手动回退仍可用

# 委派子代理（默认启用；-no-subagents 关闭 agent 工具和后台任务）
./bin/harness -no-subagents

# RPC 模式：stdin 收 JSON 命令，stdout 吐 JSON 记录，被别的程序驱动
./bin/harness -rpc

# Agent 环境 profile：选择模型、工具面、插件、MCP、Skills 和压缩策略
./bin/harness -profile coding
./bin/harness -profile ./examples/agent-profiles/coding.json
./bin/harness -list-profiles

# 管理插件：签名 registry → 依赖解析 → SHA-256 校验 → 原子安装 → lockfile
./bin/harness -plugin-trust acme=./publisher.pub
./bin/harness -plugin-trust-list
./bin/harness -plugin-index ./registry.json -plugin-install pdf-tools@^1.2.0
./bin/harness -plugin-list
./bin/harness -plugin-verify

# 会话管理
./bin/harness -list-sessions
./bin/harness -session 20261111-153000.123456789   # 恢复会话
```

REPL 内置命令：`/help` `/tools` `/profile` `/plugins` `/model <id>` `/models` `/sessions` `/context` `/compact` `/checkpoints` `/rewind` `/undo` `/quiet` `/extensions` `/reload` `/tree` `/fork` `/clone` `/mcp` `/telemetry` `/prompts` `/skills` `/exit`。

`.harness/prompts/` 里的模板和 skill 也注册成斜杠命令，和上面这些并列出现在 `/help` 里。

REPL 里以 `!` 开头的行是 shell 快捷：`!cmd` 跑命令并把结果放进对话，`!!cmd` 只给你看、
模型看不到。

### Agent 环境 profile（[agentprofile/](agentprofile/)、[docs/agent-profiles.md](docs/agent-profiles.md)）

profile 把「一个环境需要什么」变成一份可检查的 JSON，而不是散落在启动参数、配置文件和
代码分支里。它可以在一次装配中同时选择模型、系统提示词、工具白名单、进程插件、MCP
server、Skills、Prompt 模板和上下文压缩参数：

```json
{
  "schema_version": 1,
  "name": "coding",
  "model": "claude-sonnet-5-5",
  "max_turns": 40,
  "tools": ["bash", "read", "write", "edit"],
  "plugins": ["pdf-tools"],
  "mcp_servers": ["docs"],
  "skills": ["release-notes"],
  "context": {"compact_at": 0.7, "keep_recent_messages": 6},
  "env": {"PDF_CACHE_DIR": "${HOME}/.cache/pdf-tools"}
}
```

选择字段有一个重要区别：**字段省略/null = 正常发现全部；`[]` = 明确一个都不要；
`["x"]` = 只要这些**。所以同一个内核可以装配完整 coding agent、只有 shell 的运维
agent，或者完全不暴露工具的内容 agent。`tools` 在插件和 MCP 都注册完之后最后应用，
因此一个白名单可以同时引用内置工具、插件工具和 `mcp__server__tool`；引用不存在的工具
会启动失败，而不是静默少一个能力。

profile 不绕过信任和权限：项目 profile 只有在项目已被持久信任（或
`trust_project: true`）时才可读取；首次交互信任判定发生在模型装配之后，所以先信任项目、
下一次再选它的 profile；完整信任判定之后还会再次检查，扩展否决项目时就中止，不装配
项目选中的插件/MCP/Skill/Prompt。显式路径 `-profile ./x.json` 是用户自己的选择，不受此限制。
切换 provider 时，`OPENAI_*` / `ANTHROPIC_*` 的 key 和 base URL 按最终 provider 解析，
不会把 profile 应用前的旧 provider 凭据带过去；config 里显式写的值和 CLI 参数仍然优先。
`/profile` 查看当前装配，`/plugins` 查看插件来源、版本、加载和进程监督状态，RPC 提供
`get_profile` / `get_plugins`。完整 schema、插件清单和能力边界见
[docs/agent-profiles.md](docs/agent-profiles.md)。

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

### 上下文治理与自动压缩

上下文治理是运行时内核的一部分，不依赖扩展。每次请求 Provider 之前，`contextmgr`
都会重新估算一次完整请求（系统提示词 + 工具 schema + 消息 + 工具调用参数）：

```
可用窗口 = 模型上下文窗口 - 本次最大输出 - tokenizer 安全余量
压缩阈值 = 可用窗口 × 75%
```

低于阈值时请求原样发出；超过阈值时按以下顺序处理：

1. **摘要旧消息**：旧消息按“消息单元”分块，每块交给当前模型生成上下文胶囊，再递归
   合并。摘要必须保留用户目标、约束、决策、文件路径、命令与结果、错误、当前状态和
   下一步。
2. **保留近期单元**：默认至少保留最近 8 个消息单元，并受 `keep_recent_tokens`
   约束；最新一条用户消息永远保留。
3. **不拆工具调用**：assistant 的 tool calls 和其后的 tool results 是一个不可分割
   单元，压缩不会留下悬空的工具结果。
4. **大输出工件化**：超过阈值的工具输出写入会话 artifact 目录，prompt 只留下绝对
   路径和预览；模型需要细节时可以再用 `read` 读回原文件。
5. **确定性硬裁剪**：摘要器不可用或仍超预算时，从旧到新丢弃完整单元，最后才截断
   单条消息，并返回 warning。硬裁剪不会伪造摘要。

自动压缩成功后，摘要写进当前会话分支的 `compaction` entry。重载会话时得到“摘要 +
保留消息”，不会把已压缩的原文重新塞回上下文；原始 entry 仍保留在 JSONL 文件里。
如果 `context` 事件 hook 改写了本次请求，压缩只影响这一次请求，不写回会话，避免把
扩展注入的临时内容持久化。`/model` 切换后，窗口和输出预留会按新模型的目录元数据
重新计算；显式配置的 `context.window` 和 `max_tokens` 优先。

`/context` 显示当前窗口、prompt 预算、估算占用、压缩阈值、artifact 目录和本进程最近
一次自动压缩；RPC 客户端用 `get_context` 读取同一份数据。诊断接口刻意不返回摘要
正文：它只报告“还剩多少空间”，不复制对话内容。

**隐私边界**：artifact 和会话文件都可能包含原始工具输出（例如构建日志、文件内容）。
默认 artifact 目录是 `<session_dir>/artifacts/<session-id>/`，目录权限 `0700`、文件
权限 `0600`。它们和会话 JSONL 一样属于本地运行数据，不应提交到仓库或上传到不受信
位置；定期清理由使用者负责。摘要和 artifact 都是数据，不会执行任何内容。

**已知边界**：默认估算器是保守启发式（ASCII 约 4 字符/token，非 ASCII 约 1
rune/token，再乘 1.10），不是各厂商的精确 tokenizer；需要精确计费时可以实现并注入
自己的 `contextmgr.Estimator`。当前消息模型只有文本，没有图片/音频 token 估算。系统
提示词和工具 schema 本身超过预算时无法被摘要，只能告警并尽可能裁剪消息。

接口、预算公式、失败路径和测试矩阵见
[docs/context-governance.md](docs/context-governance.md)。

### 委派子代理与独立上下文

`orchestrator` 把“并行做一件相对独立的工作”变成内核能力，而不是让父模型自己
开 shell 假装并行。父模型通过 `agent` 工具 `spawn` 一个任务；每个任务有独立的
session、`contextmgr`、artifact 目录和工作区绑定工具。父上下文只收到一个有界
最终摘要，子代理的完整转录留在自己的会话文件中。

默认 `isolation=auto`：Git 仓库内创建 detached worktree，工作区外没有仓库时
回退到 shared。worktree 从 `HEAD` 创建，**不会复制父工作区未提交的改动**；
成功后保留，便于人工检查或合入，不会自动 merge、丢弃或删除。`worktree` 强制
要求仓库，`shared` 是显式的不隔离模式。

子代理共享父进程的扩展/权限/遥测事件总线，但事件带
`delegation.agentId`、`delegation.parentAgentId` 和 `delegation.workspace`
元数据；父 REPL、RPC 和 JSON 流不会把子代理事件误当成主对话。权限守卫按
子代理工作区解析相对路径；需要交互确认但未预先配置 allow 的调用会
fail-closed，不会抢父终端。

```text
/agents
/agents show <id>
/agents wait <id> [seconds]
/agents cancel <id>
```

RPC 使用 `get_agents`、`get_agent`、`cancel_agent`。任务状态为
`queued/running/completed/failed/cancelled/interrupted`。重启只恢复历史记录并把
未完成任务标记为 `interrupted`，不会自动续跑未完成的运行。

完整语义、隔离边界、权限规则和生命周期见
[docs/subagents.md](docs/subagents.md)。


### 重试、排队与 settle 边界（[agent/retry.go](agent/retry.go)）

一次 Run 的边界比「一次补全」长得多：模型可能在跑的过程中被你插一句话，网络可能抖
一下，扩展可能想在收尾前再补一件事。这三件事各自有独立的机制，共用一条时间线：

```text
input → before_agent_start → agent_start → turns → agent_end
   → 队列非空？            → 再跑一次（agent_start 又发一遍）
   → agent_before_settle   → 有 Continue / 注入消息？→ 再跑一次
   → agent_settled         ← 这才是「真的空闲了」
```

**重试**只针对一次模型请求。判定照抄 pi 的思路，但用一张正则表而不是错误类型：
**不可重试的判定优先**——额度耗尽、账单、鉴权失败、模型不存在、上下文超长这些
再试一次也是同样的结果；其余命中限流（429 / overloaded）、服务端故障（5xx）、传输
中断（ECONNRESET / 裸 `EOF` / unexpected EOF / 流被打断）的才重试——裸 `EOF` 是网关
在响应之前把连接掐了，`net/http` 报成 `Post "…": EOF`，和正常结束流不是一回事（正常
结束在 `streamOnce` 里是 `io.EOF`，算成功，到不了分类器）。退避是
`base * 2^(attempt-1)` 并封顶（默认 1s 起、60s 封顶），`max_retries` 为负即关闭。
一个关键约束：**只有一段内容都没流出去时才重试**。一旦 delta 已经开始进转录，第二
次尝试会把同一句话打印两遍，所以中途断流直接报错而不是重试。每次尝试都会发
`request_start`，重试前发 `retry` 事件（原因、第几次、退避毫秒数），所以观测者既
能算出总耗时也能显示「正在重试」。

**排队**解决的是「模型还在干活，你又敲了一句话」。这句话不会被拒绝，也不会打断当前
轮：它进队列，由正在跑的这次 Run 在**安全的位置**取走——`turn_end` 之后（助手消息
和工具结果都已落进历史，绝不会插在工具调用和它的结果之间），以及每次底层 run 结束
之后。`Enqueue` 永远不因为「没有正在跑」而报错，所以「RPC 已接受这次运行」和「运行
真的开始」之间那个小窗口不会丢输入；`Run` 入口也会先清一次队列。REPL 和 TUI 因此
把 `Run` 放到后台 goroutine，主循环继续读行：跑着的时候敲的行显示 `[queued: …]`
并入队，斜杠命令和 `!` 快捷照常可用（它们不碰模型）。RPC 的 `prompt` 同理，响应里
的 `disposition` 是 `started` 还是 `queued`。

**settle 边界**是扩展在自动工作结束前的最后决策点：`agent_before_settle` 的 handler
可以置 `e.Continue` 再跑一轮，或者往 `e.Messages` 里塞消息（塞了也会续跑）。它和
`turn_end` 的区别是层次：`turn_end` 在一轮之内，settle 在整次 run 之后。续跑有上限
（8 轮），否则一个每次都要求续跑的 handler 会让 agent 永远忙不完、提示符永远不回来。
**中止的运行不经过 settle 边界**，直接发 `agent_settled{aborted:true}`——用户按了
Ctrl+C 之后不该再有扩展把活接着干下去。

所以 `agent_end` 不是「跑完了」：它后面可能还跟着排队消息、一次重试或一次续跑。
真正可以当作空闲信号的是 `agent_settled`，这也是 RPC 客户端和 JSON 流消费者应该等
的那条记录。


### 配置

优先级：内置默认 < `~/.harness/config.json` < 环境变量 < CLI flag。

```jsonc
// ~/.harness/config.json
{
  "provider": "anthropic",          // 可省略，按模型名推断
  "model": "claude-sonnet-5-5",
  "base_url": "",                   // 自定义网关
  "max_tokens": 8192,
  "context": {                      // 请求级上下文治理（全部可省略）
    "window": 0,                    // 0 = 使用模型目录
    "reserve_tokens": 512,          // tokenizer 差异的安全余量
    "compact_at": 0.75,             // 达到 prompt 预算的多少比例开始压缩
    "keep_recent_tokens": 0,        // 0 = 自动取预算的约 1/3
    "keep_recent_messages": 8,      // 至少保留的近期消息单元
    "summary_max_tokens": 2048,     // 单次摘要完成的上限
    "summary_chunk_tokens": 16000,  // 单次摘要输入的块大小
    "artifact_threshold": 8000,     // 工具输出超过多少字节转为 artifact
    "artifact_preview": 1500,       // prompt 中保留的预览字节数
    "artifact_dir": ""              // 空 = <session_dir>/artifacts/<session-id>
  },
  "max_turns": 25,
  "max_retries": 3,                 // 一次模型请求最多重试几次（负数 = 不重试）
  "retry_base_delay_ms": 1000,      // 首次退避，之后翻倍
  "retry_max_delay_ms": 60000,      // 单次退避上限
  "subagents": {                    // 委派子代理（全部可省略）
    "max_concurrent": 4,
    "max_depth": 3,
    "timeout_seconds": 1800,
    "result_bytes": 6000
  },
  "system_prompt": "",              // 覆盖内置系统提示词
  "session_dir": "",                // 默认 ~/.harness/sessions
  "catalog_url": "",                // 远程模型目录（启动时同步）
  "extension_dirs": [               // 进程扩展目录（扫描可执行文件）
    "~/.harness/extensions"
  ],
  "extension_command": "",          // 单个进程扩展命令，如 "python3 ext.py"
  "trust_project": false,           // 总是信任项目本地资源（默认交互式问、非交互式拒绝）
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

    // 4. 新的线上协议：内核不认识的 API 形状也能像内置的一样路由
    api.RegisterProtocol("acme", func(c llm.ProviderConfig) (llm.Provider, error) {
        return NewAcmeProvider(c.BaseURL, c.APIKey), nil
    })
    api.RegisterModel(llm.Model{
        ID: "acme-1", Provider: "acme", Protocol: "acme",
        ContextWindow: 128000, MaxOutput: 8192,
    })

    // 5. 请求级上下文策略：包装内核的预算/压缩，或者整个换掉
    api.UseContextPolicy(func(next ext.ContextBuilder) ext.ContextBuilder {
        return myPolicy{next: next}
    })
    return nil
})
```

### 事件（hooks）一览

handler 签名 `func(e *ext.Event) error`，按注册顺序同步执行；出错不中断其他 handler（错误被收集返回）。可变字段就地修改（pi 同款语义）。

委派子代理的事件走同一总线，但元数据放在 `e.Data`，避免扩展核心事件结构：

```go
agentID := ext.AgentID(e)          // 空 = 主代理
parentID := ext.ParentAgentID(e)
workspace := ext.Workspace(e)      // 子代理工作区
ext.SetDelegation(e, id, parent, workspace)
```

原始键是 `delegation.agentId`、`delegation.parentAgentId` 和
`delegation.workspace`。进程扩展一样能在 `EventParams.Data` / `ConsultParams.Data`
里看到它们。

| 事件 | 时机 | 可变/可取消 |
|------|------|------------|
| `session_start` / `session_shutdown` | 会话开始/结束 | — |
| `before_agent_start` | 每次 Run 开始前 | 改 `e.Message`、`e.SystemPrompt`；`e.Cancel` 取消 |
| `agent_start` / `agent_end` | 一次底层 run 开始/结束 | `agent_end` 携带 `e.Usage`；**不是**空闲信号，后面可能还有排队消息、重试或续跑 |
| `agent_before_settle` | 自动工作全部做完之前的最后决策点 | `e.Continue` 再跑一轮；`e.Messages` 注入消息（注入也会续跑） |
| `agent_settled` | 真的空闲了（队列空、续跑耗尽） | 只读：`e.Aborted` 说明是中止而不是跑完；**这才是空闲信号** |
| `retry` | 失败的模型请求即将重试 | 只读：`e.Reason`、`e.Data["attempt"]` / `"maxAttempts"` / `"delayMs"` |
| `turn_start` / `turn_end` | 每轮补全前后 | `turn_end`：`e.Continue` 强制续跑、`e.Messages` 注入消息 |
| `request_start` | 一次模型请求开始 | 只读：携带 `e.Request`（provider / model / 工具数），便于测量单次请求 |
| `message_start` / `message_update` / `message_end` | 助手消息流式产出 | `message_end` 可替换 `e.Message` |
| `tool_call` | 工具执行**之前** | 改 `e.ToolCall`（改参数）、`e.Block` + `e.BlockReason` 拦截 |
| `tool_execution_start` / `tool_execution_update` / `tool_result` / `tool_execution_end` | 工具执行生命周期 | — |
| `context` | 发给 Provider 前 | 替换 `e.Messages`（上下文改写） |
| `session_before_compact` / `session_compact` | 压缩历史 | 提供 `e.Messages` 扮演压缩器；`e.Cancel` 放弃这次压缩 |
| `session_before_switch` / `session_before_fork` / `session_before_tree` | 换会话、分叉、`/tree` 导航**之前** | `e.Cancel` 取消，操作不发生；携带 `e.Reason`（`new` / `fork` / `clone`）、`e.EntryID`（分叉点或树目标，当前分支在 `e.Data["from"]`） |
| `project_trust` | 项目资源加载之前 | `e.Trusted` = `"yes"` / `"no"` 定下决定（留空交给内置流程）；`e.Remember` 写进 `trust.json`；`e.Cwd` 是项目目录 |
| `input` | 每次 Run 开始、prompt 交给模型之前 | 改 `e.Text` 改写；`e.Cancel` 表示扩展已处理这行，不跑 |
| `user_bash` | `!` 快捷执行 shell 命令前 | `e.Block` + `e.BlockReason` 拦截；携带 `e.BashCommand` 和 `e.ExcludeFromContext` |

### 可替换的内核层

扩展不只是"加东西"：内核里有三处现在可以整体替换，而不必改内核代码。

| 层 | 注册方法 | 替换的是什么 |
|----|---------|-------------|
| 线上协议 | `api.RegisterProtocol(name, build)` + `api.RegisterModel` | `llm.Registry` 不认识一种 API 形状时，扩展注册它，模型切换就路由过去 |
| 上下文策略 | `api.UseContextPolicy(build)` | 内核的预算/自动压缩/artifact 外置是最内层，策略包在它外面；策略可以改写消息、改结果，也可以完全忽略 `next` 自己来 |
| 工具呈现 | `api.RegisterToolRenderer(resolver)` | 任何工具（包括别人的 MCP 工具）怎么显示 |

这三层都要求返回 Go 值（`llm.Provider`、builder、渲染器），所以只有进程内扩展能注册；
进程扩展跨进程拿不到它们，它能提供的可替换点是工具、命令、hook 和事件。

上下文策略的语义值得说清楚：`UseContextPolicy` 收到的 `next` 是**它下面所有层**合成
出来的 builder，所以一个策略可以选择"先做我的事再交给内核"（`next.Build`）、"只看一眼
结果"（先 `next.Build` 再改返回值），或者"完全接管"（根本不调 `next`）。多个策略按
注册顺序从外到内，先注册的先看到请求。热重载时注册会被撤销，所以 `/reload` 不会让
策略叠两层。

工具并行：一次助手消息里的**连续只读工具调用**会并行执行（上限 4 个），有副作用的
调用保持串行且与前后顺序不变——一次写和它后面的读必须看到写的效果。结果按调用顺序
回填，不按完成顺序，因为工具消息回答的是具体的 call id。只读与否由工具的
`ReadOnly` 声明决定，未知工具按"有副作用"处理——MCP 工具和进程扩展的工具都不算只读，
哪怕服务器在 `annotations.readOnlyHint` 里自称如此：这个标记还决定"改动前要不要存
工作区快照"，不能让被检查的一方自己声明。

并行也把几处"以前只有一个 goroutine 碰"的状态变成了共享：工具通知的输出、TUI 的流式
标志、扩展里的计数器都加了锁；权限层一次只问一个问题——两个并行调用同时弹确认框会
读同一个终端，第二问会把第一问的答案吃掉。

写扩展时要注意：**工具事件可能是并发到来的**。一次助手消息里的连续只读调用并行执行，
所以 `tool_call`、`tool_execution_start` / `_update` / `_end`、`tool_result` 的 handler
可能同时在跑；带状态的 handler 要自己加锁。边界事件（`agent_*`、`turn_*`、`message_*`、
`request_*`、`session_*`）和发出它们的 run 是串行的，不用管。

### 进程扩展（任何语言）

Go 没有内置脚本解释器，所以扩展也可以是**独立的子进程**：harness 通过 stdio 上的
JSON-RPC 与它通信，扩展用任何语言写都行。

协议是换行分隔的 JSON：

```
harness → 扩展: {"id":1,"method":"manifest","params":null}
扩展 → harness: {"id":1,"result":{"name":"demo","tools":[{"name":"demo_echo",...}]}}
harness → 扩展: {"id":2,"method":"tool.execute","params":{"name":"demo_echo","arguments":{...}}}
扩展 → harness: {"id":2,"result":{"content":"...","is_error":false}}
扩展 → harness: {"method":"event","params":{"event":"demo_started","text":"..."}}
```

扩展握手时返回自己的 manifest（名字 + 工具 + 命令列表），harness 把这些工具注册成
和内置工具完全一样的工具，把命令注册成斜杠命令；模型调用工具时 harness 把调用转发给
子进程，用户敲命令时把整行参数转过去：

```
harness → 扩展: {"id":4,"method":"command.execute","params":{"name":"greet","args":"there"}}
扩展 → harness: {"id":4,"result":{"text":"hello there"}}
```

命令和工具的区别是**谁在跑**：工具是模型决定调的，命令是用户自己敲的，所以命令拿到
的是用户原文，返回的文本直接打给用户，不进入对话。扩展也可以主动推送事件到 harness
的事件总线，事件名用内置的或自定义的都行。注意这是**扩展往 harness 推**的方向，和
下面 harness 主动发出的 `input` 事件是两回事。

进程扩展的能力按 manifest 声明授予，装成插件时由 `plugin.json` 的 `capabilities`
决定：`tools`（提供工具）、`commands`（提供斜杠命令）、`hooks`（被 consult 询问）、
`events`（推送事件）。没声明的能力被拒绝而不是静默忽略——一个 manifest 提供了工具却
没拿到 `tools` 授权，加载直接失败。

#### 反向通道：harness 问扩展

`event` 是扩展往 harness 推事件的方向。反方向是 harness **问**扩展：harness 在
真正动手之前问一句"这件事允许吗"，扩展可以否决；对 prompt 还能给出一段替代文本。

```
harness → 扩展: {"id":3,"method":"event.consult","params":{"event":"tool_call","toolCall":{"id":"c1","name":"bash","arguments":{"command":"rm -rf /"}}}}
扩展 → harness: {"id":3,"result":{"block":true,"reason":"不允许"}}
```

被问的事件有七个，它们是内核真正要做决定的地方：

| 事件 | 时机 | 回答的效果 |
|------|------|-----------|
| `tool_call` | 工具执行之前 | `block`：工具不执行，原因作为错误结果回到模型 |
| `user_bash` | `!` 快捷执行命令之前 | `block`：命令不执行 |
| `input` | 每次 Run 开始、prompt 交给模型之前 | `text`：替换 prompt；`cancel`：扩展自己处理了这行，不跑 |
| `session_before_compact` | 压缩历史之前 | `cancel`：放弃这次压缩 |
| `session_before_switch` | 换新会话之前（RPC `new_session`） | `cancel`：不换，会话继续 |
| `session_before_fork` | `/fork`、`/clone` 写文件之前 | `cancel`：不写分叉文件，会话不变 |
| `session_before_tree` | `/tree` 移动分支之前 | `cancel`：叶子不动 |

请求带 `toolCall`（含模型原始的 arguments）、`bashCommand`、`text`、`reason`、
`sessionId`、`entryId`、`data`，扩展拿到的是工具本身会看到的原始参数。回答是
`{"block":true,"reason":"..."}`；压缩和三个会话事件答 `{"cancel":true}`（`reason`
里的话会打印给用户），`input` 还能答 `{"text":"..."}` 来改写 prompt。

三个会话事件的问在**任何副作用之前**：取消之后不会留下写了一半的分叉文件，叶子也
不会已经移走才发现被拒——REPL 的 `/fork`、`/tree` 打一行 `cancelled: <原因>` 就回到
提示符，RPC 的 `new_session` / `fork` 回一条带原因的失败响应。

`project_trust` 不在这七个里：信任决定发生在扩展进程装配之前，只有进程内扩展能参与
（见设计差异第 9 条）。

扩展要在 manifest 里声明它想被问到哪些事件：

```json
{"name": "policy", "consults": ["tool_call", "user_bash"]}
```

两个细节：

- **答不上来就拦下，`input` 除外。** 扩展不回答、报错或者超时（5 秒），这次操作
  **不执行**——和 pi 的 gate handler 一样（pi 文档原话是 "gate handlers should fail
  closed when their safety decision cannot be made"，`tool_call` 出错就拦工具，
  `user_bash` 出错就拦命令）。一个坏掉的策略扩展不能变成一扇开着的门。
  `input` 是这条规则的例外：它是改写不是门，算不出来的改写有一个安全默认值，就是
  用户原来那句话，所以它答不上来时 prompt 保持原样——在那里 fail-closed 等于让一个
  坏扩展吃掉每一条消息。这里比 pi 多一层保障：pi 的进程内 handler 卡住会把 harness
  一起卡住，这里有 5 秒上限。
- **没声明就不问。** manifest 里没写 `consults` 的扩展根本不会被问到，所以它的沉默
  不是拒绝——不然通道出现之前写的扩展会把每一次工具调用都弄挂。

进程内扩展（Go 写的）用同一个事件总线，改 `e.Block` / `e.BlockReason`（门）或
`e.Text` / `e.Cancel`（prompt 和会话操作）就直接生效；子进程扩展则多走一次
JSON-RPC。

可执行文件放在 `~/.harness/extensions/`、`<项目>/.harness/extensions/` 或
`config.json` 的 `extension_dirs` 里（目录内的可执行文件都会被发现）。完整示例见
[examples/extensions/demo_extension.py](examples/extensions/demo_extension.py)：

```bash
chmod +x examples/extensions/demo_extension.py
./bin/harness -model deepseek-v4.1-flash \
  -base-url https://opencode.ai/inference/go/openai/v1
# 或在 config.json 里： "extension_dirs": ["examples/extensions"]
```

目录插件不需要把入口做成裸可执行文件。它放一个 `plugin.json`，描述启动命令、固定
工作目录、环境变量和声明式能力：

```json
{
  "schema_version": 1,
  "name": "pdf-tools",
  "version": "0.1.0",
  "command": ["python3", "plugin.py"],
  "cwd": "${pluginDir}",
  "env": {"PDF_CACHE_DIR": "${PROFILE_CACHE_DIR}"},
  "capabilities": ["tools", "hooks"],
  "supervisor": {
    "restart": "on_failure",
    "max_restarts": 3,
    "backoff_ms": 250,
    "max_backoff_ms": 30000
  }
}
```

清单不是第二套工具注册表：进程启动后的 `manifest` 响应仍是工具名、参数 schema 和
consult 事件的唯一权威。`plugin.json` 负责稳定身份、启动和授权：未知字段/坏 schema
拒绝；`cwd` 与路径型入口在 symlink 解析后仍必须留在插件目录内；`capabilities` 是
显式授权（`tools` / `commands` / `hooks` / `events`），比如只给了 `tools` 的插件不能
注册 consult hook、manifest 里给了命令会直接加载失败，推送的事件也会被丢弃。旧的
可执行文件发现方式继续兼容。profile 的 `plugins`
按名字选择插件，未列出的插件不启动；`/plugins` 显示版本、来源和状态。

`supervisor` 是可选**进程级恢复**，不是任务 checkpoint：子进程崩溃后从头启动，
内存状态和未完成调用不会续上。`restart` 支持 `never`、`on_failure`、`always`；
`max_restarts` 省略时默认 3，表示这个扩展实例生命周期内最多成功替换几次，不是
“每崩一次给 3 次”；显式 0 表示崩一次就停。初始退避 `backoff_ms` 默认 250ms，
上限 `max_backoff_ms` 默认 30s（两个时长写 0 也取默认值），指数增长。新进程必须
重新握手，而且运行时 manifest
必须与首次批准时完全一致——不能靠重启偷偷增加工具或减少 consult 授权。重启空窗期
工具调用返回错误，策略 consult gate 全部 fail-closed，`input` 保持原样；达到上限、
manifest 漂移或握手失败后不再重启。`/plugins` 和 RPC `get_plugins` 会显示
`restartMode`、`restartCount` / `maxRestarts`、`healthy` / `restarting`、
`lastExit` 和 `lastError`。旧可执行插件没有 `plugin.json`，仍保持不自动重启。

### 插件 registry、签名与托管安装（[plugins/](plugins/)、[docs/agent-profiles.md](docs/agent-profiles.md)）

手工把目录放进 `~/.harness/extensions` 只是开发方式；可分发插件走 registry。registry
是严格 JSON，列出版本、`.tar.gz` artifact 的 SHA-256 和依赖约束。安装器用 semver
约束解析完整依赖图（必要时回溯），在替换任何目录之前完成下载、摘要校验、安全解包、
manifest 身份校验和整棵树摘要计算。`~/.harness/plugins/<name>` 是托管目录，
`plugins.lock.json` 记录版本、artifact/树摘要、来源、publisher、签名 key ID 和依赖。

发布者可给 registry 增加 `publisher` 和 Ed25519 `signature`。签名覆盖去掉
`signature` 后的规范化 JSON，并带固定协议域；插件元数据、artifact URL 和 artifact
SHA-256 都在签名范围内，所以信任一个 publisher 不会只保护一半分发链。公钥信任根
默认在 `~/.harness/plugin_publishers.json`，也可用 `HARNESS_PLUGIN_TRUST_FILE`
指定：

```bash
go run ./cmd/plugin-registry-sign -generate-key \
  -private-key-out publisher.pem \
  -public-key-out publisher.pub

go run ./cmd/plugin-registry-sign \
  -index ./registry.json \
  -publisher acme \
  -key publisher.pem \
  -out ./registry.signed.json

./bin/harness -plugin-trust acme=publisher.pub
./bin/harness -plugin-trust-list
./bin/harness -plugin-untrust acme

./bin/harness \
  -plugin-index https://plugins.example.com/index.json \
  -plugin-install pdf-tools@^1.2.0
./bin/harness -plugin-list
./bin/harness -plugin-verify
./bin/harness -plugin-remove pdf-tools
```

密钥生成不会覆盖已有文件，私钥权限为 `0600`。如果改用 OpenSSL，需要 OpenSSL 3；
macOS 自带的部分 LibreSSL 版本不支持 Ed25519。也可以把 `publisher.pub` 的 PEM
内容作为公钥输入，签名工具和信任命令都只接受 Ed25519。

`-plugin-verify` 会重算每个目录树的摘要、检查锁定的依赖版本，并报告 lockfile 之外的
手工托管插件目录。卸载会拒绝删除仍被其他托管插件依赖的包。归档拒绝绝对路径、`..`、
链接、设备文件、重复路径和超限内容；远程 registry 只接受 HTTPS，明文 HTTP 仅允许
loopback 开发服务器。远程 registry 默认必须带受信任的 publisher 签名；只有显式传
`-plugin-allow-unsigned` 才允许降级并打印警告。本地 registry 可以继续不签名，方便
离线开发；签名 registry 如果 key 不在信任根里仍会被拒绝。`-plugin-list` 显示
publisher，未签名安装显示 `unsigned`。

lockfile schema 2 会保存安装时的 `publisher` / `signer_key_id`；旧 schema 1 仍可
读取，并在下一次写入时升级。lockfile 本身没有签名，不是不可篡改的安装历史。Ed25519
签名认证的是 registry 元数据和 artifact 摘要，不是运行时沙箱；密钥吊销、过期、
anti-rollback/freshness、自动更新和回滚通道仍是后续工作。

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
| `git` | `args[]`, `stdin?`, `workdir?`, `timeout_ms?` | 非交互执行任意 Git 子命令；参数数组直接传 Git，不经过 shell；修改型调用先建检查点 |
| `checkpoint` | `action`, `label?`, `id?`, `limit?`, `reset_head?` | 列出/创建/恢复 Git 工作区检查点；默认同时恢复 HEAD 与 refs |
| `agent` | `action`, `id?`, `prompt?`, `name?`, `model?`, `isolation?`, `timeout_ms?` | spawn/send/wait/status/list/cancel 隔离子代理 |

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
- `!cmd` / RPC `bash` 与模型工具共用工作区回退链：执行前先建立 Git 检查点；
  快照失败就拒绝执行（fail-closed）。

## 内置 Git 与逐步回退

Git 仓库内默认启用工作区检查点：每次**修改型工具调用之前**先保存一份可恢复
状态。`read`、只读 Git 子命令和 MCP 资源读取不触发快照；`bash`、`write`、
`edit`、`git branch topic`、裸 `git stash` 等会触发。快照失败时工具不执行，
不会出现“改了但退不回去”的情况。

```text
/checkpoints                 # 最近的检查点，带 [current] / safety 标记
/checkpoints create before-refactor
/rewind                      # 只回退工作区：回到当前状态之前的下一个检查点
/rewind 2                    # 显式选择列表第 2 项
/undo                        # 整回合撤销：工作区 + 会话分支回到本回合开始前
/undo 2                      # 显式选择第 2 个检查点并撤销到它的回合边界
```

`/rewind` 和 `/undo` 的区别是“工作区”与“工作区 + 对话”：

- `/rewind` 只恢复 Git 工作树、index、HEAD/refs 和 stash，会话历史不变。
- `/undo` 在同样恢复工作区之后，把会话 leaf 移到该检查点绑定的
  **本轮用户消息之前**。这样不会留下一条没有 tool result 的 assistant
  tool call，模型也不会继续记得已被撤销的步骤。
- 一个回合可能有多次工具调用；`/undo` 选该回合**最早**的检查点，
  所以会回到第一次修改之前，而不是只撤销最后一次。只读回合没有工作区
  检查点时，仍然只移动会话并明确报告 `workspace unchanged`。
- 只有绑定了当前会话的检查点能整回合撤销；旧检查点没有 `sessionId` 时
  仍可用 `/rewind`。每次 `/undo` 会先建 safety 检查点，所以
  `/undo <safety-id>` 可以 redo。

每个检查点保存：非忽略工作树、**精确 index**（包括 merge 冲突 stage）、
HEAD/分支/detached 状态、本地分支/tag/远程跟踪 ref 等普通 refs，以及
stash 栈和它的 reflog。恢复前会再建一个独立 safety 检查点，所以回退本身也能
再回退。默认 `reset_head=true`；`checkpoint` 工具的 `reset_head:false`
只恢复文件与 index。跨分支恢复会被允许，但拒绝改动另一个 worktree 正在
checkout 的分支。

模型侧对应 `git` 和 `checkpoint` 两个内置工具。`git` 的 `args` 是字符串
数组，不经过 shell；支持 `stdin`（`git apply`、`commit -F -` 等）和仓库内
相对 `workdir`。所有 Git 调用都关闭 pager/editor/终端提示，必须使用非交互
形式。

边界：忽略文件、`.git/config`、hooks、`FETCH_HEAD`、除 stash 外的 reflog、
submodule 工作树、LFS 实际内容、split/sparse index、partial clone、空目录本身
和工作区之外的副作用不在恢复范围；
快照可能包含未跟踪文件里的本地密钥，`git push --mirror` / `git bundle --all`
会把 `refs/harness/*` 一起带走。完整语义、存储格式、失败回滚和测试矩阵见
[docs/git-checkpoints.md](docs/git-checkpoints.md)。

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
| 事件 | `{"type":"…","data":{…}}` | 会话活动：`agent_start` / `turn_start` / `message_update` / `tool_execution_start` / `request_start` / `retry` / `agent_end` / `agent_before_settle` / `agent_settled` …；`reason` 说明原因（`retry` 是错误文本，会话事件是 `new` / `fork` / `reload`），`retry` 另带 `attempt` / `maxAttempts` / `delayMs` |

命令清单：

| 命令 | 参数 | 作用 |
|------|------|------|
| `prompt` | `message` | 发一句话给模型。**响应在运行开始时就回**，`data.disposition` 是 `started`，答案走事件流。**运行中再发**是 `queued`：消息进队列，由正在跑的那次运行在 `turn_end` 后取走 |
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
| `get_context` | — | 上下文窗口、prompt 预算、当前估算与最近一次自动压缩 |
| `get_entries` / `get_tree` | — | 会话树的 entry |
| `get_last_assistant_text` | — | 最后一条助手消息 |
| `set_session_name` | `name` | 给会话命名 |
| `get_commands` | — | harness 知道的斜杠命令（内置 + 模板 + skill） |
| `get_profile` | — | 当前 Agent 环境：profile 来源、选择模式、活跃工具、环境变量键（不返回值） |
| `get_plugins` | — | 已发现插件的名称、版本、路径、是否被 profile 选择和加载状态 |
| `get_checkpoints` | — | 工作区检查点，最新在前，带 `current` / `differs` 标记 |
| `create_checkpoint` | `label?` | 手动创建一个工作区检查点 |
| `rewind_checkpoint` | `checkpoint?`, `resetHead?`, `restoreContext?` | 恢复检查点；省略 `checkpoint` 时回退到上一个更旧状态，`resetHead` 默认 `true`；`restoreContext:true` 同时把会话移回该检查点的回合边界（与 `resetHead:false` 互斥）；运行中的 prompt 结束前拒绝恢复 |
| `fork` / `clone` | `entryId` | 分叉会话 |
| `get_agents` | — | 列出委派子代理任务 |
| `get_agent` | `agentId` | 查看一个委派任务 |
| `cancel_agent` | `agentId` | 取消任务及其后代 |

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
- **`retry` 记录**带 `reason` / `attempt` / `maxAttempts` / `delayMs`，所以一次运行
  里的抖动是看得见的，而不是一段静默的等待。
- **`agent_settled` 是空闲信号**，`agent_end` 不是：`agent_end` 之后可能还有重试、
  排队消息或 `agent_before_settle` 的续跑，那些都会再发一次 `agent_start`。
  `agent_settled` 带 `"aborted": true` 时表示是被中止的，不是跑完的。

stdout 归事件流所有：模型文本、工具提示、诊断全走 stderr。退出码报告这次运行成功
没有，脚本不用解析流就知道该不该继续。

## MCP 接入

支持 [Model Context Protocol](https://modelcontextprotocol.io)：harness 连接 MCP server，
把它们的工具注册成普通工具，模型像调用内置工具一样调用它们。

**配置**：`~/.harness/mcp.json`（用户级）和 `<项目>/.harness/mcp.json`（项目级，
项目被信任时读取，见「项目信任」）——格式与其他 MCP 客户端一致：

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

当 agent profile 带 `mcp_servers` 时，上面合并出来的 catalog 会再按这个白名单过滤：
列表里的 server 才连接，`[]` 表示完全不连接 MCP，profile 点名但 catalog 里不存在的
server 会启动失败，点名且存在的 server 连接失败也会终止启动。profile 的 `env` 会继承
给 stdio MCP 子进程，单 server 自己的 `env` 覆盖同名项；HTTP headers 和 profile
环境值都不会出现在 `/profile` / RPC 输出里。
`command` / `args` / `cwd` / `env` / `url` / `headers` 里的 `${NAME}` 会按同一套严格
规则展开（profile 值优先于进程环境，未定义且没有 `${NAME:-默认}` 就报错，不连接这个
server），所以 token 不需要写进 profile 或 mcp.json。

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

## 项目信任（[cmd/harness/trust.go](cmd/harness/trust.go)）

一个克隆下来的仓库可以自带可执行扩展、`plugin.json` 目录插件、agent profile、MCP
server、权限规则、prompt 模板和 skill。这些都是项目级资源，**在信任判定之前一个都
不加载**——不然打开一个陌生仓库就等于运行它的代码。

判定顺序（对齐 pi 的 `project_trust`）：

1. **扩展**：用户级/内置扩展先加载，然后发 `project_trust` 事件。handler 把
   `e.Trusted` 设成 `"yes"` / `"no"` 就算定了，`e.Remember` 让它写进 `trust.json`。
   项目自己的扩展此时还没加载——这正是判定要排在它们前面的原因。
2. **保存过的决定**：`~/.harness/trust.json` 里该目录的 `"yes"` / `"no"`。
3. **`config.json` 的 `trust_project: true`**：对每个项目都不问、直接信任。
4. **问用户**：交互式会话列出项目会加载什么，回答 `[y/N]` 并被记住。非交互
   （`-print`、JSON、RPC，或 stdin 不是终端）没有人可问，**不加载**项目资源，并在
   stderr 打一行说明——和权限层"无人可问就拒绝"是同一条规则。

`/trust` 查看当前决定和项目资源，`/trust yes|no` 保存决定，`/trust reset` 忘掉、
下次再问。决定按**绝对工作目录**记录，保存的决定和 `trust_project` 在下次启动时
生效（资源是在启动时装配的）。

项目级 agent profile 也遵守这个顺序。它会影响模型、系统提示词和插件选择，所以只有
项目已有保存的 `"yes"` 或 `trust_project: true` 时，`-profile <name>` 才会读取
`.harness/profiles/<name>.json`。首次交互信任提示发生在模型和会话装配之后；先在没有
profile 的运行里信任项目，下一次再选它的 profile。显式文件路径不受此限制。项目 profile
的 `plugin_dirs` / `skill_dirs` / `prompt_dirs` 可以是仓外路径，信任项目前应按审查扩展
的方式检查这些声明。

项目上下文文件（`AGENTS.md` / `SYSTEM.md`）不参与信任判定：它们是给模型的指示，
不会自己运行。信任只决定"项目自己的配置要不要读"，不决定"模型能做什么"——权限层
和沙箱照常对每一次调用生效。

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
`<项目>/.harness/permissions.json`（项目级，项目被信任时读取）：

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

覆盖：文本补全、bash 工具调用闭环、文件工具读写改往返、会话持久化与恢复、请求级上下文预算与自动摘要（保留最新用户意图、tool call/result 不悬空、摘要分块合并、失败回退、会话重载一致）、上下文压缩后信息保真、推理流透传且不污染转录、工具失败后的恢复、上下文超时中止、进程扩展工具、进程扩展否决工具调用（`event.consult` 反向通道：否决、答不上来拦下、没声明就不问）、进程扩展改写 prompt（`input` 反向通道：改写后的话真的到了模型那里）、MCP 工具、MCP resources、会话分叉与分支、权限拦截与放行、遥测落盘、OTLP 投递、Prometheus remote write 投递、工具渲染器接管呈现、`!` 快捷（含 `!!` 不进上下文）、文件变更队列串行化、Prompt 模板参数替换、项目上下文文件（AGENTS.md / SYSTEM.md）、Skills（渐进披露、模型自己读 SKILL.md、`/skill:name` 显式调用、参数传递、`disable-model-invocation` 隐藏、skill 不能自己执行命令）、RPC 模式（子进程协议：prompt 出答案、状态与历史、bash 命令与退出码、未知命令不毁会话、skill 走 RPC 也生效、关掉 stdin 后仍等运行结束）、JSON 输出模式（会话头在前、事件流完整、增量拼出答案、工具调用与结果在流上、非法模式值报错）、Git 内置工具与工作区检查点（工作树/index/HEAD、跨分支与 detached HEAD、refs/tag/远程 ref、stash 栈、merge 冲突 index、忽略文件与空目录边界、跨 worktree 保护、连续回退不向前跳、失败 fail-closed）。

离线测试还覆盖 profile 的严格 schema、未知字段拒绝、环境变量展开、配置覆盖优先级、
显式空工具集、插件与 MCP 白名单、Skills/Prompt 选择、旧插件兼容、`plugin.json`
的 `cwd`/symlink 逃逸拒绝、能力授权、plugin registry 的 semver/依赖回溯、Ed25519
签名与协议域、篡改/未信任 key 拒绝、发布者信任库、远程未签名默认拒绝、归档逃逸、
SHA-256 与树摘要、升级/卸载事务、lockfile v1→v2 兼容和来源校验、进程重启的生命周期
计数与 manifest 不变性、重启空窗期的 fail-closed，以及未持久信任的项目 profile 不加载。

未设置 key 时这些测试自动跳过（`t.Skip`），所以 CI 上安全。

### CI

[.github/workflows/ci.yml](.github/workflows/ci.yml) 跑两个 job：

- **build**：`ubuntu-latest` 和 `macos-latest` 两个平台上跑 `go build ./...`、`go vet ./...`、`go test ./...`、`go test -race ./...`，外加一个 gofmt 检查。工具链跟着 `go.mod` 走（`go-version-file: go.mod`），所以 CI 用的版本就是模块声明的那个。Linux 能跑是因为终端代码按平台拆开了：`tui/term_bsd.go` 用 BSD 的 termios 布局，`tui/term_linux.go` 用 Linux 的（控制字符在前，标志位在后，顺序相反）。**tag 上也会跑**（`tags: ["v*"]`）。
- **e2e**：真实模型的端到端测试。key 从仓库 secret `OPENCODE_API_KEY` 读，**没有 key 时这一步跳过而不是失败**（`if: env.OPENCODE_API_KEY != ''`）——所以公开仓库克隆下来也不会红。模型固定 `deepseek-v4.1-flash`，和测试源码里写死的一致。

## Roadmap（深度拓展方向）

按 pi 的演进路径，预留的深化方向：

已完成的深化项：

- **可替换的内核层与并行工具调用**（[ext/ext.go](ext/ext.go)、[llm/registry.go](llm/registry.go)、[agent/agent.go](agent/agent.go)）：从「扩展能加东西」走到「扩展能换掉内核的一层」。`api.RegisterProtocol` + `api.RegisterModel` 让一种内核不认识的线上协议像内置的一样路由——注册协议名和 builder，模型声明该协议，`/model` 切过去就由扩展服务；`api.UseContextPolicy` 把内核的预算/自动压缩/artifact 外置变成最内层，扩展注册的策略包在它外面，可以改写输入、改结果或整个接管（不调 `next` 就是不压缩），多策略按注册顺序从外到内，热重载撤销注册所以 `/reload` 不会叠两层；进程扩展补上 `commands` 能力和 `command.execute` 方法，任何语言的插件都能提供斜杠命令，不再只有 Go 扩展可以。工具循环也补了并行：一次助手消息里**连续的只读调用**并行执行（上限 4），有副作用的调用保持串行且顺序不变（写和它后面的读必须看到写的效果），结果按调用顺序而不是完成顺序回填。只读由工具的 `ReadOnly` 声明决定，未知工具按有副作用处理；并行也让工具通知、TUI 流式标志和示例扩展的计数器变成多 goroutine 共享，三处都加了锁，权限层则改成一次只问一个问题。委托出去的子代理沿用父代理的上下文策略——否则一条注入项目规则或替换压缩的策略会在一层之下悄悄失效
- **委派子代理与独立上下文**（[orchestrator/](orchestrator/)、[cmd/harness/subagents.go](cmd/harness/subagents.go)、[docs/subagents.md](docs/subagents.md)）：父模型用 `agent` 工具启动后台任务，每个任务拥有独立 session、上下文预算、摘要器、artifact 目录和工作区绑定工具；父上下文只收有界摘要，完整转录留在子会话。默认在 Git 仓库内创建 detached worktree，非 Git 目录回退 shared；成功 worktree 保留，需要人工检查或合入，不会自动丢弃用户改动。`max_concurrent` / `max_depth` / 单次超时 / 取消级联 / 原子 JSON 任务记录均已落地；重启将未完成任务标记为 `interrupted` 而不假装续跑。权限层按子代理 workspace 解析路径，后台无法弹父终端，未预批准的 ask 调用 fail-closed。REPL `/agents` 和 RPC `get_agents` / `get_agent` / `cancel_agent` 提供检查与控制
- **内置 Git、逐步工作区回退与整回合撤销**（[gitkit/](gitkit/)、[docs/git-checkpoints.md](docs/git-checkpoints.md)）：Git 从“让模型跑 shell”变成内核工具。`git` 用参数数组直接执行，不经过 shell，支持 stdin、仓库内 workdir 和非交互环境；`checkpoint` 保存非忽略工作树、精确 index（含 merge 冲突 stage）、HEAD/分支/detached、普通 refs、stash 栈与 reflog。默认在每个修改型工具和用户 `!` / RPC `bash` 前自动快照，快照失败则拒绝执行；只读工具和只读 Git 子命令跳过。检查点记录所属 session、回合 ID 和会话锚点：`/rewind` 只恢复工作区，`/undo` 选择当前回合最早的快照，把工作区和会话 leaf 一起退回第一次修改之前，旧 assistant tool call 不会悬空；同一锚点下的不同分支靠回合 ID 区分，只读回合则只移动会话并明确警告。`restoreContext:true` 在 RPC 中提供同样语义，运行中的 prompt 结束前拒绝恢复与 `new_session`。恢复前先建独立 safety 检查点，所以 `/undo` 本身也能撤回，`/undo <safety-id>` 就是 redo；空选择器不会被 safety 快照带着向前跳。跨分支恢复会做 worktree 占用检查；恢复失败会尝试回滚到 safety，会话移动失败也会回滚工作区。边界明确：忽略文件、`.git/config`、hooks、`FETCH_HEAD`、非 stash reflog、submodule 工作树、未验证的 LFS 行为、进程内存、运行中的请求和工作区外副作用不在恢复范围；快照可能包含未跟踪文件里的密钥，镜像推送 `refs/harness/*` 会带走它们
- **插件 registry、依赖解析与托管安装**（[plugins/](plugins/)、[cmd/harness/plugins_cli.go](cmd/harness/plugins_cli.go)）：插件从“手工拷贝目录”升级为可审计分发单元。registry 文档声明版本、artifact SHA-256 和依赖约束；解析器选最高兼容版本并支持回溯，安装器先把完整依赖图下载/校验/解包到 stage，再原子替换 `~/.harness/plugins/<name>`，最后写 `plugins.lock.json`。lock 记录版本、artifact digest、完整目录树 digest、来源、publisher、签名 key ID 和依赖；`-plugin-verify` 重算目录树、检查依赖版本并找出未入锁的托管插件，`-plugin-remove` 拒绝破坏仍被依赖的包。tar 解包拒绝绝对路径、目录穿越、链接、设备、重复路径和资源超限；远程 registry 仅 HTTPS（明文 HTTP 只给 loopback 开发），本地 registry 可离线使用
- **Ed25519 registry 签名与发布者信任根**（[plugins/signing.go](plugins/signing.go)、[plugins/keys.go](plugins/keys.go)、[cmd/plugin-registry-sign/](cmd/plugin-registry-sign/)）：registry 用 `ed25519` + 公钥 SHA-256 `key_id` 声明 publisher，签名覆盖带协议域的规范化元数据、artifact URL 和 artifact 摘要，因此无法只替换下载地址或版本说明绕过认证。信任根默认是 `~/.harness/plugin_publishers.json`（可用 `HARNESS_PLUGIN_TRUST_FILE` 覆盖），只保存原始 Ed25519 公钥；`-plugin-trust` / `-plugin-untrust` / `-plugin-trust-list` 管理它。远程 registry 默认拒绝未签名文档，本地未签名 registry 仍可用于离线开发，`-plugin-allow-unsigned` 是显式且带警告的远程降级。lockfile schema 2 保存 publisher / key ID，兼容旧 schema 1；lockfile 本身不是签名账本，吊销、过期、anti-rollback/freshness 和自动更新仍未实现。签名解决发布者认证，不替代运行时沙箱或权限模型
- **插件进程监督与健康可见性**（[ext/supervisor.go](ext/supervisor.go)、[plugins/plugins.go](plugins/plugins.go)）：插件可在 `plugin.json` 显式启用 `never` / `on_failure` / `always` 进程恢复，`max_restarts` 按扩展实例生命周期累计成功替换次数（默认 3，最大 100），初始退避默认 250ms、指数增长到默认 30s 上限。每次重启都重新握手，运行时 manifest 必须与首次批准时完全一致，禁止靠崩溃增加工具或削掉 consult 授权；预算耗尽、manifest 漂移或握手失败后停止。重启空窗期工具调用报错，所有策略 gate fail-closed，`input` 保持原样；`/plugins` 与 `get_plugins` 暴露状态、计数、最近退出和错误。它是进程恢复而非 checkpoint：子进程从零开始，不续跑未完成任务
- **Agent 环境 profile 与插件清单**（[agentprofile/](agentprofile/)、[plugins/](plugins/)、[docs/agent-profiles.md](docs/agent-profiles.md)）：把「不同环境需要不同 Agent」变成声明式装配，而不是在启动代码里加分支。一份 profile 同时选择模型、系统提示词、turn/token 上限、工具白名单、进程插件、MCP server、Skills、Prompt 模板和上下文压缩参数；`tools`/`plugins`/`mcp_servers`/`skills`/`prompts` 区分「省略 = 全部」和「`[]` = 全不要」，工具白名单在所有工具源注册完以后最后一次生效，引用不存在的工具直接启动失败。目录插件用 `plugin.json` 描述启动命令、固定 cwd、环境变量和能力授权，`cwd`/路径型入口在 symlink 解析后不能逃出插件目录；`capabilities` 是 `tools`/`commands`/`hooks`/`events`，未授权的工具、consult hook 和推送事件分别拒绝或丢弃，manifest 里给了命令但没授权 `commands` 则直接加载失败，旧可执行文件插件继续兼容。profile 的 `env` 只传给插件和 stdio MCP 子进程，不写回 harness 进程环境，`/profile` 只显示键不显示值。历史信任已保存的项目可以读取 `.harness/profiles/<name>.json`，首次信任仍然先判项目；用户路径和显式 profile 路径分别受支持。可见性有 REPL `/profile`、`/plugins` 和 RPC `get_profile`、`get_plugins`
- **重试、排队与 settle 边界**（[agent/retry.go](agent/retry.go)、[agent/agent.go](agent/agent.go)）：补上 pi 生命周期里「一次 run 不等于一次补全」的那部分。**重试**只针对一次模型请求，分类沿用 pi 的思路——额度/账单/鉴权/模型不存在/上下文超长这些**不可重试**优先于限流、5xx、传输中断这些可重试的；退避 `base * 2^(attempt-1)` 封顶（默认 1s 起、60s 顶），`max_retries` 负数即关闭；关键约束是**只有一段内容都没流出去才重试**——delta 一旦进了转录，第二次尝试会把答案打印两遍。**排队**让「模型还在干活时你又敲了一句话」不丢不打断：消息进队列，由正在跑的 run 在 `turn_end` 之后（工具结果都已落地，绝不插在调用和结果之间）和每次底层 run 结束后取走；`Enqueue` 永远不因为「没在跑」报错，所以 RPC 接受运行到运行真正开始之间的小窗口不丢输入。REPL/TUI 因此把 run 放到后台 goroutine，主循环继续读行，跑着时敲的行显示 `[queued: …]`。**settle 边界**给扩展最后一个决策点：`agent_before_settle` 可以 `Continue` 再跑一轮或注入消息，续跑有 8 轮上限（否则 handler 能让 agent 永远忙不完）；中止的运行**跳过** settle 边界，直接 `agent_settled{aborted:true}`。结论是 `agent_end` 不再是空闲信号——后面可能还有排队消息、重试或续跑——`agent_settled` 才是，RPC 客户端和 JSON 流消费者等的都是它
- **JSON 输出模式**（[jsonout/](jsonout/)、[cmd/harness/json.go](cmd/harness/json.go)）：`-print` 给的是答案，`--mode json` 给的是**整次运行**——这是 harness 层最后一个缺口（pi 里剩下的两个缺口：RPC 模式与 JSON 模式，都已补上）。输出是 JSONL 事件流：**第一行是会话头**（消费者看到任何事件之前就知道是哪个会话的，事件流存成文件能靠头找回会话），**最后一行是 `agent_settled`**（这一行之前没有更多自动工作，读到它就可以停）。**事件名就是 harness 自己的事件名**，消费者不用学第二套词汇——`agent_start` / `turn_start` / `request_start` / `message_start` / `message_update` / `message_end` / `tool_execution_start` / `tool_execution_end` / `agent_end`。两个设计点：`message_update` **只带增量**不带整条消息（增量多了整条消息跟着膨胀，消息本身在 `message_start` / `message_end` 里）；**推理的增量带 `"thinking": true`**，和答案的增量分开标记，否则同一个流里分不清哪段在想哪段在答。和 RPC 模式的分工：RPC 是**被驱动**的（stdin 收命令，来来回回），JSON 模式是**一次性**的（给一句提示、跑完、退出），两者共用 JSONL 分帧但记录形状不同（RPC 是「响应 + 事件」，JSON 只有「事件」）。stdout 同样归事件流所有，退出码报告运行成不成功
- **RPC 模式**（[rpc/](rpc/)、[cmd/harness/rpc.go](cmd/harness/rpc.go)）：harness 从「人用的 REPL」变成**长驻子进程**，被别的程序用 JSON 驱动——IDE、任何语言的脚本、自制 UI 都行，这是把 harness 嵌进别人产品里的唯一入口。协议是严格的 JSONL：stdin 收命令，stdout 收响应和事件；**stdout 归协议所有**（模型文本、工具提示、诊断全改走 stderr），一个读 stdout 的客户端永远不该看到别的东西。分帧**只按 LF 切**——记录里的字符串可以含 U+2028 / U+2029（JSON 里是普通字符），按它们切会把一条记录劈成两半；CRLF 客户端也接受。**响应和事件共用一条流**，所以客户端按 **ID 关联**而不是按顺序猜（一次运行的十几个事件会插在命令的响应之间）。`prompt` 是异步的：响应只说明「收下了」（`data.disposition` = `started`），答案走事件流，客户端要等 `agent_settled`（不是 `agent_end`——一次运行后面可能还跟着重试）；**响应必须先于运行写出**，否则一个等整轮跑完再读的客户端会在写满的管道上死锁。一条坏记录不毁会话：解析失败回一条 `command: "parse"` 的失败响应，流继续；未知命令同理。**关掉 stdin 是有序关机**：harness 等在跑的那次运行结束再退出，所以「发完 prompt 立刻关 stdin」的客户端仍然拿得到答案（这里踩过一个坑：run 的 context 原本挂在 server 的 context 上，stdin 一 EOF 就把运行取消了，于是加了 `sync.WaitGroup` 单独跟踪在跑的任务）。权限层在 RPC 模式下**不弹窗**——stdin 归协议所有，审批提示会吃掉客户端的记录；模型侧的调用走「无人可问就拒绝」，客户端自己发的 `bash` 走 `user_bash` 拦截（那是用户自己的命令，不是模型的）。这个模式还逼出了两处真实缺陷：agent 的 `history` 和 `cfg` 原本**没有任何锁**，而 RPC 会在模型还在跑的时候读历史、换模型，于是补上了 mutex 和「配置按快照读」；`isTerminal(stdin)` 在 RPC 模式下为真会让权限提示去读协议流，于是 RPC 一律视为非交互
- **Skills（Agent Skills 标准）**（[skills/](skills/)、[frontmatter/](frontmatter/)）：模板是**用户敲命令**才用的，Skills 是**模型自己按 description 决定**要不要读的——两者一起才是 pi 完整的「自己可改」。一个 skill 是一个目录 + 一个 `SKILL.md`；**渐进披露**是这一层的核心：启动时只有 `name`/`description`/`location` 进系统提示词（XML 格式），完整指示留在磁盘上，模型觉得对上了才用 `read` 去读，所以 50 个 skill 的库代价是 50 行提示词而不是 50 份文档。发现位置有四个（项目 `.harness/skills`、项目 `.agents/skills` 及其各层父目录、用户 `~/.harness/skills`、用户 `~/.agents/skills`），**先到先得**：项目在前、用户在后，项目定义的 skill 归项目，用户的目录是项目没用到的名字的兜底。`description` **必填**——它是唯一的路由信号，没有它 skill 根本不会被加载；`name` 缺省取目录名，`disable-model-invocation: true` 让 skill 只走 `/skill:name` 不进提示词；**不合规只警告不拒绝**（超长描述、非法名字照样加载），坏掉一个文件不该拖垮整个库。两个入口：模型自己 `read` 读 `SKILL.md`，或 `/skill:name [参数]` 显式把指示塞进对话（不依赖模型选不选对）。`/skills` 列出所有 skill 和来源路径。skill 的指示**跑不了任何东西**——它就是模型的输入，模型要跑脚本仍然得走工具，走同一套权限。`frontmatter/` 是抽出来的共用解析器（`prompts/` 和 `skills/` 共用一套 YAML 语义，标量 + `|`/`>` 块标量 + 转义；集合类型拒绝而不是猜），`prompts/` 里原先那份重复实现因此删掉了
- **Prompt 模板与项目上下文**（[prompts/](prompts/)、[projcontext/](projcontext/)）：pi 的「自己可改」不止扩展 API——模板和上下文文件都是**数据**，不改一行代码就能加命令、换提示词。**Prompt 模板**：`.md` 文件变成斜杠命令（文件名即命令名），可选 frontmatter（`description` / `argument-hint`），正文按 **shell 式**语法替换参数（`$1` / `$@` / `${1:-默认}` / `${@:N:L}`），参数本身按 shell 引号规则切分（`/review "API compatibility"` 是一个参数）；替换是**单遍**的，被替换进去的内容不会再被替换（模型自己的输出变不成新的占位符）；模板注册在**和内置命令、扩展命令同一个注册表**上，`/help` 直接列出，`/prompts` 还能显示来源路径（模板不对，多半是目录找错了）。**项目上下文**：`AGENTS.md` 是项目指示，**追加**到系统提示词；`SYSTEM.md` **替换**系统提示词；`AGENTS.md` 从工作目录**向上**找（仓库根那份对所有子目录生效，最近的同名文件优先），`SYSTEM.md` 只从工作目录本身读——从父目录继承一份系统提示词太意外了。两者都是数据，跑不了任何东西
- **遥测后端 · Prometheus remote write**（[telemetrybackend/prometheus.go](telemetrybackend/prometheus.go)、[telemetrybackend/prometheus_backend.go](telemetrybackend/prometheus_backend.go)、[telemetrybackend/prompb/](telemetrybackend/prompb/)、[telemetrybackend/snappy/](telemetrybackend/snappy/)）：第二个后端，证明 `Backend` 接口**真的与协议无关**——Prometheus 存的是时间序列，没有 trace，所以 `SendTraces` 是空操作并计入 `DroppedTraces`（把 counter 当 trace 存是编造契约里没有的数据）；契约的点分名翻译成 Prometheus 的合法名（稳定，dashboard 跨 run 不失效），指标名放在 `__name__` 标签里，counter 补 `_total`、upDownCounter 降级为 gauge；直方图展开成 `_bucket`（带 `le`，计数**累加**——Prometheus 的桶装的是「所有不超过上界」的观测）/`_count`/`_sum` 三件套；载荷是 protobuf + snappy，但 harness 的 `go.mod` 仍然是空的——两个编码器手写，**上游实现只作测试里的 oracle**，逐字节比对且 oracle 在自己的 module 里编译，所以零依赖和「不会悄悄分叉」两件事同时成立；`telemetry_prometheus_endpoint` / `telemetry_prometheus_headers` 配置
- **工具生态**（[tools/render.go](tools/render.go)、[shellext/](shellext/)、[tools/mutation_queue.go](tools/mutation_queue.go)、[mcp/resources.go](mcp/resources.go)）：四件事都落在扩展点上，核心没有多一分逻辑——**工具渲染器**对齐 pi 的 `ToolRenderers` / `ToolRendererResolver`：`RenderCall` / `RenderResult` 两个钩子 + 一条按注册顺序串起来的解析器链（链尾是工具自带的渲染器，再尾是内置渲染器），所以扩展可以接管任何工具（包括不是它自己的 MCP 工具）的呈现；内置工具各有自己的渲染器，`tools.DiffLines` 提供行级 diff。**`!` shell 快捷**：`!cmd` 直接跑命令并把结果按 pi 的格式放进对话，`!!cmd` 只给你看、模型看不到；命令走同一个 bash 工具所以沙箱照样生效，跑之前发 `user_bash` 事件供扩展拦截。**文件变更队列**：按真实路径（`EvalSymlinks`）给每个文件一条队列，同一文件的改动串行、不同文件并行，`write` / `edit` 都走队列，避免并发改动互相覆盖。**MCP resources**：`resources/list`、`resources/read`、`resources/templates/list` 落地，并注册 `list_mcp_resources` / `list_mcp_resource_templates` / `read_mcp_resource` 三个工具（沿用 pi 的命名，模型认得）；不带 `server` 时覆盖所有带资源的 server，单个 server 列举失败不挡其他并进 `errors`；二进制资源只描述不下载
- **遥测后端**（[telemetrybackend/](telemetrybackend/)）：把契约的数据发到真正的 collector——OTLP over HTTP + JSON 载荷，所有 OpenTelemetry collector 都收且不需要 protobuf 依赖；`Backend` 接口只有四个方法（SendTraces / SendMetrics / Flush / Close），再加一个后端不碰契约；编码器处理了 OTLP/JSON 的两个坑（64 位整数是十进制字符串、trace/span 标识符是 hex 而非 base64——这是 protobuf JSON 映射唯一的偏离），契约里的不透明标识符被哈希成合规长度且跨请求稳定；span 批量队列 + 指标定时刷新（计数器和直方图按「名字 + 属性集」聚合，bucket 按 schema 边界分桶）；5xx 重试一次、4xx 不重试、失败批次计数但不拖慢 agent；span 和指标的**属性**都按 schema 的敏感标记脱敏（脱敏在后端做——那是数据离开进程前的最后一道闸）；`telemetry_endpoint` / `telemetry_headers` / `telemetry_interval_ms` / `telemetry_timeout_ms` 配置
- **遥测契约**（[telemetry/](telemetry/)、[telemetryext/](telemetryext/)）：对齐 `pi-telemetry` 的厂商中立契约——`Context.StartSpan` 围绕回调开 span（回调同步、恰好一次、错误原样返回，契约里没有公开的 `end()`，Go 用 `telemetry.Deferred` 在自己的 goroutine 上表达 pi 的 promise 语义）、`Metrics`/`Propagation` 作为适配器的可选能力挂在每个 context 上、`Store` 读回 trace 与 metric 行；`NoopContext` 是没有遥测时的惰性实现，`MemoryContext` 是内存参考适配器；schema 是纯数据（span / event / metric / 属性类型 / 基数 / 敏感标记），harness 自带 `pi.harness.*` 词汇表（session / request / tool / compaction + 四个 metric）；采集器是又一个扩展，挂在事件总线上，靠事件开/关 span；JSONL 导出器按 schema 脱敏；隐私策略由测试守住——prompt、工具参数、工具输出、文件路径一律不进属性，只有名字、计数、耗时、token 数成为属性；`config.json` 的 `telemetry` / `telemetry_path` 开关与 `/telemetry` 命令

- **权限与沙箱**（[perm/](perm/)、[sandbox/](sandbox/)）：权限守卫作为扩展挂在 `tool_call` 钩子上（内核仍无内置权限，遵循 pi 的设计）；规则按「内置 deny 底线 → 首个命中即决定」判定，来源顺序为 config.json → 用户级 → 项目级 → 内置默认；allow/ask/deny 三态，ask 在无交互会话时默认拒绝（fail-safe）；`permissions.json` 用户级/项目级配置、`/permissions` 查看与 `/permissions allow <path>`；沙箱用 macOS Seatbelt 限制 shell（写白名单 + 读黑名单 + 网络开关），不可用时明确标注未受控
- **项目信任**（[cmd/harness/trust.go](cmd/harness/trust.go)）：项目自己的扩展、MCP server、权限规则、prompt 模板和 skill 在信任判定之前一个都不加载——克隆一个陌生仓库不该等于运行它的代码。判定顺序对齐 pi 的 `project_trust`：信任范围内的扩展先决定（`e.Trusted` + `e.Remember`）→ `~/.harness/trust.json` 里保存的决定 → config 的 `trust_project: true`（总是信任）→ 问用户；非交互会话没有人可问，**不加载**并在 stderr 说明，和权限层"无人可问就拒绝"同一条规则。`/trust` 查看、`/trust yes|no` 保存、`/trust reset` 忘掉；决定按绝对工作目录记录，和 pi 一样在下次启动生效。项目上下文文件（`AGENTS.md` / `SYSTEM.md`）不算项目资源：它们是给模型的指示，不会自己运行
- **MCP 工具接入**（[mcp/](mcp/)）：Model Context Protocol 客户端——stdio 子进程与 streamable HTTP 两种传输、`initialize` 握手与能力协商、`tools/list` 分页发现、`tools/call` 调用、工具以 `mcp__<server>__<tool>` 命名桥接进 `tools.Registry`；`mcp.json` 配置读取与用户级/项目级合并、`-mcp-list` 与 `/mcp` 命令；一个 server 失败不影响其他
- **会话树与分叉**（[session/tree.go](session/tree.go)、[session/fork.go](session/fork.go)）：会话文件升级为树格式（entry 带 `id`/`parentId`），`/tree` 列出并切换分支（模型上下文随分支切换），`/fork` 从指定用户消息分叉出独立会话文件、`/clone` 复制当前分支；当前位置（`leaf_pointer`）持久化，重载会话能回到正在走的分支；压缩写成 `compaction` entry（摘要替代被压缩的消息，原文保留在文件里）；旧 v1 线性格式加载时自动迁移
- **扩展动态加载与热重载**（[ext/protocol.go](ext/protocol.go)、[ext/process.go](ext/process.go)、[ext/scope.go](ext/scope.go)）：进程扩展协议（stdio 上的 JSON-RPC，扩展可用任何语言写）、manifest 握手、工具桥接、事件转发、扩展发现（`~/.harness/extensions`、项目目录、`config.json` 的 `extension_dirs`）；`/reload` 按 pi 语义重载（`session_shutdown` → 撤销注册 → 重新 `Setup` → `session_start`），注册项由 harness 自动记录并撤销，扩展只需实现 `ext.Teardown` 释放子进程等资源
- **扩展的反向通道（`event.consult`）**（[ext/protocol.go](ext/protocol.go)、[ext/process.go](ext/process.go)）：扩展原来只能往 harness 推事件，没法否决 harness 要做的事——它能提供工具，却拦不住一次调用。补一条 host→extension 的请求通道：harness 在动手之前问一句「这件事允许吗」，扩展答 `{"block":true,"reason":"..."}` 就否决，问的是内核真正要做决定的七件事（`tool_call` / `user_bash` / `input` / `session_before_compact` 和三个会话操作 `session_before_switch` / `session_before_fork` / `session_before_tree`），其中 `input` 除了否决还能用 `{"text":"..."}` 改写 prompt，会话操作用 `{"cancel":true}` 否决（问在任何副作用之前，取消后会话原样不动）。失败策略沿用 pi 的 **fail-closed**（"gate handlers should fail closed when their safety decision cannot be made"）——答不上来就拦下，坏掉的策略扩展不能变成一扇开着的门；`input` 是唯一的例外，改写算不出来就保持原样（否则坏扩展会吃掉每条消息）；比 pi 多两层保障：**等待有上限**（pi 的进程内 handler 卡住会挂住 harness，这里 5 秒放手）和**没声明就不问**（manifest 里写 `"consults": [...]` 才被问到，没声明的扩展它的沉默不算拒绝）。另外**已被否决的调用不再去问用户**（权限层看到 `e.Block` 就跳过）。进程内扩展走同一个事件总线用 `e.Block`，子进程扩展多走一次 JSON-RPC，两边语义一致
- **TUI 差分渲染**（[tui/](tui/)）：帧差分只重写变化的行、CSI 2026 同步输出防闪烁、ANSI 感知的显示宽度（含 CJK 宽字符）、Component 体系（Text/Container/VStack/HStack/Transcript/Editor）、主屏与备用屏两种渲染器。`-tui` 启用；模型输出流式更新单行，编辑器行不抖动
- **上下文治理与自动压缩**（[contextmgr/](contextmgr/)）：请求级预算不靠扩展，内核在每次 Provider 调用前估算“系统提示词 + 工具 schema + 消息 + 工具参数”；达到 prompt 预算的 75% 时自动分块摘要旧消息并递归合并，保留最新用户意图和最近消息单元，assistant tool calls 与 tool results 永不拆散；超大工具输出按内容 SHA-256 落到会话 artifact（`0700` 目录 / `0600` 文件），prompt 只留绝对路径和预览，模型可再用 `read` 读回。摘要写入当前分支的 `compaction` entry，重载会话得到同一份“摘要 + 保留消息”；`context` hook 改写请求时只压缩本次请求、不污染持久化。模型切换后按新模型窗口重算，显式 `context.window` / `max_tokens` 优先；`/context` 与 RPC `get_context` 暴露预算、阈值、artifact 路径和最近压缩统计。没有摘要器或摘要失败时退回确定性硬裁剪并告警，不伪造摘要。REPL `/compact` 仍可手动触发，`session_before_compact` 钩子仍可接管（钩子优先）
- **推理透传**：OpenAI 兼容流的 `reasoning_content`、Anthropic 流的 `thinking` 块都会作为 thinking 增量流向 UI（`-show-thinking` 显示），且不会污染转录内容。**同一个 chunk 同时带 reasoning 和正文时，两个都要出来**——早期实现遇到 reasoning 就直接 return，正文被整段吞掉，于是模型的回答缺了开头几个字（`HARNESS_SKILL_HIDDEN` 变成 `KILL_HIDDEN`）；现在多出来的事件进队列，下一次 `Recv` 取出来
- **网关兼容**：Anthropic provider 同时发送 `x-api-key` 与 `Authorization: Bearer`（OpenCode Go 等网关只认后者）
- **模型注册表与多 provider 路由**：`llm.Registry` 把「模型 → 协议 → 端点」解耦，`/model` 切换模型时自动换到对应 provider（跨协议、跨网关），`/models` 列出可用模型
- **远程模型目录**：`config.json` 的 `catalog_url` 指向网关目录，启动时自动同步（`llm.Registry.SyncCatalog`），并按目录里的 `api` 字段判定每个模型实际使用的协议
- **请求级 hooks**：`llm.Hooks` 提供 `BeforeHeaders` / `BeforeRequest` / `AfterResponse`，扩展可改写 HTTP 头、请求体并观察响应（对齐 pi 的 `before_provider_headers` 等）
- **Gemini 协议**：新增 `llm.GeminiProvider`（generateContent / streamGenerateContent，支持工具调用）

尚未做的深化项（按优先级）：

- **真正的隔离**：进程扩展目前靠能力声明和 fail-closed 的 gate 约束，不是沙箱——子进程和 harness 同一个用户、同一套文件权限，能读到的东西一样多。WASM 插件或按插件降权的子进程是下一步。这一项不是接口化能解决的：上面那三层可替换的内核层要的是 Go 值，跨进程的扩展拿不到，隔离要的是另一件事——限制子进程本身。
- **工具级重试**：现在的重试只针对模型请求。一次 `bash` 超时或一次网络工具失败，模型得自己重新发一次调用；让工具声明可重试 + 退避，能省掉一轮往返。
- **预算闸门**：`max_turns` 和上下文预算是有的，但一次运行花掉多少 token / 多少钱没有上限，也没有"到 X 就停"的开关。
- **多模态输入**：`llm.Message` 目前只有文本，图片输入（pi 的 `input: ["text","image"]`）没有落地。
- **事件 schema 版本化与 SDK**：事件形状写在 `ext/ext.go` 和 README 里，没有版本号也没有别的语言的客户端库——RPC 客户端目前只能照文档手写。

## License

MIT
