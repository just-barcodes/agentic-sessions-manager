# Design note: making session state match reality

Status: implemented (full version)
Date: 2026-05-25

> Implementation note: shipped the full version (both new hooks + reaper
> widening). One divergence — rather than re-home the end-of-turn desktop
> notification onto `idle`, `notify-send` was removed entirely (the `NotifySend`
> sink is gone); push notifications will be redesigned separately. The
> `waiting-count` file and `sm status` remain the status surfaces.

## Problem

Reported states often don't match what the agent is actually doing. The
worst cases:

- A live Claude session resting at the prompt reads `finished` and is
  **hidden** from `sm ls` (the default listing drops `finished`).
- A session that has died is almost never marked `dead`.
- After answering a permission prompt, a session stays `waiting` while it is
  actually computing.

These are not edge cases — under normal Claude usage they are the common case.

## Why it happens

State is **edge-driven and sticky**: it is "the last lifecycle event we saw,"
held until the next event. There is no heartbeat and no "actively computing"
signal. The whole mapping is `session.NextState` (`internal/session/session.go:57`):

| Event kind                  | → State    |
|-----------------------------|------------|
| `session_start`, `note`     | `running`  |
| `notification`              | `waiting`  |
| `stop`, `session_end`       | `finished` |
| `fail`                      | `failed`   |
| (reaper, not an event)      | `dead`     |
| (`sm mark` only)            | `idle`     |

Three structural faults follow from this table:

### 1. `Stop` is per-turn, but maps to the terminal-sounding `finished`

Claude's `Stop` hook fires at the **end of every response turn**, not at
session end (`internal/hook/hook.go:103`). So a healthy session reads
`finished` the moment each turn ends. Nothing maps a mid-session turn back to
`running` (`SessionStart` fires only once), so after turn one a Claude session
is effectively `finished` for the rest of its life, blipping to `waiting` only
on permission prompts.

`sm ls` without `--all` hides `finished` (`internal/store/store.go:136`), so
those live sessions vanish from the default view. `Stop` (alive, between turns)
and `SessionEnd` (genuinely terminated) are also indistinguishable once both
land on `finished`.

### 2. No "turn started" signal to recover from `waiting`/`finished`

There is no wired hook for "the user submitted a prompt" or "a tool is
running," so once a session is `waiting` or `finished` it cannot return to an
active state mid-session. After you answer a permission prompt, the session
stays `waiting` until the turn's `Stop` flips it straight to `finished` — it
never passes back through `running`.

### 3. The reaper only probes `running`/`waiting`

`ReapStale` filters `status IN ('running','waiting')`
(`internal/store/store.go:181`). Because fault #1 parks most sessions in
`finished`, the reaper never looks at them — so a session that truly died is
mislabeled `finished` forever, never `dead`. The same blind spot strands
`idle` and `failed` sessions.

Secondary issues:

- `SessionEnd` is unreliable — it does not fire on `/clear` or `/exit` — so the
  one genuinely-terminal signal is often missing. (Already noted in `NOTES.md`.)
- `failed` is opencode-only (`session.error`); Claude has no failure hook, so
  Claude errors surface as `finished` or `dead`.
- `idle` is manual-only (`sm mark`, `internal/cli/cli.go:122`) and never
  auto-cleared or auto-reaped, so it goes stale.
- pid liveness tracks the first non-shell ancestor of the hook
  (`internal/liveness/liveness.go:44`). If that ancestor is a wrapper, tmux
  server, or editor rather than the agent, liveness follows the wrong process.

## Proposed model

Redefine states around what each signal actually means, and stop conflating
"between turns" with "terminated."

| State      | Meaning                                          | Set by                                  |
|------------|--------------------------------------------------|-----------------------------------------|
| `running`  | Actively processing a turn                       | `session_start`, **`user_prompt`**, **`tool_use`** |
| `waiting`  | Blocked on a human (permission / input)          | `notification`, `permission.asked`      |
| `idle`     | Alive, between turns, at the prompt              | `stop`, `session.idle`                  |
| `finished` | Session terminated cleanly                       | `session_end` **only**                  |
| `failed`   | Reported error                                   | `fail` / `session.error`                |
| `dead`     | Process gone without a clean exit                | reaper                                  |

The two changes that do most of the work:

1. **`stop`/`session.idle` map to `idle`, not `finished`.** `finished` becomes
   exclusively "clean termination" (`session_end`). `idle` becomes
   auto-derived (the manual `sm mark idle` path is no longer the only source).
   This kills the false-`finished` problem and makes `sm ls` show live
   between-turn sessions, because the default filter then hides only the two
   genuinely-terminal states, `finished` and `dead`.

2. **The reaper probes every non-terminal state.** Change the filter from
   `status IN ('running','waiting')` to `status NOT IN ('finished','dead')`.
   Now `idle`, `failed`, and `waiting` sessions all get corrected to `dead`
   when their process is gone — which, combined with change #1, is what finally
   makes "dead" reliable for ordinary sessions.

### Minimal vs. full

**Minimal (no new hooks):** apply changes #1 and #2 only. `running` then
appears only briefly at `session_start`; sessions otherwise sit in `idle`
between turns and `waiting` when blocked. We cannot tell "actively computing"
from "at the prompt," but the three headline bugs (false `finished`, hidden
live sessions, no reaping) are all fixed. This is the recommended first step.

**Full (wire two more hooks):** add Claude's `UserPromptSubmit` (→ `user_prompt`)
and `PreToolUse` (→ `tool_use`), both mapping to `running`. This gives a real
`running` vs `idle` distinction and the missing recovery edge from change #2,
so a session that resumes after a permission prompt correctly shows `running`
again. opencode already distinguishes these via its event stream.

## Out of scope

- Claude failure detection — no reliable failure hook exists; `failed` stays
  opencode-only and Claude crashes are caught by the reaper as `dead`.
- The liveness wrong-ancestor problem (`liveness.go:44`) is real but separable;
  track it on its own.

## Migration

States are stored as free TEXT, so no schema change is needed. Existing
`finished` rows are ambiguous (could be alive-between-turns or truly done); they
stay `finished` until their next event remaps them to `idle`/`running`, or are
left as-is. No backfill required. The reaper-filter widening takes effect on the
next sweep.

## Affected code

- `internal/session/session.go:57` — `NextState` mapping; add `user_prompt`,
  `tool_use` kinds if doing the full version.
- `internal/store/store.go:181` — reaper status filter.
- `internal/store/store.go:136` — `sm ls` default filter (already excludes
  `finished` + `dead`; verify it still reads correctly under the new meaning).
- `internal/hook/hook.go:97` — Claude event→kind map; add `UserPromptSubmit`,
  `PreToolUse` for the full version.
- `internal/cli/cli.go:160` — `parseState` for `sm mark` (idle remains a valid
  manual override).
