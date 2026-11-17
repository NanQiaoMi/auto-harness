# Agent profiles and plugin manifests

An agent profile is the declarative environment layer between the
harness kernel and a particular way of working. It selects the model,
prompt, tool surface, plugins, MCP servers, skills, prompt templates,
context policy and child-process environment without adding another
code path to the agent loop.

A profile does not make code trusted. Every plugin and MCP process it
names still goes through the normal trust, permission and execution
paths.

## Selecting a profile

```bash
# A name in ~/.harness/profiles/coding.json
./bin/harness -profile coding

# An explicit file, regardless of the current project
./bin/harness -profile ./team/ci-profile.json

# The same default for embedding or CI
export HARNESS_PROFILE=coding
./bin/harness -rpc

# List names, scopes and paths without loading a model
./bin/harness -list-profiles
```

Selection order:

1. Explicit profile path.
2. A trusted project's `.harness/profiles/<name>.json`.
3. The user's `~/.harness/profiles/<name>.json`.

Project profiles use the project trust decision already saved in
`~/.harness/trust.json`, or `trust_project: true`. A first run that is
still waiting for the interactive trust prompt does not load a project
profile: model and session assembly happen before that prompt. Trust
the project in one run, then select its profile in the next. An
explicit profile path is always available because the user named it.
After the normal trust pass, the runtime rechecks the decision and
aborts if an extension vetoed the project. No project-selected plugin,
MCP server, skill or prompt is assembled in that case.

A project profile is configuration that can select code. Its
`plugin_dirs`, `skill_dirs` and `prompt_dirs` are resolved and validated
when the profile loads, and after trust they may point outside the
repository. Review those paths before trusting a project, exactly as
you would review its extension list.

## Profile schema

```json
{
  "schema_version": 1,
  "name": "coding",
  "description": "Repository work with shell and file tools",
  "provider": "anthropic",
  "model": "claude-sonnet-5-5",
  "base_url": "${HARNESS_GATEWAY_URL:-https://api.anthropic.com}",
  "system_prompt_file": "prompts/system.md",
  "max_turns": 40,
  "max_tokens": 8192,
  "tools": ["bash", "read", "write", "edit", "mcp__docs__search"],
  "plugins": ["pdf-tools", "git-tools"],
  "plugin_dirs": ["../team-plugins"],
  "mcp_servers": ["docs", "filesystem"],
  "skills": ["pdf-tools", "release-notes"],
  "skill_dirs": ["../shared-skills"],
  "prompts": ["review"],
  "prompt_dirs": ["../shared-prompts"],
  "context": {
    "compact_at": 0.7,
    "keep_recent_messages": 6,
    "artifact_threshold": 24000
  },
  "env": {
    "PDF_CACHE_DIR": "${HOME}/.cache/pdf-tools",
    "LOG_LEVEL": "info"
  }
}
```

Unknown fields, a wrong schema version, duplicate selections and bad
values fail at load time. A typo must not silently produce a weaker
environment.

### Selection fields

| Field | Effect |
|---|---|
| `provider`, `model`, `base_url` | Model routing. Missing provider is inferred from the model. |
| `system_prompt`, `system_prompt_file` | Mutually exclusive prompt override. |
| `max_turns`, `max_tokens` | Agent-loop and completion caps. |
| `tools` | Exact active-tool allowlist after every tool source has registered. |
| `plugins` | Process-plugin allowlist by manifest `name`, or executable basename for legacy plugins. |
| `plugin_dirs` | Additional plugin discovery roots. |
| `mcp_servers` | MCP server allowlist from the merged user/project/config catalog. |
| `skills`, `skill_dirs` | Skill selection and additional skill roots. |
| `prompts`, `prompt_dirs` | Prompt-template selection and additional roots. |
| `context` | Overlay on the effective context-governance settings. |
| `env` | Variables passed only to plugin and MCP child processes. |

CLI flags win over the profile. The profile in turn wins over the
config file and environment defaults.

Provider-specific defaults are resolved against the final provider, not
the provider that happened to be active before the profile loaded. A
profile that switches to Anthropic therefore uses `ANTHROPIC_API_KEY`
and `ANTHROPIC_BASE_URL`; switching to an OpenAI-compatible endpoint
uses `OPENAI_API_KEY` and `OPENAI_BASE_URL`. Explicit `api_key` /
`base_url` values in the config file, an explicit profile `base_url`,
and the corresponding CLI flags remain authoritative.

### Empty is not missing

