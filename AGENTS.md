# CC-Connect Development Guide

## Project Overview

CC-Connect is a bridge that connects AI coding agents (Claude Code, Codex, Gemini CLI, Cursor, etc.) with messaging platforms (Feishu/Lark, Telegram, Discord, Slack, DingTalk, WeChat Work, QQ, LINE). Users interact with their coding agent through their preferred messaging app.

## Architecture

```
┌─────────────────────────────────────────────────┐
│                   cmd/cc-connect                │  ← entry point, CLI, daemon
├─────────────────────────────────────────────────┤
│                     config/                     │  ← TOML config parsing
├─────────────────────────────────────────────────┤
│                      core/                      │  ← engine, interfaces, i18n,
│                                                 │     cards, sessions, registry
├──────────────────────┬──────────────────────────┤
│     agent/           │      platform/           │
│  ├── claudecode/     │  ├── feishu/             │
│  ├── codex/          │  ├── telegram/           │
│  ├── cursor/         │  ├── discord/            │
│  ├── gemini/         │  ├── slack/              │
│  ├── iflow/          │  ├── dingtalk/           │
│  ├── opencode/       │  ├── wecom/              │
│  ├── acp/            │  ├── qq/                 │
│  └── qoder/          │  ├── qqbot/              │
│                      │  ├── line/               │
│                      │  ├── weibo/              │
│                      │  └── cloud-web/          │
├──────────────────────┴──────────────────────────┤
│                     daemon/                     │  ← systemd/launchd service
└─────────────────────────────────────────────────┘
```

### Key Design Principles

**`core/` is the nucleus.** It defines all interfaces (`Platform`, `Agent`, `AgentSession`, etc.) and contains the `Engine` that orchestrates message flow. The core package must **never** import from `agent/` or `platform/`.

**Plugin architecture via registries.** Agents and platforms register themselves through `core.RegisterAgent()` and `core.RegisterPlatform()` in their `init()` functions. The engine creates instances via `core.CreateAgent()` / `core.CreatePlatform()` using string names from config.

**Dependency direction:**
```
cmd/ → config/, core/, agent/*, platform/*
agent/*   → core/   (never other agents or platforms)
platform/* → core/  (never other platforms or agents)
core/     → stdlib only (never agent/ or platform/)
```

### Core Interfaces

- **`Platform`** — messaging platform adapter (Start, Reply, Send, Stop)
- **`Agent`** — AI coding agent adapter (StartSession, ListSessions, Stop)
- **`AgentSession`** — a running bidirectional session (Send, RespondPermission, Events)
- **`Engine`** — the central orchestrator that routes messages between platforms and agents

Optional capability interfaces (implement only when needed):
- `CardSender` — rich card messages
- `InlineButtonSender` — inline keyboard buttons
- `ProviderSwitcher` — multi-model switching
- `DoctorChecker` — agent-specific health checks
- `AgentDoctorInfo` — CLI binary metadata for diagnostics

## Development Rules

### 1. No Hardcoding Platform or Agent Names in Core

The `core/` package must remain agnostic. Never write `if p.Name() == "feishu"` or `CreateAgent("claudecode", ...)` in core. Use interfaces and capability checks instead:

```go
// BAD — hardcodes platform knowledge in core
if p.Name() == "feishu" && supportsCards(p) {

// GOOD — capability-based check
if supportsCards(p) {
```

```go
// BAD — hardcodes agent type
agent, _ := CreateAgent("claudecode", opts)

// GOOD — derives from current agent
agent, _ := CreateAgent(e.agent.Name(), opts)
```

### 2. Prefer Interfaces Over Type Switches

When behavior differs across platforms/agents, define an optional interface in core and let implementations opt in:

```go
// In core/
type AgentDoctorInfo interface {
    CLIBinaryName() string
    CLIDisplayName() string
}

// In agent/claudecode/
func (a *Agent) CLIBinaryName() string  { return "claude" }
func (a *Agent) CLIDisplayName() string { return "Claude" }

// In core/ — query via interface, fallback gracefully
if info, ok := agent.(AgentDoctorInfo); ok {
    bin = info.CLIBinaryName()
}
```

