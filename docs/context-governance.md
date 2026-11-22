# Context governance

`contextmgr` keeps one provider request inside the model window without
throwing away the thread of work. It is a runtime service, not an
extension: every agent entry point (REPL, one-shot, JSON, RPC, prompt
template, skill) goes through the same `Builder`.

## Goals

- Never split an assistant tool call from its tool results.
- Preserve the latest user intent and the recent working set.
- Summarize older work into a faithful, dense capsule.
- Move oversized tool output out of the prompt without losing it.
- Persist an automatic compaction so a reloaded session rebuilds the
  same context.
- Degrade deterministically when the summarizer is unavailable.
- Make every decision inspectable through `/context` and `get_context`.

## Non-goals

- Reproducing a provider's exact tokenizer. The default estimator is
  deliberately conservative; applications can supply an exact one.
- Summarizing an oversized system prompt or tool schema. Those are
  instructions and contracts, not transcript; the manager warns rather
  than silently rewriting them.
- Audio/video token accounting. Static images (`llm.Image`) are
  estimated by pixel budget (256..2048 tokens) and degraded to compact
  `[image: ...]` text placeholders when older turns are summarized.
- Encrypting local session or artifact files.

## Interfaces

```go
type Builder interface {
	Build(ctx context.Context, in Input) (Result, error)
}

type Estimator interface {
	EstimateText(string) int
	EstimateMessage(llm.Message) int
	EstimateMessages([]llm.Message) int
	EstimateTools([]llm.ToolDefinition) int
}

type Summarizer interface {
	Summarize(ctx context.Context, messages []llm.Message, maxTokens int) (string, error)
}

type ArtifactStore interface {
	Put(ctx context.Context, name, content string) (string, error)
}
```

`Builder` is the policy seam. `Estimator`, `Summarizer` and
`ArtifactStore` are the component seams. A host can replace any of them
without changing the agent loop.

`Inspector` is an optional read-only capability:

```go
type Inspector interface {
	Status(in Input) Status
}
```

The default manager implements it. `/context` and the RPC
`get_context` command use it and never return message or summary text.

## Budget

```
prompt budget = context window - maximum output - safety reserve
compact at    = prompt budget * compact_at
recent budget = min(configured recent budget, prompt budget)
```

The context window comes from the model catalog for the model selected
on that turn. An explicit `context.window` or `max_tokens` wins over the
catalog. This is why switching `/model` is safe: the next request is
budgeted for the new model, not the model used at startup.

The default estimator counts ASCII at four characters per token and
non-ASCII runes at one token each, then applies a 1.10 safety factor.
That intentionally over-estimates CJK and code-heavy prompts.

## Pipeline

1. **Measure** the whole request: system prompt, tool schemas, messages,
   tool-call arguments and envelopes.
2. **Stop early** when the estimate is at or below the compaction
   threshold; messages are returned unchanged.
3. **Group** messages into units. An assistant message with tool calls
   and the following tool-result messages are one unit.
4. **Split** the units into old and recent. The recent set honors both
   `keep_recent_messages` and `keep_recent_tokens`; the latest user unit
   is forced into the recent set.
5. **Summarize** the old units in chunks, then merge the partial
   capsules pairwise until one capsule remains. A provider failure is a
   warning, not a fatal request error.
6. **Fit** the new history. Oversized tool results become artifacts;
   only complete units are dropped; a final truncation pass is the last
   resort.
7. **Persist** the compaction entry on the current session branch. The
   matching is done against `Tree().Branch()`, never against every
   entry in the file, so an abandoned branch with identical text cannot
   steal the compaction point.
8. **Emit** `session_compact` with automatic, before/after token
   estimates, prompt budget, warnings and artifact paths.

If a `context` hook rewrote the outgoing messages, the manager still
compacts the request but does not persist it. The hook's temporary
content must not become part of the durable transcript.

## Artifacts

`FileArtifactStore` uses the SHA-256 of the content in the filename and
an atomic rename. Repeated builds of the same request reuse the same
path. The directory is `0700`; files are `0600`. The prompt contains an
absolute path and a UTF-8-safe preview, so the model can read the full
output with the normal `read` tool.

Artifacts contain the same raw tool output the session already records.
They are local data, not a trust boundary. Do not commit the session
directory or artifact directory to a repository.

## Failure behavior

| Condition | Behavior |
|---|---|
| below threshold | return messages unchanged |
| summary provider fails | warning; continue to fit/drop path |
| no summarizer | warning; deterministic drop when over budget |
| a single tool result is huge | artifactize it, then re-measure |
| artifact write fails | warning; fall back to clipping/dropping |
| system prompt + tools exceed budget | warning; trim what can be trimmed |
| context hook rewrites request | compact request only; no session write |
| session retained suffix not found | do not write a compaction entry |

The last row is important: writing a compaction with an empty
`firstKeptEntryId` would replace the whole branch with a summary on
reload. Skipping persistence keeps the durable state safe.

## Testing

The unit tests cover:

- no-op below threshold;
- old-history summarization with latest-user preservation;
- recursive chunking and merging;
- hard-trim fallback without a summarizer;
- artifact content, permissions, deduplication and readback;
- tool-call/tool-result unit integrity;
- summary failure fallback;
- model-switch budget resolution and explicit overrides;
- automatic compaction persistence, reload and branch selection;
- no persistence when a context hook rewrites the request;
- RPC `get_context`.
