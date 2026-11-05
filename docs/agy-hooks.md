# agy (Antigravity CLI) hooks: findings and decision

Researched and tested 2026-11-02. Local CLI versions during testing: 1.4.8,
then 1.4.12 (agy updated itself between runs).

## 1. What we observed locally

Test setup: a workspace-local `.agents/hooks.json` in a throwaway workspace
under the session scratchpad, in an isolated tmux server (`tmux -L cccptest`).
Each hook appended its stdin to a log file.

* `SessionStart` fires **lazily, on the first prompt**, not when agy launches.
  An agy that has been started but never prompted has no conversation id yet.
* Each hook runs as `sh -c <command>`; the hook's parent is that `sh`, whose
  parent is the `agy` process. `$TMUX_PANE` is inherited.
* Payloads are camelCase JSON. Fields seen:
  * common: `conversationId`, `workspacePaths[]`, `transcriptPath`
    (`…/brain/<id>/.system_generated/logs/transcript_full.jsonl`),
    `artifactDirectoryPath`, `modelName`, `lastUserInput`
  * `PreInvocation`: `invocationNum`, `initialNumSteps`
  * `PreToolUse` / `PostToolUse`: `stepIdx`, `toolCall{name,args}` (e.g.
    `run_command` with `args.CommandLine`, `args.toolSummary`); `PostToolUse`
    adds `error` (empty string on success)
  * `Stop`: `executionNum`, `terminationReason` (`NO_TOOL_CALL` for a normal
    turn end), `error`, `fullyIdle`
* Event order for one turn with one tool call:
  `SessionStart → PreInvocation → PreToolUse → PostToolUse → PreInvocation → Stop`.
* Printing `{}` from `SessionStart`, `PreInvocation`, `PostToolUse` and `Stop`
  had no visible effect on the session.
* A `PreToolUse` hook that printed **nothing** and exited 0: the tool ran.
  This machine has `"toolPermission": "always-proceed"` in
  `~/.gemini/antigravity-cli/settings.json`, so the tool would have run with
  no hook at all. The test therefore says nothing about what an empty
  decision does under a prompting policy. agy's log
  (`~/.gemini/antigravity-cli/log/cli-*.log`) records hook loading but not how
  the decision was interpreted.

## 2. What the sources say about `PreToolUse` output

### Official contract

* **Current web docs** ([antigravity.google/docs/hooks][ag-hooks], fetched
  2026-11-02): `decision` is **Required**. Values:
  * `allow`: "Allows the tool call to proceed, **delegating to the next hook
    or default policy**." This is a documented no-opinion value.
  * `auto_approve`: approves when the default policy would otherwise prompt,
    while still honouring explicit user rules.
  * `deny`, `ask` (respects Always Allow), `force_ask` (ignores cached
    permissions), `deny_unless_prior_grant`.
* **Docs bundled with the installed CLI 1.4.12**
  (`~/.gemini/antigravity-cli/builtin/skills/agy-customizations/docs/hooks.md`)
  list only `allow`/`deny`/`ask`/`force_ask` and describe `allow` as
  "Automatically allow the tool execution". The docs on disk and on the web
  disagree about what `allow` means.
* Neither source documents what happens when `decision` is missing or
  malformed, when the hook exits non-zero, or when it times out.

### Community evidence (live tests by third parties)

