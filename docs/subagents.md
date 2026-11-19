# Delegated agents

`orchestrator` adds durable, isolated background agents to the harness.
It is deliberately not a second agent loop: each delegated task is a
normal `agent.Agent` run with its own session, context manager and
workspace-bound tools. The parent agent only receives a bounded report.

## What is isolated

A delegated task gets:

- a fresh `session.Session` with a parent-session link; it does not
  inherit the parent transcript;
- its own `contextmgr.Manager`, summarizer and artifact directory;
- a cloned tool registry with `read`, `write`, `edit`, `bash`, `git` and
  `checkpoint` rooted at its own workspace;
- its own permission/telemetry event identity, while sharing the
  parent's extension bus;
- a workspace selected by `isolation`.

The parent receives only the child's final text, truncated to
`subagents.result_bytes` (UTF-8-safely). The full child transcript stays
in its own session file; the result points at the task session path.
Model-facing `spawn`/`list` snapshots omit the original prompt, so a
large delegation request is not echoed back into the parent context.

## Isolation modes

`agent action=spawn` accepts `isolation`:

| mode | behavior |
|---|---|
| `auto` | use a detached linked Git worktree when the parent is in a repository; otherwise use the shared workspace |
| `worktree` | require a Git repository and create a detached linked worktree |
| `shared` | deliberately use the parent's working directory; suitable only when the task is read-only or the caller accepts concurrent edits |

`auto` is the default. A worktree is created from `HEAD`; uncommitted
changes in the parent are **not** copied. The child sees the committed
baseline, not the parent's dirty working tree.

The successful worktree is retained after the child finishes because it
may contain uncommitted work. No automatic merge, cherry-pick or discard
is performed. Inspect the task's `workspace` in `/agents show <id>` and
use normal Git tools/commands to review or apply it. Use
`/agents cancel <id>` to stop work; cancellation does not delete a
worktree.

If preparing a task fails after creating a worktree, the worktree is
removed as part of the failed preparation. A successful task never
deletes its workspace automatically.

## Lifecycle and limits

The task states are `queued`, `running`, `completed`, `failed`,
`cancelled` and `interrupted`.

- `max_concurrent` bounds the number of simultaneously running child
  agents (default 4).
- `max_depth` bounds recursive delegation (default 3). At the maximum
  depth, the child no longer receives the `agent` tool; the orchestrator
  also enforces the limit using the context task ID, not a model-supplied
  parent ID.
- `timeout_seconds` bounds one child run (default 1800). A timeout fails
  that task and its queued follow-ups.
- `result_bytes` bounds the summary passed back to the parent (default
  6000).
- `cancel` cancels the task and all of its descendants. Cancellation is
  idempotent for terminal tasks.
- A failed or timed-out parent cancels its still-running descendants;
  a normally completed parent leaves intentionally spawned background
  tasks running until they are waited for or cancelled.
- `wait` is a poll-friendly wait. A timeout returns the current snapshot
  with `timedOut: true`; it does not fail the task.
- The parent's tool retry policy (`tool_max_retries`, `tool_retry_delay_ms`,
  `retry_max_delay_ms`) is inherited: the same tools fail the same way one
  level down. `max_run_tokens` is **not**: each child has its own spend, and
  handing every child the parent's limit would multiply the allowance by the
  number of children rather than bounding the tree.

Follow-up prompts can be sent to a live, completed task. A task loaded
from disk during a new harness process is history-only: it is shown in
`/agents` and RPC, but `send` returns a "restored from disk" error rather
than silently queueing a prompt with no worker to run it. Set up a new
task if the work needs to continue.

## Tool and REPL interface

The model-facing tool is `agent`:

```json
{"action":"spawn","prompt":"Review the parser and propose a patch.","name":"parser review","isolation":"auto"}
{"action":"wait","id":"sub-...","timeout_ms":30000}
{"action":"send","id":"sub-...","prompt":"Now run the focused tests."}
{"action":"status","id":"sub-..."}
{"action":"list","recursive":true}
{"action":"cancel","id":"sub-..."}
```

The human interface is the `/agents` command:

```text
/agents
/agents list
/agents show <id-prefix>
/agents wait <id> [seconds]
/agents cancel <id>
```

IDs may be abbreviated when the prefix is unique. The RPC mode exposes
`get_agents`, `get_agent` (`agentId`) and `cancel_agent` (`agentId`).

RPC clients still receive only the primary agent's events in the normal
event stream. Delegated task activity is kept out of the parent
transcript; use the task endpoints and the task's session file to
inspect it.

## Git checkpoints

Every child uses the same checkpoint machinery as the primary agent.
When the parent started with automatic checkpoints, a child creates a
`subagent start` checkpoint before its first run. `-no-git-checkpoints`
is inherited: children skip automatic snapshots and the startup
checkpoint, but explicit `checkpoint`/Git commands remain available.

Checkpoints in a worktree are local to that worktree and its shared Git
object store. They do not merge the child's changes into the parent
checkout.

## Permissions

The permission guard sees delegated events with their workspace, so
relative paths are resolved against the child worktree. A delegated
task cannot prompt on the parent terminal. If a call needs interactive
approval and no `allow` rule covers it, it is blocked with:

```text
delegated agents require pre-approved permission rules
```

Configure allow rules for the operations child agents are expected to
perform. `shared` isolation has the same fail-closed behavior; it does
not grant extra approval authority.

## Configuration

```json
{
  "subagents": {
    "max_concurrent": 4,
    "max_depth": 3,
    "timeout_seconds": 1800,
    "result_bytes": 6000
  }
}
```

CLI:

```bash
./bin/harness -no-subagents
```

Task records live under:

```text
<session_dir>/subagents/<parent-session-id>/tasks/
```

They are atomically written JSON records. The store contains prompts
and bounded summaries, so treat it as local sensitive data. On a
harness restart, tasks that had not reached a terminal state are marked
`interrupted`; completed and failed history is preserved. Running work
is not silently resumed.

## Known boundaries

- Isolation is filesystem/Git isolation, not a Linux container, SSH or
  remote execution backend. On non-macOS systems the shell may not have
  an OS sandbox; use dedicated workspace tools and pre-approved rules.
- Absolute paths and arbitrary shell commands can still reach outside a
  worktree unless the platform sandbox blocks them. The built-in
  `read`/`write`/`edit` tools reject lexical and symlink escapes.
- Extension/MCP tools are cloned from the parent registry. A tool whose
  closure is hard-coded to the parent directory is not automatically
  rebound to the child worktree.
- The child's `contextmgr` compresses independently. The parent does
  not receive the child's raw history or its intermediate tool output.
- A worktree is based on `HEAD`; parent dirty files and untracked files
  are not visible to the child. This is intentional protection for the
  user's working tree.
