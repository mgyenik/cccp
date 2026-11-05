# Notifications: which turn ends need you

Status: built 2026-11-05, after correlating the daemon's notification log
with transcripts.

## What was wrong

Two complaints: notifications showing a previous turn's text, and
notifications when there was nothing to do.

**Stale text: two causes.**

* A session's one-line summary is generated after its turn ends (a few
  seconds of `claude -p`), but it was attached to the session whatever turn
  it had been written for. The notification fires as the turn ends, so it
  carried the previous turn's summary, and the cockpit showed that summary
  until the new one arrived.
* When background work reports, Claude Code feeds the report to the agent as
  a prompt (`<task-notification>…`, or `Another Claude session sent a
  message:` for a subagent's hand-back), and `UserPromptSubmit` fires for it.
  cccp recorded that as the user's last prompt: shown in the cockpit and fed
  to the summarizer.

**Notifications with nothing to do.** Every turn end (`Stop`) counted as
YOUR TURN, including the turns Claude Code starts on its own to take in a
background task's or subagent's report. In a session running several
agents these come every few minutes, and nearly all of them are progress
reports ("the agent is waiting on its sub-agent"). In one evening's log,
the three notifications within 74 seconds were all such turns.

## Telling the turns apart

The transcript records who started each turn: `turnOrigin` (`human`,
`task_notification`, `peer`) and `promptSource` (`typed`,
`suggestion_accepted`, `system`), on the entry that starts it (a subagent's
hand-back is a meta entry). Older entries carry neither; there a task
notification is recognised by its text. Claude Code's `last-prompt` entries
hold only what the user typed (8,165 of 8,165 checked), and the hook payload
carries only the prompt text (no origin field, 2.1.318), so the hook state
skips automatic prompts by their text.

Whether a report is the *last* one (all background work done) cannot be
derived: counting tasks launched minus tasks reported finished never
reached zero over two days of a busy session, even when the agent said
"nothing to act on".

## Rule

* A turn the user started notifies at once, as before.
* A turn Claude Code started (`Session.AutoTurn`) waits for its summary, and
  notifies only if the summary's verdict is `act`: the assistant's last
  message asks a question, wants a decision or approval, tells the user to do
  something, or says new work is ready to try. `wait` (progress, agents
  still running, what the assistant will do next) does not notify; the
  session still shows YOUR TURN in the cockpit and the status line.
* A turn that ends while a background shell command it started still runs
  (`Session.BackgroundShell`) is judged the same way, because it may be a
  build it is waiting on, or a server it left running for the user. On
  `wait` it stays in WORKING, shown as `background shell · …`; on `act` it
  is YOUR TURN and notifies. Claude Code's registry says so: status
  `shell` is idle at the prompt with a `local_bash` task unfinished
  (2.1.318). Background subagents do not change that status (it stays
  `idle`); their reports are automatic turns.
* A registry status cccp does not know never overrides the hooks' state
  (before `shell` was known, such sessions showed as UNKNOWN).
* It notifies anyway when no verdict can come: summaries off or not allowed
  (`summaries = "ac"` on battery), no message to summarise, the summary
  failed, summaries paused, or no answer within 2 minutes.
* A permission prompt or question during an automatic turn (NEEDS YOU)
  notifies at once.
* A summary is shown only for the state it was written for (keyed by the
  hash of its input).

Each decision is logged with its text: `notify KEY (GROUP, LOCATION): TITLE —
BODY`, `not notifying (background work, nothing to do) …`, or `not notifying
(pane visible) …` (once per state, so every attention state leaves a line)
(`journalctl --user -u cccp`).

## Measurement

The verdict is a model's judgment, so it was measured: the 35 automatic
turns among one session's 48 most recent turn ends, each labelled by hand
from its full text (act: asks, wants a decision, or has something ready to
try; 8 of them), run through cccp's own summarizer input and command.

| Prompt | Model | progress reports silenced | asks notified |
|---|---|---|---|
| verdict first ("act: …"), with the user's last prompt | haiku | 4 of 27 | 5 of 8 |
| verdict first, without the stale prompt, criteria spelled out | haiku | 19 of 27 | 7 of 8 |
| **verdict last ("… [act]"), same input** | **haiku** | **26 of 27** | **7 of 8** |
| verdict first, same input | sonnet | 27 of 27 | 5 of 8 |

What changed the result:

* **The user's last prompt misled it.** In an automatic turn it can be hours
  old; the summarizer answered it ("decide whether to restart the stuck
  agent", turn after turn) instead of the report. An automatic turn's input
  leaves it out and says the turn answered a background report.
* **The verdict written first contradicted the line written after it**
  ("act: none needed; …"). Asked for at the end of the line, after the line
  is written, it agreed with it.
* **A long message was cut**: the input kept the message's last 2,500 bytes
  and one ask was at its start; it keeps 6,000 now.

Limits of this measurement: one session, labels by one reader, one run of a
non-deterministic model. The remaining miss was a "reload to try it" at the
start of a long message; the remaining false alarm read "ready for me to
review" as the user's task. A missed ask is never lost: the session stays in
YOUR TURN in the cockpit.