### 3. Configuration Over Code

- Features that may vary per deployment should be configurable in `config.toml`
- Use `map[string]any` options for agent/platform factories to stay flexible
- Add new config fields with sensible defaults so existing configs don't break

### 4. High Cohesion, Low Coupling

- Each `agent/X/` package is self-contained: it handles process lifecycle, output parsing, and session management for agent X
- Each `platform/X/` package is self-contained: it handles API connection, message receiving/sending, and card rendering for platform X
- Cross-cutting concerns (i18n, cards, streaming, rate limiting) live in `core/`

### 5. Error Handling

- Always wrap errors with context: `fmt.Errorf("feishu: reply card: %w", err)`
- Never silently swallow errors; at minimum log them with `slog.Error` / `slog.Warn`
- Use `slog` (structured logging) consistently; never `log.Printf` or `fmt.Printf` for runtime logs
- Redact tokens/secrets in error messages using `core.RedactToken()`

### 6. Concurrency Safety

- Agent sessions are accessed from multiple goroutines; protect shared state with `sync.Mutex` or `atomic` types
- Use `context.Context` for cancellation propagation
- Channels should have clear ownership; document who closes them
- Prefer `sync.Once` for one-time teardown (`pendingPermission.resolve()`)

### 7. i18n

All user-facing strings must go through `core/i18n.go`:
- Define a `MsgKey` constant
- Add translations for all supported languages (EN, ZH, ZH-TW, JA, ES)
- Use `e.i18n.T(MsgKey)` or `e.i18n.Tf(MsgKey, args...)`

## Code Style

- Follow standard Go conventions (`gofmt`, `go vet`)
- Use `strings.EqualFold` for case-insensitive comparisons
- Avoid `init()` for anything other than platform/agent registration
- Keep functions focused; extract helpers when a function exceeds ~80 lines
- Naming: `New()` for constructors, `Get/Set` for accessors, avoid stuttering (`feishu.FeishuPlatform` → `feishu.Platform`)

## Testing

### Requirements

- All new features must include unit tests.
- **All bug fixes MUST include a regression test in the same PR.** A bug
  fix PR without a test that fails on the pre-fix code and passes on the
  fixed code will not be merged. Name regression tests so the bug is
  searchable later, e.g. `TestSwitchToAgentSession_PreservesHistory` for
  the cmdSwitch history-loss bug.
- Tests must pass before committing: `go test ./...`.
- Changes that touch a Critical User Journey (CUJ) — see
  `core/cuj_test.go` — should explicitly run `go test ./core/ -run TestCUJ`
  before opening the PR.

### Running Tests

```bash
# Full test suite
go test ./...

# Specific package
go test ./core/ -v

# Run specific test
go test ./core/ -run TestHandlePendingPermission -v

# Run Critical User Journey tests (recommended for any core/engine.go or
# core/session.go change)
go test ./core/ -run TestCUJ -v

# With race detector (CI)
go test -race ./...
```

### Test Patterns

