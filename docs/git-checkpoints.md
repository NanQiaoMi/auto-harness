# 内置 Git 与工作区检查点

这个功能把 Git 变成 harness 的内置能力，并让“每一步可退回”成为运行时
契约，而不是模型自觉执行的一条提示词。它只在 **Git worktree** 内工作；
非 Git 目录仍然可以使用全部普通工具，但没有自动检查点。

它不是任务级 checkpoint/resume：它不保存 agent 对话、进程内存、正在运行
的请求或未完成的工具调用。它也不是整机备份：工作区之外的文件、被
`.gitignore` 排除的内容和 `.git` 里的配置有自己的边界，见文末。

## 两个入口

### `git` 工具

```json
{"args":["status","--short"]}
{"args":["commit","-F","-"],"stdin":"feat: add parser\n"}
{"args":["apply","-"],"stdin":"diff --git a/a.txt b/a.txt\n..."}
{"args":["rebase","main"],"timeout_ms":600000}
```

- `args` 直接传给 `git` 可执行文件，不经过 shell，因此没有引号、管道和
  注入问题。
- 支持 `stdin`，用于 `git apply`、`git commit -F -`、`git hash-object
  --stdin` 等非交互输入，上限 10 MiB。
- `workdir` 必须是仓库内的相对目录；绝对路径和 `..` 逃逸会被拒绝。
- 输出截断到 30k 字符，默认超时 120 秒。
- 所有调用都在非交互环境中运行：关闭 pager、editor 和终端凭据提示。
  需要 editor 的 `commit`、`rebase -i`、`add -p` 等命令必须显式改成非
  交互形式；需要凭据的 `push` / `fetch` 在没有凭据助手时会失败。

工具本身覆盖任意 Git 子命令。网络操作仍然受权限层、沙箱和用户的
`ask` / `deny` 规则约束。

### `checkpoint` 工具、斜杠命令和 RPC

模型侧：

```json
{"action":"list"}
{"action":"create","label":"before migration"}
{"action":"restore","id":"20261117T101500.000000000Z-ab12cd34"}
{"action":"restore","id":"2","reset_head":false}
```

`action=restore` 接受完整 ID、唯一前缀或列表中从 1 开始的序号。
`reset_head` 默认 `true`，同时恢复 Git 历史；设为 `false` 只恢复工作树
和 index。

REPL：

```text
/checkpoints
/checkpoints create before migration
/checkpoints restore 2
/rewind
/rewind 20261117T101500.000000000Z-ab12cd34
/undo
```

RPC：

```json
{"id":"1","type":"get_checkpoints"}
{"id":"2","type":"create_checkpoint","label":"before migration"}
{"id":"3","type":"rewind_checkpoint","checkpoint":"2","resetHead":true}
```

## 自动检查点

启用后（默认启用），每次**可能修改工作区的工具调用之前**都会建立快照：

- `bash`、`write`、`edit`、未知扩展工具默认按可修改处理；
- `read` 和 MCP 资源读取工具标记为只读，不建立快照；
- `git` 工具先判断子命令。`status` / `log` / `diff` / `branch --list` /
  `stash list` 等只读调用不建立快照；`git branch topic`、裸 `git stash`、
  `git reset` 等会建立；
- `checkpoint` 工具自身跳过自动快照：`list` / `create` 不需要，
  `restore` 在内部先建立 safety 快照；
- 用户 REPL 的 `!cmd` / `!!cmd` 和 RPC `bash` 与模型工具共用同一条回退
  链，也会先建立快照。

检查点失败时，修改型工具**不会执行**（fail-closed）。这比“先改，快照
失败就算了”更重要：没有回退保证时不应该继续写。

关闭与容量：

```bash
./bin/harness -no-git-checkpoints
./bin/harness -checkpoint-limit 200
```

`-no-git-checkpoints` 只关闭自动快照和用户 shell 的自动快照；手动
`/checkpoints`、`checkpoint` 工具和 RPC 命令仍然可用。`-checkpoint-limit`
默认 100，必须至少为 1；它限制保留的检查点数量，最旧的 ref 会被删除。

## 捕获什么

每个检查点保存以下状态：

| 状态 | 机制 | 恢复效果 |
|---|---|---|
| 非忽略工作树 | 临时 index + `git add -A` + `write-tree` | 跟踪文件和未跟踪但未忽略的文件、内容、可执行位、符号链接 |
| 精确 index | 原始 `.git/index` 的 blob | 暂存状态、删除、重命名、merge 冲突的 stage 1/2/3 |
| HEAD | commit ID + 分支名/detached 标记 | 同分支或跨分支回退、detached HEAD |
| refs | `for-each-ref` 的 JSON blob | 本地分支、tag、远程跟踪 ref、notes、replace、bisect 等普通 ref，以及 `refs/remotes/origin/HEAD` 这类符号 ref |
| stash | `refs/stash` + `.git/logs/refs/stash` | stash 栈和 `git stash list` 的顺序 |

忽略文件（`.gitignore` 匹配的文件）**不会**进入快照；恢复时会保留当前
磁盘上的忽略文件。这样不会把 `node_modules`、本地密钥或大型构建产物塞
进 Git 对象库，但代价是：如果某一步删除了忽略文件，回退不能把它们变回来。
如果检查点中的跟踪文件需要占用一个当前被忽略文件或符号链接占据的路径，
恢复会明确失败并由 safety 快照回滚，不会覆盖该忽略文件。

