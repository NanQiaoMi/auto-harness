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
  "capabilities": ["tools", "hooks"]
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

Earlier discovery roots win duplicate names. That keeps the user's
plugin ahead of a project plugin trying to replace the same name.

Built-in Go extensions and `config.json`'s `extension_command` are base
runtime, not catalog plugins: the profile allowlist applies to plugins
found in the user, project and `plugin_dirs` discovery roots.

## Assembly order

The order is deliberate:

1. Load config and resolve the profile.
2. Apply profile, then CLI overrides.
3. Build the model registry and session.
4. Load trusted built-ins, legacy/config extensions and selected plugins.
5. Resolve project trust, then load trusted project resources.
6. Register built-in tools and the permission guard.
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

## Visibility

```text
/profile    active profile, source, selections, active tools and warnings
/plugins    discovered plugins, versions, paths, selection and load state
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

This layer does not yet provide remote plugin installation, signatures,
a registry UI, plugin dependency resolution, automatic supervisor
restart, per-profile sandbox policy, or multi-tenant quotas. Those need
separate security and lifecycle work. The current contract is local,
inspectable composition with the existing trust and permission model.