| Source | agy version, mode | Missing/empty decision | Non-zero exit |
|---|---|---|---|
| safe-chains [HARNESS-BEHAVIORS.md][sc-hb] (live drive) | 1.1.2, `request-review` | "A missing decision **prompted** rather than ran." | — |
| safe-chains [harness-capability-model.md][sc-cap] and [agy.rs][sc-agy] (same project, same probe) | 1.1.2 | "Antigravity **FAILS CLOSED** on a missing/malformed decision." (contradicts the line above) | — |
| [google-antigravity/antigravity-cli#1183][gh1183] (author's own repro) | 1.2.14 | "a non-zero exit or an empty/invalid decision on `PreToolUse` **denies** the tool call" | denies |
| safe-chains [HARNESS-BEHAVIORS.md][sc-hb] | 1.1.2, `request-review` | — | `allow` (in 1.1.2) **still prompted**, consistent with the "delegates to default policy" wording later documented |
| [#1310][gh1310] | 1.4.4, ADC auth | `PreToolUse` hooks are **never invoked** with `AGY_ADC_AUTH=true` (log shows the file loaded) | — |
| this machine (section 1) | 1.4.8, `always-proceed` | tool **ran** | — |

## 3. Conclusion

What agy does with a `PreToolUse` hook's output is **not stable across
versions, permission modes or sources**. Depending on build and mode, a
missing decision has been reported to prompt, to deny, or (here) to run;
and a failed hook (crash, non-zero exit, possibly timeout) is reported to
deny.

For a monitoring tool this matters in two ways:

1. **Silence is not neutral.** Any `PreToolUse` hook has to emit a decision.
   The only documented pass-through is `allow`, and its meaning changed
   between the bundled and the web docs.
2. **A broken monitor would break the agent.** If cccp's hook crashed, was
   missing after an uninstall, or was slow, agy could deny every tool call
   (fail closed, per #1183). A status display must never be able to do that.

What `PreToolUse` would give cccp: the name of the tool in flight (instead
of the most recently completed one), and the start of a possible
approval wait. Under this machine's `always-proceed` setting there are no
approval waits.

**Decision: cccp does not hook agy `PreToolUse`.** It hooks `SessionStart`,
`PreInvocation`, `PostToolUse` and `Stop`, which have no permission
semantics, and prints `{}`. Failure behaviour of those events:

* `PreInvocation`: #1183's table shows `{}`, exit 1 and exit 2 all let the
  model run (fail open).
* `Stop`: the docs say any `decision` other than `"continue"` allows the
  stop, so `{}` or a failure cannot keep the agent looping.
* `PostToolUse` / `SessionStart`: these run after the fact or only inject
  context; `{}` is the documented empty output.

agy hooks are synchronous, so the hook entries set a short `timeout`, and the
cccp hook does no blocking I/O beyond its own state file.

Consequences:

* agy's state is `working` from `PreInvocation` until `Stop`; the tool shown
  is the most recently *completed* one (from `PostToolUse`).
* agy approval waits aren't detected. With `always-proceed` they never occur.
* `PreInvocation`/`Stop` hooks don't fire under ADC auth if that bug (#1310)
  extends beyond `PreToolUse`; such sessions fall back to unmanaged
  (inferred) tracking.

If this is revisited, the hook must emit exactly `{"decision":"allow"}` on
every path, and must be re-verified live under `request-review` on the
installed version, both for the normal path and for a crashing hook.

[ag-hooks]: https://antigravity.google/docs/hooks
[sc-hb]: https://docs.rs/crate/safe-chains/latest/source/HARNESS-BEHAVIORS.md
[sc-cap]: https://docs.rs/crate/safe-chains/latest/source/docs/design/harness-capability-model.md
[sc-agy]: https://docs.rs/safe-chains/latest/src/safe_chains/targets/agy.rs.html
[gh1183]: https://github.com/google-antigravity/antigravity-cli/issues/1183
[gh1310]: https://github.com/google-antigravity/antigravity-cli/issues/1310

## Interrupts (2026-11-05)

**agy runs no hook when the user interrupts a turn** (Escape; "Interrupted ·
What should Antigravity CLI do instead?"), though its bundled docs list
`USER_CANCELED` as a `Stop` reason. Tested in an isolated workspace with a
hook logging every event, interrupting once while a tool ran and once while
the model was writing: neither `Stop` nor `PostInvocation` fired, on 1.4.12
(a session started before agy updated itself) and 1.5.1. The transcript
records nothing either. Without a correction a session stays WORKING
forever. A background task finishing after the turn also fires a late
`PostToolUse`, which reads as work.

agy does record it: each conversation's own database
(`~/.gemini/antigravity-cli/conversations/<id>.db`) gets an
`executor_metadata` row when an execution (the agent's run on one prompt)
ends, however it ends, and none while it runs. The row is a protobuf whose
leading varint fields are 1, the `ExecutorTerminationReason` (the enum's
order in the binary: `UNSPECIFIED, ERROR, USER_CANCELED, MAX_INVOCATIONS,
NO_TOOL_CALL, …`), 2, the number of model calls, and 3, the execution's last
step index. Across 35 rows of two conversations every normal end was 4
(`NO_TOOL_CALL`), every interrupt 2 (`USER_CANCELED`), and field 3 equalled
the transcript's last `step_index`.

So an agy turn the hooks call working is over when the latest execution has
ended and the transcript has no step after its last one (a new prompt adds
a step at once), once the hooks have been quiet for 10 s (the moment a new
prompt's hook has arrived and its transcript step has not). An interrupt is
not a completion (the user was there). This reads undocumented internals:
if the layout changes, the decoder finds no reason and cccp behaves as
before.