## 恢复语义

### 默认 `reset_head=true`

恢复会：

1. 把 HEAD 指向检查点的分支和 commit（必要时切换分支或 detach）；
2. 用检查点 tree 重写非忽略工作树；
3. 用原始 index blob 恢复精确暂存状态（包括未解决的 merge 冲突）；
4. 恢复检查点中的 refs 和 stash 栈。

因此 `git switch -c feature`、`git commit`、`git branch`、`git tag`、
`git fetch` 更新远程 ref、`git stash` 等步骤都可以一起退回。

恢复前的状态会先写入一个**独立的 safety 检查点**。它不是去重后的旧
快照：每次恢复都有一个自己的 ID。于是 `/rewind` 本身可以再被
`/rewind <safety-id>` 撤回。

### 空选择器不会向前跳

`/rewind`、`/undo` 或 `{"action":"restore"}` 不带 ID 时，会先找到与当前
工作区完全匹配的检查点，再选择它**更旧**的下一个不同状态。连续执行不会
因为时间线里的 safety 快照而跳回较新的状态；要 redo，请显式选择
`/checkpoints` 里的某个 ID。

### `reset_head=false`

只恢复工作树和 index，不移动 HEAD、分支、tag、远程 ref 或 stash。适合
“我只想找回文件，但保留当前分支和历史”的场景。

### 跨分支和并发 worktree

检查点可以跨分支恢复，但会拒绝把某个分支改成另一个 **worktree 正在
checkout** 的状态。这样不会在不相关的工作树里制造“文件突然变了”的
事故。错误会指出占用该分支的工作树路径；可以移除那个 worktree，或使用
`reset_head=false` 只恢复文件。

### 失败回滚

恢复不是原子文件系统事务。如果恢复中途失败（例如目标路径被非空目录
挡住、ref 被另一个 worktree 占用、Git 命令失败），harness 会尝试用
safety 检查点把 HEAD、工作树、index、refs 和 stash 全部放回恢复前的
状态，并返回包含原始错误和回滚结果的错误。回滚本身失败时也会明确报告，
不会假装成功。

## 存储与安全

检查点提交存放在自定义 ref：

```text
refs/harness/checkpoints/<UTC timestamp>-<random>
```

它们不提交到用户分支，也不会自动出现在 `git log` 里。检查点提交的
message 保存 JSON 元数据；tree/index/refs/stash 内容作为 Git 对象保存。
除 safety 外，状态与最新检查点完全相同时不会重复创建。

清理只删除旧 ref，不立即删除对象；实际磁盘回收要等 Git 自己 `gc`。
快照包含未跟踪但未忽略的文件，**可能包含本地密钥或隐私数据**。这些对象
只在本仓库的 `.git/objects` 中，`git push` 默认不会推
`refs/harness/*`，但 `git push --mirror`、`git bundle --all` 或类似
“推所有 ref”的操作会把它们包含进去。不要把含敏感文件的检查点镜像推送
到不受信任的远端。

## 明确不覆盖

以下内容不在工作区检查点的恢复范围内：

- `.git/config`、hooks、`FETCH_HEAD`、除 stash 外的 reflog、`.git`
  下的其他本地元数据；
- 被 `.gitignore` 排除的文件内容；`git clean -fdx` 或 `bash rm` 删除
  它们后无法通过 `/rewind` 找回；
- 空目录本身。恢复不会删除与目标无关的空目录；如果目标文件需要占用
  一个包含忽略文件的非空目录，恢复会拒绝并回滚；
- submodule 的工作树；Git LFS、split index、sparse index、partial clone /
  promisor remote 的行为未单独验证，快照只保证普通 Git 对象和当前
  checkout 能表达的内容；
- worktree 元数据、`git worktree add` 新建的目录、bare repository；
- 工作区之外的文件、网络请求、数据库、进程状态、agent 对话和未完成
  的任务；
- 文件 mtime、ACL、xattr、稀疏 checkout 之外的内容；
- 并发多个 harness 进程同时写同一仓库的跨进程事务（单进程内由 Manager
  mutex 串行化）。

需要这些边界时，应该使用专门的备份、数据库迁移工具或任务级
checkpoint/resume，而不是把这个机制说成万能快照。

## 验证

`gitkit/gitkit_test.go` 覆盖：

- 工作树、精确 index、同分支提交、初始提交、detached HEAD；
- 跨分支恢复和 safety 撤回；
- 分支/tag/远程 ref/符号 ref 的恢复和额外 ref 的删除；
- stash 栈和 stash reflog；
- merge 冲突 index 的保存与恢复；
- 忽略文件保留、同路径忽略文件和目录替换冲突拒绝并回滚、无关空目录
  保留、文件/目录替换；
- 另一个 worktree 正在使用的分支保护；
- 连续空选择器回退不向前跳；
- 只读/修改型 Git 子命令分类和 `git` 工具的 stdin；
- 检查点失败时拒绝执行修改型工具。

常用验证命令：

```bash
go build ./...
go vet ./...
go test -count=1 ./...
go test -race -count=1 ./...
```
