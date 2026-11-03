# Closed conversations in the session list

Status: design agreed 2026-11-02; implemented in the same change.

## Goal

One list of every conversation, in any harness (Claude Code, agy) and any
profile, whether or not a process is running it. A conversation without a
process is just one more state, colder than COLD. `/` searches all of it.
Enter shows a live conversation, or resumes a closed one.

## Model

* **Conversation** = a transcript on disk: Claude transcripts in every
  profile (`<profile>/projects/*/<id>.jsonl`, top level only: subagent
  transcripts live in subdirectories and are excluded; print-mode
  `sdk-cli` runs are excluded), and agy conversations
  (`brain/<id>/…/transcript_full.jsonl`).
* **Identity** = agent + session id. Resuming keeps the id (verified: a
  `claude --resume <id>` process registers under `<id>`), so a conversation
  and its live session share one key and one row.
* **CLOSED** group (glyph `·`) replaces ENDED: every conversation with no
  live process. It sits below COLD, ordered by last activity (newest
  first). A resumed conversation moves back to its live group; a session
  that exits moves to CLOSED. The status-line counts ignore CLOSED. In the
  cockpit CLOSED is shown by default; `e` hides or shows it. `cccp ls`
  shows it with `--all`.
* Rows and preview for closed conversations: title (custom → AI title → agy
  title → first prompt), profile tag, project, age, message count, branch;
  preview shows the first prompt, the last exchange and the summary.

## Search: `/`

As soon as a query is typed the grouped list becomes one flat list ranked by
match:

1. **Fuzzy metadata matches** (fzf-style: case-insensitive subsequence,
   bonuses for consecutive characters, word starts and prefixes; every
      space-separated term must match) over title, alias, project path,
   profile, harness, state, branch, first and last prompt, summary. Instant.
   Each term must match inside one field. Names and paths match fuzzily,
   but a match split into several runs counts only if every run starts a
   word (`gs` → "Gain staging") or is at least 3 characters long (`vitrbi` →
   "Viterbi"); otherwise letters picked out of unrelated words match
   (`viterbi` in "an avid kitten, very bitter"). Prompts and summaries
   are prose and match only by substring, at half weight, since a
   subsequence across a paragraph matches almost anything.
2. **Content matches** from the full-text transcript index, appended below
   (newest first) with their snippet, starting just before the match, after
   a 150 ms pause in typing (the scan takes tens of milliseconds). Results
   for a query that is no longer typed are dropped.

Clearing the query restores the grouped view. The separate `?` search mode is
removed.

## Enter

`core.PlanJump` decides, purely:

1. **Live in a tmux pane** → switch to it (as before).
2. **Live as a background session** → open a window running
   `claude attach <id>` (as before).
3. **Live but not in tmux** (a process in another terminal) → refuse:
   "already open outside tmux (pid N)". Starting a second process on the
   same transcript would interleave writes.
4. **Closed** → resume in a new tmux window:
      * command: `claude --resume <id>` in the conversation's profile
     (`CLAUDE_CONFIG_DIR` set for another profile; for the default one
     `env -u CLAUDE_CONFIG_DIR claude …`, removed in the command because a
     new window gets the tmux server's environment), or
     `agy --conversation=<id>`;
      * directory: the conversation's original working directory. This is
     required, not cosmetic: Claude Code finds the transcript under the
     project directory derived from the cwd it is started in. A missing
     directory is reported and nothing is started (`Jump.Resume`; attaching
     a background session works from anywhere, so there the directory is
     only a convenience);
   * tmux session: the one that already has the most panes in that
     directory (or below it); otherwise the client's current session;
   * window name: tmux's default, like every other window.
   The client switches to the new window and the cockpit closes; the hooks
   or the registry report the session within a second and the row leaves
   CLOSED.

## Where data lives

| Data | Where | Why |
|---|---|---|
| Catalog facts per transcript (title, cwd, branch, model, entrypoint, first/last prompt, last message tail, message count, first/last activity) | `~/.cache/cccp/text/<agent>-<id>.json`, next to the extracted search text | Derived from the transcript, rebuildable: a cache. Collected in the same single incremental pass as the search text. |
| Summaries (live and closed) | `~/.local/state/cccp/summaries.json` | Each costs a model call: durable state. Keyed by session and input hash, regenerated only when the input changes; pruned when the conversation disappears. |
| Seen marks, aliases, registry completions | `~/.local/state/cccp/daemon.json` | User state, unchanged. |

The cache directory is written by both the daemon and an in-process cockpit,
so updates hold an exclusive lock on `~/.cache/cccp/text/.lock`.

## Summaries for closed conversations

A different prompt from live sessions: what the conversation was about and
where it left off (live sessions: what the session needs from you).
Generated, under the conversation's own profile and policy:

* when a live session closes (its transcript is final), and
* on demand, when a closed row without a summary is selected in the
  cockpit (an `OpSummarize` request to the daemon).

There is no bulk backfill: titles cover most rows, and backfilling ~70
conversations would be ~70 model calls at once.

## Battery

Closed conversations do not change, so nothing watches the transcript
directories. The catalog is refreshed incrementally (a stat per transcript,
reading only appended bytes) when:

* the daemon starts (the first ever run builds the index: ~4 s for the
  current ~400 MB of transcripts, off the event loop);
* a session ends (pidfd exit or `SessionEnd`), and
* a full refresh is requested (`r` in the cockpit), which also picks up
  conversations created outside tmux.

## Limits

Claude Code deletes transcripts after `cleanupPeriodDays` (default 30), so
the list reaches back only that far; raise the setting in each profile for
longer history. cccp keeps no copies: resuming needs the original transcript
anyway.