For `tools`, `plugins`, `mcp_servers`, `skills` and `prompts`:

- **field omitted or `null`** means normal discovery; use everything.
- **`[]`** means an explicit empty set; expose none.
- **`["name"]`** means exactly those entries.

This distinction is why an agent profile can describe a tool-free
narrator, a shell-only operator and a full coding environment without
special modes in the kernel.

## Environment expansion

`env` values and path fields support:

```text
${NAME}            required; missing is an error
${NAME:-fallback}  fallback when not set
$$                 a literal dollar sign
```

Bare `$NAME` stays literal, which avoids damaging prompts and shell
snippets. Profile `env` values are never printed by `/profile` or RPC;
only their keys are visible. Do not put raw credentials in a profile:
reference the existing environment variable instead.

The same syntax and strict missing-variable behavior applies to MCP
`command`, `args`, `cwd`, `env`, `url` and `headers`. Profile values
win over the harness process environment there as well.

Relative `system_prompt_file`, `plugin_dirs`, `skill_dirs` and
`prompt_dirs` paths resolve against the profile file's directory.

## Plugin manifest v1

A directory plugin contains `plugin.json` and its program:

```text
pdf-tools/
├── plugin.json
├── plugin.py
└── references/formats.md
```

```json
{
  "schema_version": 1,
  "name": "pdf-tools",
  "version": "0.1.0",
  "description": "PDF extraction and conversion tools",
  "command": ["python3", "plugin.py"],
  "cwd": "${pluginDir}",
  "env": {
    "PDF_CACHE_DIR": "${PROFILE_CACHE_DIR}"
  },
  "capabilities": ["tools", "hooks"],
  "supervisor": {
    "restart": "on_failure",
    "max_restarts": 3,
    "backoff_ms": 250,
    "max_backoff_ms": 30000
  }
}
```

The descriptor is a launch and identity contract. It is **not** a
second tool registry: after launch, the extension's runtime `manifest`
response remains the authority for tool names, JSON schemas and
consulted events.

Rules:

- exactly one JSON value, with unknown fields rejected;
- `schema_version: 1`, a stable `name`, and a non-empty `version`;
- `command` is a non-empty string array;
- `cwd` resolves inside the plugin directory, including after symlink
  resolution;
- a path-like executable also resolves inside the plugin directory;
  use a bare command such as `python3` for a PATH lookup;
- `capabilities` may contain `tools`, `hooks` and `events`. If omitted,
  the plugin keeps the legacy unrestricted behavior. If present, it is
  an explicit grant: the runtime rejects undeclared tools or consult
  hooks and drops undeclared pushed events.
- `supervisor` is optional and controls process recovery. If it is
  omitted, the plugin keeps the historical one-process lifetime.
  `restart` must be `never`, `on_failure` or `always`;
  `max_restarts` must be between 0 and 100;
  `backoff_ms` and `max_backoff_ms` must be between 0 and 60000, and
  the initial backoff cannot exceed the maximum.

Available expansion names in a plugin manifest:

```text
${PLUGIN_DIR} or ${pluginDir}
${PLUGIN_NAME}
${PLUGIN_VERSION}
${PROFILE_ENV_NAME}
```

Profile `env` is available while the manifest is validated and is
merged into the child environment. Manifest `env` overrides profile
`env`. For manifest plugins the harness always sets `HARNESS_PLUGIN_DIR`,
`HARNESS_PLUGIN_NAME` and `HARNESS_PLUGIN_VERSION`; those reserved
values win last.

Legacy plugins remain supported. An executable directly in
`~/.harness/extensions`, `<project>/.harness/extensions`, a configured
`extension_dirs` entry, or directly inside a manifest-free subdirectory
is still launched as before. A manifest plugin is one candidate even
when its directory contains helper executables.

### Supervised process lifecycle

Supervision is process-level recovery, not task checkpointing. A
restart starts a fresh child: in-memory state, open work and unfinished
calls are not resumed. The value is keeping a stateless tool or policy
process available after a crash, not pretending that an interrupted
task survived it.

`restart` has three modes:

- `never` keeps the historical one-process lifetime;
- `on_failure` replaces a child that exits non-zero;
- `always` also replaces a clean exit.

If `max_restarts` is omitted it defaults to 3. The count is the number
of successful replacements over the lifetime of this loaded extension,
not a per-crash allowance. Explicit `0` means one crash is final. After
the budget is exhausted, or after 100 replacements, no further child is
started and the plugin becomes unhealthy. `/reload` creates a new
extension instance and resets the budget.