- Use stub types for `Platform` and `Agent` in core tests (see `core/engine_test.go`).
- Test card rendering by inspecting the returned `*Card` struct, not JSON.
- For agent session tests, simulate event streams via channels.
- **For multi-step user behavior, add a CUJ test in `core/cuj_test.go`.**
  CUJ tests assert what a USER sees on the platform side across multiple
  actions (e.g. "create s1 → chat → /new s2 → /switch s1 → /history
  must show s1's content"). They exist because per-function unit tests
  can all pass while a user journey is still broken — the `/switch
  loses history` bug shipped in exactly that scenario despite full
  unit coverage of every individual function involved.

### Critical User Journeys (CUJ)

A CUJ test is a USER-perspective end-to-end scenario, not a developer-
perspective unit test. The current inventory of CUJs and their coverage
status lives in:

`CUJ-INVENTORY.md` in the internal QA workspace (the registered authoritative
copy; it is not kept in this repository and must not be linked from here).

Rules for adding/updating CUJ tests in `core/cuj_test.go`:

1. Name: `TestCUJ_<group><id>_<short_camel_case>` (e.g. `TestCUJ_B3_SwitchPreservesHistory`).
2. Use real `SessionManager` + real `Engine`; mock only external boundaries (`Platform` sender, `Agent` process).
3. Drive the engine via `ReceiveMessage` — the same entrypoint platforms use, so engine/platform wiring is also covered.
4. Assert what the USER sees via `p.getSent()`, not internal struct fields.
5. ≥3 user actions per CUJ. A single-action assertion belongs in a unit test, not a CUJ.

When a user-reported bug maps to an existing CUJ, add a sub-case to that
CUJ rather than creating a new one.

## Selective Compilation

Each agent and platform is imported via a separate `plugin_*.go` file with a
build tag (e.g. `//go:build !no_feishu`). By default **all** agents and
platforms are compiled in.

### Include only specific agents/platforms

```bash
# Only Claude Code agent + Feishu and Telegram platforms
make build AGENTS=claudecode PLATFORMS_INCLUDE=feishu,telegram

# Multiple agents
make build AGENTS=claudecode,codex PLATFORMS_INCLUDE=feishu,telegram,discord
```

### Exclude specific agents/platforms

```bash
# Exclude some platforms you don't need
make build EXCLUDE=discord,dingtalk,qq,qqbot,line
```

### Direct build tag usage (without Make)

```bash
go build -tags 'no_discord no_dingtalk no_qq no_qqbot no_line' ./cmd/cc-connect
```

Available tags: `no_acp`, `no_claudecode`, `no_codex`, `no_copilot`, `no_cursor`, `no_gemini`,
`no_iflow`, `no_opencode`, `no_qoder`, `no_feishu`, `no_telegram`,
`no_discord`, `no_slack`, `no_dingtalk`, `no_wecom`, `no_weixin`, `no_qq`, `no_qqbot`,
`no_line`, `no_weibo`.

## Public-fork hygiene

This repository is public ([`utensil/cc-connect`](https://github.com/utensil/cc-connect),
a fork of `chenhg5/cc-connect`). Everything written here is world-readable
forever: PR titles and bodies, issue bodies, review and issue comments, commit
messages (**including the squash-merge message GitHub generates from the PR**),
branch names, file names, file contents, test fixtures, committed images and
screenshots, CHANGELOG and release notes, and CI logs.

Treat identifiers taken from real deployments as private, not just credentials:

| Category | Never publish | Publish instead |
| --- | --- | --- |
| Credentials | API keys, bot tokens, `*.env` contents, auth files | nothing, ever |
| Account identity | bot emails (`*-bot@…`), realm/host URLs, bot display names, personal emails | "the bot account", "an internal Zulip realm" |
| Channels and topics | `channel > topic`, `zulip:stream:<stream>:<topic>`, DM addresses | "an internal Zulip topic", "a DM" |
| Session identifiers | agent session ids and their prefixes (`01a2b3c4`), cc-connect session labels (`s123`), message ids | `<session-id>`, `<message-id>` |
| Platform ids | Discord guild/channel/user/message ids, DingTalk/Feishu ids seen in production | synthetic values or `<id>` |
| Host paths | `/Users/<name>/…`, a person's own dirs under `~/` (`~/projects/…`), a deployment's env or credential file (`<env-dir>/<account>.env`) | `t.TempDir()`, `/path/to/workdir`, `<env-file>` (the product's own documented defaults, e.g. `~/.cc-connect/config.toml`, are fine) |
| Devices and network | tailnet addresses, device hostnames, internal service ports | "the host", "the service" |
| Internal projects | unpublished project or workspace names, private repository names, plan titles | "an internal project" |

Rules:

- **Keep the evidence, drop the identifier.**
  `session: invalidating stale agent session session=<id> old_agent=grok new_agent=codex`
  is as convincing as the original log line.
- **A synthetic-looking value is not safe.** Rollout files
  (`rollout-…-<uuid>.jsonl`), `session=…` labels, rendered mentions
  (`@**Name|1234567**`) and quoted message ids all carry deployment
  identifiers.
- **Scrub before pushing, not after.** A later redaction commit leaves the
  original in the diff, in every clone, and in the squash-merge message.
- **Squash-merge messages come from the PR title and body.** Fix the source;
  do not hand-edit a merge commit on `dev` to repair a leak — that rewrites
  public history and breaks every recorded SHA.
- **Fixtures use synthetic values** (`t.TempDir()`, `"demo"`, `"1234567890"`),
  never production ids.
- **If a leak is already on `dev`**, correct the text in the next PR (a one-line
  scrub commit only when a live secret is exposed) and record the residue —
  including immutable history you chose not to rewrite — in that PR.

## Fork feature ledger

`FORK_FEATURES.md` is the fork's ledger and has no upstream counterpart. A PR
that changes user-visible fork behavior **must update it in the same PR**:

- New behavior → add an `###` entry: what changed, why it was missing or broke,
  the tests that pin it, any residual risk, and the PR/merge reference. If the
  behavior extends an accumulating section (Zulip, Grok, Pi, session-scoped
  switching), add a dated paragraph there instead — those sections collect one
  change per paragraph by design.
- Changed behavior → replace the superseded paragraph and say in one line what
  it replaced, so the file keeps the record of when behavior changed.
- Exempt: pure chores, refactors with no behavior change, documentation-only
  changes, and test-only changes. Say so in the PR body
  (`no ledger change: chore|refactor|docs|test`) so the omission is visible
  rather than assumed.
- After merge, a small docs commit on `dev` links the merge commit, following
  the entries that carry `- Fork PR:` and `- Merge record:` lines.

Both files are public: sanitise an incident in the ledger exactly as in a PR
body (see *Public-fork hygiene*).

## Pre-Commit Checklist

1. **Build passes**: `go build ./...`
2. **Tests pass**: `go test ./...`
3. **CUJ tests pass** (for any change in `core/engine.go`, `core/session.go`, `core/cron.go`, `core/timer.go`, or command handlers): `go test ./core/ -run TestCUJ`
4. **Bug fix has a regression test**: a new test in this PR that fails on the pre-fix code and passes on the fix.
5. **No new hardcoded platform/agent names in core**: grep for platform names in `core/*.go`.
6. **i18n complete**: all new user-facing strings have translations for all languages.
7. **No secrets in code**: no API keys, tokens, or credentials in source files.
8. **No private identifiers** (see *Public-fork hygiene*): title, body, comments, commit message, and any fixture you add or modify are free of private host paths, bot/realm identities, channel and topic names, session and message ids, and platform ids.
9. **Fork ledger updated** (`FORK_FEATURES.md`) when this PR changes user-visible fork behavior, or the PR body states why it is exempt.

## Adding a New Platform

1. Create `platform/newplatform/newplatform.go`
2. Implement `core.Platform` interface (and optional interfaces as needed)
3. Register in `init()`: `core.RegisterPlatform("newplatform", factory)`
4. Create `cmd/cc-connect/plugin_platform_newplatform.go` with `//go:build !no_newplatform` tag
5. Add `newplatform` to `ALL_PLATFORMS` in `Makefile`
6. Add config example in `config.example.toml`
7. Add unit tests

## Adding a New Agent

1. Create `agent/newagent/newagent.go`
2. Implement `core.Agent` and `core.AgentSession` interfaces
3. Register in `init()`: `core.RegisterAgent("newagent", factory)`
4. Create `cmd/cc-connect/plugin_agent_newagent.go` with `//go:build !no_newagent` tag
5. Add `newagent` to `ALL_AGENTS` in `Makefile`
6. Optionally implement `AgentDoctorInfo` for `cc-connect doctor` support
7. Add config example in `config.example.toml`
8. Add unit tests
