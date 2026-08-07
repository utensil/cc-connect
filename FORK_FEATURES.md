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
active Codex turn by default. Messages with attachments, locations, or a
session still starting continue to queue safely. Explicit `/steer` remains
available.

- Fork merge commit: [c0c76ab7](https://github.com/utensil/cc-connect/commit/c0c76ab7).
- Merge record: [PR #1](https://github.com/utensil/cc-connect/pull/1).

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

### Pre-existing full-suite test flakiness on macOS

`go test -race ./core` (full suite) intermittently fails
`TestCUJ_A3_ImageReachesAgent` / `TestCUJ_A5_FileReachesAgent` with
`TempDir RemoveAll cleanup: directory not empty` on macOS. Reproduces on clean
`origin/dev` (fc22b8b) — unrelated to /agent switching. Run the failing tests
in isolation (`-run 'TestCUJ'`) to confirm green.
