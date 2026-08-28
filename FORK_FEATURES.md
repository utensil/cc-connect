# Fork features

This is the source of truth for behavior carried by `utensil/cc-connect` that
differs from the real upstream,
[`chenhg5/cc-connect`](https://github.com/chenhg5/cc-connect). Keep this
ledger current whenever fork-specific behavior is added, removed, or
superseded there.

## Branch policy

- `dev` is this fork's default and product branch.
- `main` is reserved for clean upstream synchronization and must not receive
  fork-only changes.
- The README contains only a short pointer to this file. Keeping the feature
  details here makes upstream README refreshes a small, isolated merge.

## Feature ledger

### Discord reply routing and acknowledgement reactions

Replied `/steer` commands are routed from the user's raw message text. The
quoted message remains agent context through `ExtraContent`. When configured
with `ack_style = "reaction"`, successful steering and accepted busy-message
queueing react to the original Discord message, with localized text as the
fallback if a reaction cannot be added.

- Fork commits: [33145614](https://github.com/utensil/cc-connect/commit/33145614)
  and [bf8d3a5f](https://github.com/utensil/cc-connect/commit/bf8d3a5f), both
  authored by Miro (`@utiberious`).
- Related-fork reference: [`utiberious` a89c8619](https://github.com/utiberious/cc-connect/commit/a89c8619a5d8c1b853f4945c4d4b6eeab42e8246),
  authored by Ulysses Tiberious, has the same combined behavior on that
  fork's `main`.
- Merge status: not cherry-picked. The Discord implementation and docs in
  `dev` already cover it, so merging it would duplicate/conflict with the
  fork's changes.

### Default busy-message steering

Set `queue.busy_behavior = "steer"` to inject a plain-text follow-up into the
active steer-capable agent turn by default. Messages with attachments,
locations, or a session still starting continue to queue safely. If the
backend rejects the steer, the original follow-up falls back to the durable
FIFO queue; explicit `/steer` remains available and reports its failure.

- Fork merge commit: [c0c76ab7](https://github.com/utensil/cc-connect/commit/c0c76ab7).
- Merge record: [PR #1](https://github.com/utensil/cc-connect/pull/1).

### Pi RPC same-turn steering

Pi sessions in persistent RPC mode implement `SessionSteerer` with Pi's native
`{"type":"steer","message":"..."}` command. The command is queued by Pi
after the current turn's tool calls and before the next model call, so it does
not create a second session or turn. One-shot JSON-mode Pi sessions report
steering as unsupported. Enable `rpc = true` for any Pi agent used with
`queue.busy_behavior = "steer"` or `/steer`.

- Fork commits: [fc905d32](https://github.com/utensil/cc-connect/commit/fc905d32)
  (Pi RPC transport) and [213c8d81](https://github.com/utensil/cc-connect/commit/213c8d81)
  (busy-steer fallback and CUJ coverage), currently on the feature branch.
- Upstream status: no Pi steering implementation was found in the canonical
  repository; generic steering API work remains in competing upstream PRs.

### Pi turn cancellation and output policy

Pi RPC prompts always carry Pi's required `streamingBehavior = "followUp"`;
`/stop` sends the native RPC `abort` and serializes follow-up admission so the
next prompt cannot race the cancellation. JSON-mode cancellation synchronously
reaps the one-shot process before the engine falls back to session cleanup.
Configure `stream_preview.disabled_agents = ["pi"]` when Pi replies should be
delivered only as final messages: this disables preview edits, compact progress
edits, and streaming-card updates without an agent-name branch in the engine.
Pi's session adapter separately compacts runs of blank lines at the response
boundary, preserving normal Markdown paragraph breaks and history continuity.

- Fork commit: [2605d4e4](https://github.com/utensil/cc-connect/commit/2605d4e4) on `feat/agent-switch-pi` (PR #10).
- Deployment note: enable the documented setting in each environment that uses
  Pi with busy-message steering; keep the session store intact during rollout.

### Pi deployment and session-recovery lesson

The Pi transcript on disk and cc-connect's `AgentSessionID` are the continuity
boundary. A deployment must retain both: do not use `/cancel`, `/new`, or delete
the Pi session JSONL when recovering a busy session. `/stop` is the preserving
operation; after the service is idle, one guarded follow-up is safe, but the
old JSON one-shot transport can still race process reaping.

For a reliable rollout, keep the existing per-agent settings and add
`rpc = true` under `[projects.agent.options.agents.pi]`. Restart the service
once after installing the fork binary so the persistent RPC process resumes the
same session ID. Keep the independent output policy
`[stream_preview] disabled_agents = ["pi"]` if Pi should emit only final
messages; response-boundary newline normalization handles excess blank
paragraphs separately. Verify a normal continuation, a busy follow-up, and a
stop-then-follow-up sequence all retain one Pi session.

One-shot launchd helpers must capture the daemon result in a non-reserved zsh
variable such as `exit_code` (not `status`); verify the service restart and
helper cleanup independently so a post-restart cleanup error cannot be mistaken
for a failed deployment.

### Unicode-aware command parsing

Commands split arguments on Unicode whitespace, so pasted Discord text and
non-ASCII spaces behave like ordinary spaces.

- Fork merge commit: [24d80ec3](https://github.com/utensil/cc-connect/commit/24d80ec3).
- Merge record: [PR #2](https://github.com/utensil/cc-connect/pull/2).

### Codex GPT-5.6 discovery and reasoning levels

Codex exposes the GPT-5.6 reasoning levels, including `max` and `ultra`, and
renders the complete supported set in command and card UI. Model choices are
discovered dynamically so the connector follows the installed Codex CLI.

- Feature commits: [7c508cfc](https://github.com/utensil/cc-connect/commit/7c508cfc),
  [6197d4bb](https://github.com/utensil/cc-connect/commit/6197d4bb),
  [e46085e6](https://github.com/utensil/cc-connect/commit/e46085e6), and
  [ea11fa82](https://github.com/utensil/cc-connect/commit/ea11fa82).
- Source attribution: the GPT-5.6 commits were authored by AaronZ345; the
  dynamic-discovery follow-up was authored by cg33.
- Merge record: [PR #3](https://github.com/utensil/cc-connect/pull/3), merged
  as [0499c01e](https://github.com/utensil/cc-connect/commit/0499c01e).

### Preserve Codex sessions when changing reasoning

Changing reasoning effort keeps the same Codex conversation when it is live
or resumable, retains the chosen effort through app-server resume, and keeps
the workspace indicator in sync. Live effort changes are serialized with
interactive-state cleanup to avoid races.

- Fork commits authored by Miro (`@utiberious`):
  [357f7c83](https://github.com/utensil/cc-connect/commit/357f7c83),
  [70ae260b](https://github.com/utensil/cc-connect/commit/70ae260b),
  [a4b39043](https://github.com/utensil/cc-connect/commit/a4b39043),
  [5f3c9bfe](https://github.com/utensil/cc-connect/commit/5f3c9bfe), and
  [0499c01e](https://github.com/utensil/cc-connect/commit/0499c01e).
- Merge record: [PR #3](https://github.com/utensil/cc-connect/pull/3).

### Preserve live Codex sessions when changing model

`/model` and model-card changes use Codex app-server's
`thread/settings/update` on the existing thread. This changes the model
without closing the session or losing its conversation context. Agents that do
not support live model switching retain the previous restart-and-resume
fallback.

- Fork commit: [7bcf7761](https://github.com/utensil/cc-connect/commit/7bcf7761).
- Merge record: [PR #4](https://github.com/utensil/cc-connect/pull/4), merged
  as [61310b27](https://github.com/utensil/cc-connect/commit/61310b27).

### Session-scoped Codex model selection and durable resume

Bare `/model <model>` changes only the current session. Use
`/model default <model>` to choose the default for new sessions; `session`
and `switch` remain explicit current-session aliases. The selected model is
stored with that session. On resume, the connector reapplies it through
`thread/settings/update`, because `thread/resume` can return the historical
model even when given an override. The existing thread ID and context remain
intact.

- Fork commit: [cfad9612](https://github.com/utensil/cc-connect/commit/cfad9612).
- Merge record: [PR #6](https://github.com/utensil/cc-connect/pull/6).

### Codex selected-model capacity recovery

For the exact upstream diagnostic `Selected model is at capacity. Please try a
different model.`, the Codex app-server session stays alive and retries with
capped exponential delays of 1s, 2s, 4s, 8s, 16s, then 30s until the backend
accepts a turn or the session is closed. An initial `turn/start` rejection
retries its already-staged input. A capacity failure reported after a turn has
completed starts a same-thread continuation instead, so completed tools and
other side effects are not replayed. Other errors retain their existing error
path.

- Fork commit landed on `dev`: [43a3f65b](https://github.com/utensil/cc-connect/commit/43a3f65b).
- Merge record: [PR #8](https://github.com/utensil/cc-connect/pull/8), merged as
  [1701d6e2](https://github.com/utensil/cc-connect/commit/1701d6e2).
- Provenance: not present upstream; reimplemented in this fork after the
  provider's selected-model capacity response was observed in production.

## Maintenance rules

For every new fork feature, add a short behavior description, the commit that
landed on `dev`, and its PR. If it comes from a real-upstream PR or commit,
link the `chenhg5/cc-connect` source and say whether it was merged,
reimplemented, or intentionally left unmerged. Record related-fork commits
separately. If the real upstream adopts a fork feature, update this entry with
the upstream link before removing any fork delta.

### Session-scoped /agent and /path switching

`/agent <codex|claude|claudecode|pi|reset> [absolute-path]` switches the
current session's agent type; `/path <absolute-path|reset>` switches the
current session's work dir. Both are per-session overrides persisted on the
`Session` row (`agent_override`, `work_dir_override`) and are restored by
`/switch`, so each Discord thread keeps its own agent/path choice under
`thread_isolation`. Switching clears the agent session ID and history so the
next turn starts fresh on the new agent.

- Ported from upstream PR
  [chenhg5/cc-connect#193](https://github.com/chenhg5/cc-connect/pull/193)
  (Ginhzh, maintainer-approved), adapted to this fork's engine:
  - Injection point is `getOrCreateInteractiveStateWith` — the single place a
    session's agent is spawned — instead of PR #193's 30-call-site context
    refactor, so fork features (steering, reasoning, quiet display, provider
    switching) are untouched.
  - Session-agent-aware commands (fork commit 8be784c): `/model`, `/mode`,
    `/reasoning`, `/provider`, `/current`, `/status` resolve the session's
    effective agent via `sessionAgentFor` instead of `e.agent`, so after
    `/agent pi` they operate on pi (e.g. `/model` lists deepseek models), not
    the project's codex agent.
  - **`pi` added to the `/agent` allowlist** (upstream PR allows only
    `codex`/`claudecode`). This is the fork-specific delta that enables
    codex↔pi switching on one Discord bot.
  - **Per-agent-type options**: `[projects.agent.options.agents.<type>]` lets
    each switchable backend declare its own `model`/`mode`/`work_dir` etc.
    instead of inheriting the project's single options block, so a codex
    project's `codex_home`/`app_server_url`/model do not leak into a switched
    pi agent and vice versa.
- Upstream issue:
  [chenhg5/cc-connect#703](https://github.com/chenhg5/cc-connect/issues/703)
  ("Multi-Agent Switching Within the Same Project").
- Fork PR: (this feature's PR) — see the pull request that landed this entry.

### Combined `/agent <type> [path] [model] [reasoning]`

`/agent` accepts optional trailing `[model] [reasoning]` args so agent, model,
and reasoning switch in one command: `/agent <codex|claudecode|pi> [path]
[model] [reasoning]`. Arg[1] is treated as an absolute path when it starts
with `/` or `~`; otherwise it is a model name (bare form), so both
`/agent pi /path model reasoning` and `/agent pi model reasoning` work.

- Model and reasoning are validated against the *target* agent (built from
  the requested type) BEFORE anything is applied; invalid input rejects the
  whole command — the session's agent/path/model overrides stay untouched.
- On success, `agent_override`, optional `work_dir_override`, and optional
  `model_override` + reasoning effort are all applied to the session row
  (persist across `/switch` under `thread_isolation`).
- Fork commits: this PR (core/engine.go `cmdAgent` + `isAbsolutePathArg`;
  i18n keys `agent_changed_with_model`, `agent_changed_with_model_reasoning`).
- Provenance: not present upstream; written in this fork (2026-08-08).
- Related: `Combined /model <model> <reasoning>` entry above — `/agent` now
  subsumes it for the switch case; `/model` remains for model-only changes.

### Combined `/model <model> <reasoning>`

`/model` accepts an optional trailing reasoning effort so model + reasoning
switch in one command: `/model <model> <reasoning>`, `/model session <model>
<reasoning>`, `/model default <model> <reasoning>`. The reasoning effort is
validated against the session agent's `AvailableReasoningEfforts()` before
anything is applied; if invalid, the whole command is rejected (model
unchanged). Both changes are per-session overrides like the plain `/model`.

- Fork commits: this PR (core/engine.go `parseModelSwitchArgs` +
  `applyReasoningEffort`/`resolveReasoningTarget` helpers + i18n keys
  `model_changed_with_reasoning`, `model_session_changed_with_reasoning`).
- Refactor: `cmdReasoning` now shares `applyReasoningEffort` /
  `resolveReasoningTarget` with `cmdModel` — one validation + application
  path for reasoning effort.
- Provenance: not present upstream; written in this fork (2026-08-08).
- Related: `/agent`/`/path` session switching entry above — this extends the
  same session-scoped switching surface so `/agent pi` + `/model <model>
  <reasoning>` sets agent, model, and reasoning in two commands instead of
  three.

### Pre-existing full-suite test flakiness on macOS

`go test -race ./core` (full suite) intermittently fails
`TestCUJ_A3_ImageReachesAgent` / `TestCUJ_A5_FileReachesAgent` with
`TempDir RemoveAll cleanup: directory not empty` on macOS. Reproduces on clean
`origin/dev` (fc22b8b) — unrelated to /agent switching. Run the failing tests
in isolation (`-run 'TestCUJ'`) to confirm green.

### `[projects.display]` applied at engine startup

Upstream applies the project display config (`mode`, `thinking_messages`,
`tool_messages`, …) only in the config-reload path. A fresh daemon start
kept the engine default (full mode, thinking shown, cards edited), so a
project with `mode = "quiet"` + `thinking_messages = false` still streamed
thinking into an edited Discord message — notably for pi agents after an
`/agent` switch — until a `/config` reload happened.

The fork applies `SetDisplayConfig` from `config.EffectiveDisplay` at engine
creation in `cmd/cc-connect/main.go`, mirroring the reload path.

- Fork commit: [7c78177](https://github.com/utensil/cc-connect/commit/7c78177).
- Provenance: not present upstream; written in this fork (2026-08-07).
- Related: `/agent`/`/path` session switching entry above — this makes the
  session-effective display behave correctly for switched agents.