`backoff_ms` defaults to 250 and `max_backoff_ms` to 30000. A value of
`0` for either duration selects its default. The delay doubles after
each replacement until it reaches the maximum; nonzero values below
10 ms are raised to 10 ms and the process API clamps the maximum at
60 seconds.

Every replacement must pass the manifest handshake. Its runtime
manifest must be exactly equal to the manifest approved during the
initial setup. A plugin cannot add tools, drop consult authority or
otherwise change its declared capabilities by crashing. A changed
manifest, repeated handshake failure or exhausted restart budget
terminates supervision for this instance.

Tool registrations stay in the host registry across a restart and
route to the new child once it is running. While the child is down or
inside a restart backoff:

- tool calls return an error because there is no live process;
- consult gates such as `tool_call` and `user_bash` fail closed;
- `input` keeps its safe default, leaving the user's prompt unchanged;
- stopping or reloading the extension cancels the pending backoff and
  waits for the supervisor to exit.

Health is visible in `/plugins` and `get_plugins`. For a loaded plugin,
the state is `loaded`, `restarting` or `unhealthy`; the payload also reports
`restartMode`, `restartCount`, `maxRestarts`, `healthy`, `restarting`,
`lastExit` and `lastError`. These are point-in-time observations, so a
successful restart can change them between two reads.

Earlier discovery roots win duplicate names. That keeps the user's
plugin ahead of a project plugin trying to replace the same name.

Built-in Go extensions and `config.json`'s `extension_command` are base
runtime, not catalog plugins: the profile allowlist applies to plugins
found in the user, project and `plugin_dirs` discovery roots.

## Registry installation and lockfile

Managed plugins are installed below `~/.harness/plugins/<name>` and
discovered in the same user-trusted scope as `~/.harness/extensions`.
A profile still has to select a plugin by name; installation does not
activate it automatically.

```bash
# Local registry document
./bin/harness \
  -plugin-index ./registry.json \
  -plugin-install pdf-tools@^1.2.0

# HTTPS registry
./bin/harness \
  -plugin-index https://plugins.example.com/index.json \
  -plugin-install pdf-tools

./bin/harness -plugin-list
./bin/harness -plugin-verify
./bin/harness -plugin-remove pdf-tools
```

`HARNESS_PLUGIN_INDEX` supplies the default registry source. `@` takes
an exact version or one of these constraints:

```text
*
1.2.3
>=1.0.0 <2.0.0
^1.2.3       >=1.2.3 and <2.0.0
~1.2.3       >=1.2.3 and <1.3.0
```

Versions are stable `MAJOR.MINOR.PATCH` values. Dependency resolution
selects the highest satisfying release for the requested plugin and
all transitive dependencies, backtracking when two packages impose
different constraints.

The registry document is strict JSON. A signed registry adds a stable
publisher and an Ed25519 signature:

```json
{
  "schema_version": 1,
  "publisher": "acme",
  "plugins": {
    "pdf-tools": {
      "description": "PDF extraction and conversion",
      "versions": {
        "1.2.0": {
          "artifact": {
            "url": "artifacts/pdf-tools-1.2.0.tar.gz",
            "sha256": "0000000000000000000000000000000000000000000000000000000000000000"
          },
          "dependencies": {
            "shared-runtime": ">=1.0.0 <2.0.0"
          }
        }
      }
    }
  },
  "signature": {
    "algorithm": "ed25519",
    "key_id": "ed25519:<sha256-of-public-key>",
    "value": "<base64-signature>"
  }
}
```

The signature covers canonical JSON with the `signature` field removed,
prefixed by the fixed registry-signature protocol domain. Publisher,
plugin metadata, artifact URLs, artifact SHA-256 values and dependency
constraints are all authenticated as one document. `key_id` is the
lowercase SHA-256 fingerprint of the raw Ed25519 public key.

