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

### Zulip platform (fork-only) and reaction acknowledgements

`platform/zulip` is a **fork-only** platform: it does not exist in the canonical
repository (`chenhg5/cc-connect`), so it must be carried across every rebase. It
uses Zulip's REST event API (`POST /register` plus a long-poll `GET /events`),
replies with `POST /messages`, edits streaming previews with `PATCH
/messages/{id}`, and scopes sessions by topic (`zulip:stream:<stream>:<topic>`
with `thread_isolation = true`; DMs are `zulip:dm:<email>`). `chatmode =
"oncall"` (mention-only) is the default, `dm_policy` gates DMs, and
`ack_reaction` reacts to each inbound message with a Zulip emoji name.

Steering and busy-message acknowledgements are delivered as **reactions**,
matching the Discord behaviour: the platform implements
`core.MessageAcknowledger`, so a steered or queued follow-up reacts on the
user's own message instead of posting the localized
"Guidance sent to the current task." text. Because Zulip has no typing
indicator, `ack_style = "reaction"` is the platform's default; the text reply
remains the fallback when a reaction cannot be added (invalid emoji name, stale
or missing message id, HTTP error). Emoji values are Zulip emoji **names** (sent
as `emoji_name`), not unicode: `steer_ack_emoji = "compass"` and
`queue_ack_emoji = "hourglass"` are the defaults, both verified valid against
the live realm (`hourglass_flowing_sand` and `white_check_mark` do not exist
there).

The same change closes a message-id gap: `processMessage` now sets
`core.Message.MessageID` from the Zulip message id (previously empty, so the
engine logged `msg_id=""` for every Zulip message) and `replyContext` carries
that id for the ack path.

Operational precondition (Zulip-side, not code): the bot only receives events
for streams it is **subscribed** to, and it cannot self-subscribe to a private
channel (`Unable to access channel`); a realm admin must add it. While it was
not a member, reactions and replies were rejected and previously failed
silently, because the platform logs send/reaction errors at debug level.

Event-queue recovery (2026-09-21): Zulip garbage-collects idle event queues, and
the poll loop used to retry a rejected queue forever because it searched the
error *text* for the machine code `BAD_EVENT_QUEUE_ID` while `apiCallRaw` only
embedded Zulip's human-readable `msg`. A quiet realm therefore deafened the
Zulip side until a daemon restart. `apiCallRaw` now returns a typed
`zulipAPIError` carrying `code`, `isQueueGone` recognises the code (with the
text as fallback), the loop re-registers with a one-second settle, and three
consecutive `/events` failures are escalated to an ERROR line so an unresponsive
Zulip connection cannot sit silently in the log.

