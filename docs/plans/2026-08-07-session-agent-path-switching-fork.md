# Session Agent/Path Switching Design (fork adaptation)

**Date:** 2026-08-07
**Status:** Implemented on `dev`
**Origin:** upstream PR [chenhg5/cc-connect#193](https://github.com/chenhg5/cc-connect/pull/193)
(Ginhzh, maintainer-approved) + fork delta: `pi` allowlist + per-agent options.

## Problem

A cc-connect project is bound to one agent type. A codex project on one
Discord bot wanted the same bot to switch between codex and pi without a
second project or a second bot token. Upstream had no merged mechanism:
`/provider`/`/model`/`/mode`/`/reasoning` all switch *within* an agent.

## Design

### Session-scoped overrides

Two persisted fields on `core.Session` (JSON `agent_override`,
`work_dir_override`), mirroring the fork's existing `model_override` and
`active_provider`:

- `AgentOverride string` — per-session agent type chosen via `/agent`.
- `WorkDirOverride string` — per-session work dir chosen via `/path`.

They are written by `/agent`/`/path`, survive process restarts (snapshot
serialization), and are restored by `/switch` because they live on the
Session row.

### Single chokepoint injection

The fork's `getOrCreateInteractiveStateWith` is the one place a session's
agent is spawned. After the base agent is selected (project agent or
workspace agent), `buildSessionAgentFrom(baseAgent, session)` applies the
overrides:

1. No override → return base agent unchanged (zero-cost for existing setups).
2. Override present → derive effective agent type, clone project options,
   drop the `agents` namespace, overlay per-type options
   (`options.agents.<type>`), set work_dir, then `core.CreateAgent(type,
   opts)`. Providers are carried from the base agent when the new agent is a
   `ProviderSwitcher`.

This is deliberately smaller than PR #193, which converted ~30 command call
sites to session-aware contexts (`commandSessionContext`,
`sessionAgentContextForKey`, `switchToAgentSession`). Those conversions
conflict with fork evolution (steering, reasoning, quiet display, provider
switching); the fork already restores session-level model/provider at session
start, so only the spawn point needed the hook.

### Fork delta 1 — pi in the allowlist

PR #193 hard-codes `case "codex", "claudecode"`. The fork extends it to
`codex | claudecode | pi` so codex↔pi switching works. The allowlist lives in
`allowedAgentTypes()`/`isAllowedAgentType()`.

### Fork delta 2 — per-agent-type options

Cross-type switching makes the project's single `[projects.agent.options]`
block leak type-specific keys (`codex_home`, `app_server_url`, a codex model)
into the switched agent. `[projects.agent.options.agents.<type>]` provides a
per-backend options map merged over the project defaults; unknown keys are
ignored by adapters that do not read them.

## Commands

- `/agent` — status (current agent, current path, default agent, default path)
- `/agent <codex|claude|claudecode|pi|reset> [absolute-path]` — switch
- `/path [absolute-path|reset]` — view/switch session work dir
- Switching clears the agent session ID + history; the next turn starts fresh
  on the new agent in the new work dir.

## Non-goals

- Dynamic per-message agent routing.
- Arbitrary agent aliases (only the allowlist).
- Multi-workspace interplay beyond the existing workspace agent flow.