Create and trust a publisher key with the included signer. Key
generation refuses to overwrite existing files and writes the private
key with mode `0600`. The signer accepts a PKCS#8 PEM Ed25519 private
key; the trust command accepts a PKIX PEM public key or its raw base64
form.

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
```

OpenSSL 3 can generate the same key formats, but some LibreSSL versions
shipped with macOS do not support Ed25519.

The trust root defaults to `~/.harness/plugin_publishers.json`;
`HARNESS_PLUGIN_TRUST_FILE` overrides the path. It stores public keys
only. A remote HTTP(S) registry must be signed by a trusted publisher
unless the caller explicitly passes `-plugin-allow-unsigned`, which
prints a warning. Local registry files may remain unsigned for offline
development. A signed local or remote registry with an unknown
publisher, unknown key ID, altered payload or invalid signature is
rejected.

An artifact URL resolves relative to the registry document. Local
registry files may reference local artifacts; remote registries may
reference HTTPS artifacts. Plain HTTP is accepted only for loopback
development servers. Artifacts are `.tar.gz` archives with
`plugin.json` either at the archive root or inside one top-level
directory.

Installation is transactional and fail-closed:

1. Apply the registry trust policy and verify the signature before
   trusting any metadata.
2. Resolve the complete dependency set.
3. Download/read and SHA-256-check every artifact before extraction.
4. Reject absolute paths, traversal, duplicate paths, links, devices,
   oversized archives and oversized expanded trees.
5. Verify each manifest name/version matches the registry release.
6. Compute a deterministic digest of each complete plugin tree.
7. Stage every replacement before swapping managed directories.
8. Write the lockfile last; any commit failure restores the previous
   directories.

`~/.harness/plugins/plugins.lock.json` schema 2 records the version,
artifact digest, tree digest, registry source, publisher, signer key
ID and dependency constraints. Schema 1 lockfiles remain readable and
are upgraded on the next write. The lockfile is not itself signed, so
it is provenance metadata rather than tamper-evident history.
`-plugin-verify` recomputes every tree digest, checks locked dependency
versions and reports manifest directories not present in the lock.
`-plugin-remove` refuses to remove a plugin another managed plugin
still depends on.

SHA-256 proves that the downloaded artifact and installed tree match
the registry metadata. Ed25519 registry signatures additionally prove
which trusted publisher signed that metadata and its artifact digests.
They do **not** sandbox plugin code, revoke a compromised key, prove
that a fresh registry is newer than a previously seen one, or provide
automatic updates and rollback channels. Key revocation/expiry,
anti-rollback/freshness and registry-driven update policy remain
separate security and lifecycle work.

## Assembly order

The order is deliberate:

1. Load config and resolve the profile.
2. Apply profile, then CLI overrides.
3. Build the model registry and session.
4. Load trusted built-ins, legacy/config extensions and selected plugins.
5. Resolve project trust, then load trusted project resources.
6. Register built-in tools (`bash`, `read`, `write`, `edit`, `git`,
   `checkpoint`) and the permission guard.
7. Connect selected MCP servers and register their tools.
8. Load selected skills and prompt templates.
9. Apply the exact active-tool allowlist.
10. Build the agent and context manager.

Applying the tool allowlist last means it can include built-ins, plugin
tools and MCP tools in one list. An unknown tool is a startup error,
not a silent omission. A profile-selected MCP server that is missing or
fails to connect is a startup error for the same reason. Normal
best-effort discovery remains the default when the profile does not
name a server list.

Workspace checkpoints are a runtime safety mechanism, not part of the
profile. Even when a profile hides the `checkpoint` or `git` tool, a
mutating tool that remains active still gets an automatic checkpoint
before it runs (unless the process starts with `-no-git-checkpoints`).
The `checkpoint` tool is how the model can inspect or restore that
history; hiding it does not turn the rollback guarantee off.

## Visibility

```text
/profile    active profile, source, selections, active tools and warnings
/plugins    plugins, versions, paths, selection, load and supervision health
/mcp        selected and connected MCP servers
/skills     loaded skills and missing selections
/prompts    loaded prompt templates
```

RPC clients can use:

```json
{"id":"p","type":"get_profile"}
{"id":"q","type":"get_plugins"}
```

`get_profile` reports selection modes and environment **keys**, never
environment values or prompt text.

`/reload` performs profile discovery before replacing the live
extension set. If a profile-selected plugin disappeared, the old
environment stays active and the command reports the missing selection
instead of half-loading the new one.

## Deliberate boundaries

This layer now provides registry-based installation, Ed25519 publisher
authentication, dependency resolution and tree verification. It does
not yet provide key revocation/expiry, registry anti-rollback or
freshness policy, automatic updates, rollback channels, a registry UI,
per-profile sandbox policy, multi-tenant quotas or task
checkpoint/resume. Workspace checkpoints restore Git state (worktree,
index, HEAD/refs and stash), not agent memory, running tasks or
conversation state. Those need separate security and lifecycle work.
The current contract remains inspectable composition with the existing
trust and permission model, plus bounded process recovery when a plugin
explicitly opts in and Git-backed workspace rollback inside a worktree.