- Pull request: [utensil/cc-connect#12](https://github.com/utensil/cc-connect/pull/12)
  (branch `fix/zulip-reaction-acks`; the ledger intentionally cites the PR and branch rather
  than commit hashes, which churn when the branch is rebased onto `dev`).
  `AcknowledgeMessage` is synchronous with a 2s bound because the engine calls it inline
  before the turn starts; an invalid `ack_style` is rejected at startup, mirroring Discord.
- Upstream status: absent from canonical `chenhg5/cc-connect` (`git ls-tree
  canonical/main platform/ | grep zulip` is empty); nothing to cherry-pick, keep
  the platform when syncing upstream.
- Config reference: `config.example.toml` now documents the platform, including
  the subscription precondition (before this change the platform had no example
  anywhere in the repository).

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

### Pi live model and thinking switching (session-scoped)

`/model <model>` and `/reasoning <level>` on a Pi session apply to the running
conversation instead of tearing it down. `piSession` implements
`core.LiveModelSwitcher` and `core.LiveReasoningEffortSwitcher` through Pi's
RPC commands `{"type":"set_model","provider":…,"modelId":…}` and
`{"type":"set_thinking_level","level":…}`. Replies are matched by request id,
so a switch is reported as applied only when Pi confirms it; an unanswered or
rejected switch returns false and the engine keeps its previous respawn
fallback. A bare model id (as used by `[projects.agent.options.agents.pi]
model` and `/model`) is resolved to Pi's provider+id pair through
`get_available_models`, cached per session; a `provider/id` string needs no
lookup.

The Pi agent also implements `core.SessionModelStarter` and
`core.SessionRuntimeStarter`, so a session's persisted model/reasoning override
is honoured when its process is (re)spawned. Before this, the override was
silently dropped at spawn (the engine fell through to a plain `StartSession`)
and a reasoning override made cron/per-session starts fail with "does not
support per-session reasoning". `PreservesSessionOnReasoningEffortChange`
returns true: a thinking-level change never needs a history wipe, because RPC
sessions apply it in place and one-shot json sessions resume the same Pi
session id with a new `--thinking` flag.

- Fork commit: [c30e33c8](https://github.com/utensil/cc-connect/commit/c30e33c8) on `feat/pi-live-model-switch`.
- Merge record: [PR #16](https://github.com/utensil/cc-connect/pull/16).
- Provenance: not present upstream, and not present in the related fork. Pi's
  RPC protocol ships `set_model`, `set_thinking_level`, and
  `get_available_models`, but no cc-connect agent used them; upstream's live
  model switching is Codex-only (`core.LiveModelSwitcher`).

### Agent stdout line bound and visible resume fallback

Agent stdout records are read with a shared bound (`core.AgentStreamMaxLineBytes`, 64 MiB) applied
by `core.NewAgentStreamScanner`, replacing the previous per-adapter 10 MiB buffers. Resuming a long
Codex thread emits records larger than the old bound: the read loop aborted with
`bufio.Scanner: token too long`, the session died, and the engine replaced the saved conversation
with an empty one. A record approaching the bound is logged once per stream so conversation growth
is visible before it becomes a failure.

When a saved conversation still cannot be resumed, the fallback is no longer silent. The engine
posts an i18n notice naming the abandoned session (`core.MsgResumeFailedFreshStarted`, and
`core.MsgResumeFailedTooLargeFreshStarted` when the cause is an oversized record), and the replaced
id stays recoverable through `Session.PastAgentSessionIDs` / `GetPastAgentSessionIDs`. Before this,
the swap appeared only as a WARN line in the log, so an agent answering without context looked like
it had forgotten the subject — or, in a shared workspace where another project's artifacts were
fresher, like it had answered about the wrong project.

- Files: `core/agent_stream.go`, `core/engine.go`, `core/i18n.go`, `core/session.go`,
  `agent/codex/appserver_session.go`, `agent/codex/context_usage.go`, `agent/codex/list.go`,
  `agent/gemini/session.go`, `agent/cursor/session.go`, `agent/claudecode/session.go`.
- Tests: `TestAppServerSession_ReadLoopAcceptsRecordLargerThanTheLegacyCap` (fails with the old
  10 MiB bound, passes with the shared one),
  `TestResumeFailureFallbackNotifiesUserAndKeepsFailedSessionID`,
  `TestScanContextUsageFromRollout_AcceptsRecordLargerThanLegacyCap` and
  `TestParseCodexSessionFile_AcceptsRecordLargerThanLegacyCap` (both fail with the previous codex
  caps: `bufio.Scanner: token too long`, and a session that vanishes from the listing).
- Related upstream issue: [chenhg5/cc-connect#189](https://github.com/chenhg5/cc-connect/issues/189)
  (single stdout line over the scanner limit aborts a session). The same 10 MiB bound and the silent
  fallback exist in the canonical repository, so this is a candidate upstream PR.
- Every line reader in the Codex package shares the bound, including the transcript readers on disk:
  a rollout record measured 2.09 MiB in the 2026-10-11 resume failure on the Zulip instance, so the
  previous 2 MiB context-usage cap and 256 KiB session-listing cap failed on exactly the threads this
  change is about.
- Branch commit: [d080848b](https://github.com/utensil/cc-connect/commit/d080848b) on
  `fix/codex-resume-line-cap`.
- Merge record: [PR #17](https://github.com/utensil/cc-connect/pull/17) — open, merge is
  human-gated.

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

`/agent <codex|claude|claudecode|pi|grok|reset> [absolute-path]` switches the
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

### Native Grok Build (`grok`) agent

One Discord bot can run xAI's Grok Build CLI (`https://x.ai/cli`) as a first-class
agent: `type = "grok"`, plus `/agent grok [path] [model] [reasoning]` for
session-scoped switching on a codex or pi project.

- Adapter: `agent/grok` drives ACP by default (`grok agent stdio`, one long-lived
  session per conversation) and falls back to headless mode
  (`grok -p|--prompt-file <prompt> --output-format streaming-json`) when
  `backend = "headless"`. Headless runs one process per turn with
  `--resume <sessionId>` (taken from the terminal `end` frame) for conversation
  continuity, `-m/--model`, `--reasoning-effort`, `--max-turns`, `--cwd`, and
  `--permission-mode` (+ `--always-approve` for every mode except `plan`,
  because headless turns have no TUI to click "allow").
- Event mapping follows the streaming-json schema published with the CLI
  (`thought`, `text`, `tool_call`, `tool_call_update`, `usage`, `plan`,
  `available_commands`, `end`, `error`). Token deltas are coalesced into single
  `EventThinking`/`EventText` payloads so a reply is not one IM bubble per token;
  `tool_call_update` is only surfaced for terminal `completed`/`failed` statuses
  and its `toolCallId` is resolved back to the tool name from its `tool_call`.
- Model catalog: `AvailableModels`/`AvailableReasoningEfforts` read the ACP
  `initialize` result of `grok agent stdio` (`_meta.modelState`), which reports
  ids, display names, context windows and the per-model reasoning efforts (and
  needs no login for the shape; the account's own catalog needs auth).
  `grok models` is the fallback and a static list the last resort; the probe is
  cached for 10 minutes.
- Default model: when no `model` is configured the adapter passes no `-m`, so
  each turn runs on whatever the CLI currently defaults to — the newest model
  (grok-4.7 at the time of writing, grok-4.6/4.5 also offered). `GetModel`
  reports that CLI default from the cached catalog, so `/model` and `/current`
  stay informative without pinning a version that rots. Verified that
  `--resume <sessionId> -m <other model>` is accepted, so a live `/model` switch
  keeps the conversation instead of restarting it.
- Live switching: the session implements `LiveModelSwitcher` and
  `LiveReasoningEffortSwitcher`, so `/model` and `/reasoning` change the next
  turn of the same conversation without clearing history (each turn is a new
  headless process, so nothing has to be replaced). The agent implements
  `SessionModelStarter`/`SessionRuntimeStarter` so a persisted override is
  applied at spawn, and `ReasoningEffortSessionPreserver`.
- Also implemented: `ModeSwitcher` (`/mode`: `default`, `accept_edits`, `yolo`,
  `plan`, `dont_ask`), `ProviderSwitcher` (`XAI_API_KEY`/`XAI_API_BASE_URL` from
  `[projects.agent.options.providers]`), `WorkDirSwitcher`, `SkillProvider`
  (`<work_dir>/.grok/skills`, `~/.grok/skills`), `MemoryFileProvider`
  (`AGENTS.md`), session listing/deletion under `~/.grok/sessions`.
- Fork delta on top of the ported adapter: `/agent` allowlist accepts `grok`
  (`allowedAgentTypes` + the `cmdAgent` switch), per-backend options work through
  the existing `[projects.agent.options.agents.grok]` block, the catalog probe,
  the documented-schema event mapping, and the live model/reasoning switches.
- Provenance: ported from the *unmerged* upstream PR
  [chenhg5/cc-connect#1558](https://github.com/chenhg5/cc-connect/pull/1558)
  (liyang480, `agent/grok` headless `-p` adapter). Upstream also carries the much
  larger unmerged
  [chenhg5/cc-connect#1635](https://github.com/chenhg5/cc-connect/pull/1635)
  (hzy9738, native-slash surface) and issue
  [chenhg5/cc-connect#1914](https://github.com/chenhg5/cc-connect/issues/1914);
  neither is merged as of 2026-11-01, and `chenhg5/cc-connect` has **no** grok
  agent on `main`. There is therefore no upstream commit to sync for this
  feature; the adapter was adapted to this fork's older `core` contract.
- Upstream policy note: [chenhg5/cc-connect#1244](https://github.com/chenhg5/cc-connect/issues/1244)
  asks new integrations to go through ACP first. The adapter now does: the ACP
  transport is the default, and the headless CLI remains only as a fallback.
- Known limits: session listing assumes Grok's `~/.grok/sessions/<escaped-cwd>/<sessionId>`
  layout; attachments are written to `<work_dir>/.cc-connect/attachments` and
  referenced by path in the prompt rather than sent as native image blocks.

#### ACP transport: native steering, turn cancellation, live switching

The default transport is one `grok agent stdio` process per agent session
(JSON-RPC 2.0, one object per line). It exists because the headless transport
has no control channel: `SessionSteerer` and `AgentSessionCanceller` were
impossible to implement on one-process-per-turn, so a message sent while the
agent worked was queued until the turn ended and `/stop` killed the process.

- Transport (`agent/grok/acp.go`): `initialize` with **empty**
  `clientCapabilities` (the agent then runs its own tools in-process instead of
  asking this adapter for fs/terminal handlers), then `session/load` when a
  session id is stored and `session/new` otherwise. `session/update`
  notifications map onto `core.Event` (`agent_message_chunk`, `agent_thought_chunk`,
  `tool_call`, `tool_call_update`, `usage_update`); updates that arrive before
  the first prompt are the `session/load` transcript replay and are dropped so
  restarting the daemon does not re-post old answers.
- Steering (`SessionSteerer`): `Steer` writes a second `session/prompt`. Grok
  injects a mid-turn prompt into the running turn only when its own
  `[ui] follow_up_behavior = "steer"`; the default `"queue"` would silently
  turn cc-connect's `queue.busy_behavior = "steer"` back into a queue. That
  setting is user-scoped and reachable **only** through the CLI's config file:
  the `GROK_CONFIG` / `GROK_CONFIG_PATH` overlays are confined to an allowlist
  that excludes `ui`, and ACP's `session/set_config_option` exposes only `model`
  and `reasoning_effort`. cc-connect therefore generates an overlay GROK_HOME
  (`agent/grok/steerhome.go`): `config.toml` is the user's config with only
  `[ui] follow_up_behavior` rewritten to `"steer"` (comments, formatting and
  every other key — including a same-named key in a different table — are left
  untouched), every
  other entry of the real home is symlinked so auth, sessions, skills, plugins
  and caches stay shared, and the user's own config file is never modified. A
  steered prompt settles as `stopReason: "cancelled"` when the agent folded it
  into the running turn; the adapter treats that as success and does not open a
  second engine turn. Grok injects at its next *safe gap* and deliberately
  holds the guidance while the agent is blocked on a long tool call, a
  background task or a subagent, so a steer sent during one of those can finish
  as its own turn after the primary one; the engine relays it as an unsolicited
  follow-up instead of dropping it. `steer_delivery = "cancel"` opts out of
  that trade-off: the adapter cancels the turn (`session/cancel` notification)
  and continues with the guidance immediately, aborting the in-flight tool call
  rather than waiting for the gap.
- `/stop` (`AgentSessionCanceller`): `CancelTurn` sends the `session/cancel`
  **notification** (as a request it returns `-32601`, which is easy to get
  wrong) and keeps the process and conversation alive, so the engine's
  canceller path preserves the interactive state instead of closing and
  rebuilding the agent session. If the cancelled prompt does not settle within
  30 s the adapter reports failure, and the engine falls back to closing the
  session — /stop must guarantee a stop, and the stored session id still
  resumes the conversation on the next message.
- Live switching: `SetLiveModel` / `SetLiveReasoningEffort` use
  `session/set_config_option` with `configId` `model` / `reasoning_effort`.
- Permissions: with `mode = "yolo"` (or `accept_edits` / `dont_ask`) the child
  runs with `--always-approve` plus `_meta.yoloMode`. Otherwise
  `session/request_permission` is surfaced as `core.EventPermissionRequest` and
  `RespondPermission` answers with the matching `optionId`; unknown agent→client
  requests get `-32601` instead of hanging the turn.
- Config: `backend = "acp" | "headless"` (default `acp`),
  `steer_delivery = "native" | "cancel"` (default `native`), optional
  `steer_home` to pin the generated overlay. `max_turns` is headless-only.
  Upgrade note: existing `grok` projects switch transport on upgrade with no
  config change — the session id in the store is reused through `session/load`,
  and the first turn of a conversation resumes the same grok session.
- Known duplication: `agent/acp` is the repository's generic ACP client, but its
  transport and session types are package-private and its `Send` blocks on the
  prompt, with no steer, no live model/effort switch and no per-turn cancel
  reason. Consolidating both agents onto one ACP client means exporting that
  transport and adding an async prompt + `SessionSteerer` there; until that
  refactor lands, `agent/grok` carries its own client (roughly the framing layer
  of `agent/acp/rpc.go` plus grok's `_meta`/catalog/config-option mapping).
- Verified live against grok 1.0.70 (`GROK_LOCAL_INTEGRATION=1 go test
  ./agent/grok -run TestLocalACP`): single turn, native steer injected into a
  running turn without a second turn, `/stop` keeping the conversation, and
  `session/load` resume across processes.
- Transport-level tests (`agent/grok/acp_test.go`) drive a scripted fake agent
  over pipes, so the frame handling, steer folding, cancel (including a
  settle timeout), the steer fallback, permission round trip, attachment cleanup
  and the stray-turn-timeout guard are covered without the CLI.
- Fork commit: [167a2bda](https://github.com/utensil/cc-connect/commit/167a2bda)
  on `feat/grok-acp`, [PR #20](https://github.com/utensil/cc-connect/pull/20).
  Supersedes the headless transport as the default; `backend = "headless"`
  keeps that path available for rollback.

### Combined `/agent <type> [path] [model] [reasoning]`

`/agent` accepts optional trailing `[model] [reasoning]` args so agent, model,
and reasoning switch in one command: `/agent <codex|claudecode|pi|grok> [path]
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

`agent/pi TestHandleMessageUpdate_ThinkingAccumulation` fails deterministically
(not intermittently) on clean `origin/dev` (bd0b973a): it expects
`thinking_end` with no following tool call to emit an accumulated
`EventThinking`, while `pendingThinking` intentionally drops that block unless a
tool call follows in the same turn. Reproduced in an isolated worktree at that
base ref before the Pi live-switching change, so it is a stale test, not a
regression from it. Do not use the full `agent/pi` package run as a green gate
until the test is either updated or retired.

Also reproduced at base ref `1abb24e8` (each passing in the fixed tree):
`agent/pi TestPiSession_CancelTurn_JSONStopsActiveProcess` and
`config TestValidateProjectDisplayConfig`. In the broad parallel run
`go test -tags no_web ./...`, `agent/codex
TestGetModelAndReasoningEffort_FromRuntimeConfigWhenUnset` can also fail while it
passes in isolation at both refs — it drives a scripted subprocess, so it is
load-sensitive. Treat a broad-suite failure as inconclusive until the test has
been re-run in isolation at the same ref.

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
