# Agentic Sessions Manager

A background service that tracks multiple agentic coding sessions (Claude Code, opencode, etc.) running anywhere on the machine, alerts when they need attention, and exposes a CLI to inspect them.

## Goals

- Single place to see the status of every agent session, regardless of where it's running (terminal, tmux pane, VS Code, etc.).
- Push-based alerts when a session is waiting for input or finishes.
- Fast, scriptable CLI for status, history, and live event streams.
- Modular design so new agent tools, transports, and alert sinks can be added later.

## Non-goals (for MVP)

- No GUI. A desktop-notification daemon and walker integration are the only visual surfaces.
- No cross-device sync. The MVP runs on one host; protocol and schema choices keep that door open.
- No control plane (sending input, killing sessions). MVP is observe-only. See [Future extensions](#future-extensions).

## How it works

```
┌──────────────────┐    NATS    ┌──────────────────┐
│ Claude Code      │  events    │  sm daemon       │
│ (hook fires)     │ ─────────► │  (systemd --user)│
└──────────────────┘            │                  │
                                │  - subscribes    │
┌──────────────────┐            │  - writes SQLite │
│ opencode         │            │  - serves CLI    │
│ (hook fires)     │ ─────────► │  - emits alerts  │
└──────────────────┘            └────────┬─────────┘
                                         │
                              ┌──────────┴──────────┐
                              ▼                     ▼
                         walker bar              sm ls / status
```

### Session registration

Sessions self-register; the manager never spawns them. You start `claude` or `opencode` however you normally do (standalone terminal, tmux, VS Code terminal — irrelevant), and the agent's lifecycle hooks publish events to NATS.

- **Claude Code**: `SessionStart`, `UserPromptSubmit`, `PreToolUse`, `Notification`, `Stop`, and `SessionEnd` hooks fire shell commands that publish to NATS.
- **opencode**: equivalent hooks publish to the same subjects.

**UUID handoff.** Every hook passes the agent's _native_ session id (Claude Code provides one on stdin as `session_id`) plus the agent name. The daemon owns the mapping `(agent, native_id) → uuid` and persists it in the `sessions` table. On every event:

- known `(agent, native_id)` → reuse the existing UUID;
- unknown → assign a new UUID and create the session row right then.

No client-side state, no NATS request/reply round-trip. A missed `SessionStart` hook is non-fatal: the next event that arrives still produces a coherent session.

### State model

| State      | Meaning                                          | Set by              |
|------------|--------------------------------------------------|---------------------|
| `running`  | Actively processing a turn                       | `UserPromptSubmit`, `PreToolUse` |
| `waiting`  | Blocked on human input or permission             | `Notification` hook |
| `idle`     | Alive, at the prompt (just started or between turns) | `SessionStart`, `Stop` hook (or `sm mark idle`) |
| `finished` | Session terminated cleanly                       | `SessionEnd` hook   |
| `failed`   | Reported failure                                 | `session.error` (opencode) |
| `dead`     | Process gone without a clean exit                | reaper (process liveness) |

State is edge-driven: it reflects the last lifecycle event seen. The key distinction is that `idle` (the agent finished a turn and is waiting at the prompt) is separate from `finished` (the session actually terminated). `idle` is derived automatically from each turn's `Stop`; `finished` is set only by `SessionEnd`.

Because `SessionEnd` is unreliable — it does not fire on `/clear` or `/exit` — genuine termination is usually caught by the reaper instead. The reaper detects gone sessions by **process liveness rather than timers**: hooks record the agent's pid (plus its start time and the boot id) at first contact, and the daemon periodically reaps any non-terminal session (anything but `finished`/`dead`) whose process is gone, marking it `dead`. `sm ls` / `sm status` also reap on read. Sessions without a captured pid are left untouched.

### Transport

NATS pub/sub on `localhost:4222`.

- Subjects: `sm.session.<uuid>.event` (events from session).
- NATS is overkill for single-host but chosen deliberately: zero rewrite when we later add cross-device support (just point hooks at a remote NATS).
- The daemon runs an embedded NATS server (no separate install) for the MVP.
- The bus requires token auth: the daemon generates an owner-only token at
  `~/.local/share/sm/bus-token` on startup and clients read it from there.
  Loopback is reachable by every local process; the token is what scopes the
  bus to your user.
- Clients honor `SM_BUS_URL` to dial a non-default bus (e.g. a tailnet address
  or an SSH-forwarded port).

### Storage

SQLite, single file at `~/.local/share/sm/sm.db`.

```sql
sessions(id TEXT PK, agent TEXT, native_id TEXT, cwd TEXT, host_id TEXT,
         started_at INTEGER, last_event_at INTEGER, status TEXT,
         pid INTEGER, pid_start INTEGER, boot_id TEXT, last_prompt TEXT)
events  (id INTEGER PK, session_id TEXT, ts INTEGER, kind TEXT, payload JSON)
-- UNIQUE(agent, native_id) where native_id != ''  → enforces 1:1 handoff
-- (pid, pid_start, boot_id) is the agent process fingerprint used for liveness
-- last_prompt caches the latest user_prompt text so `sm ls` needn't scan events
```

- `pid` / `pid_start` / `boot_id` / `last_prompt` are added by additive migrations on open, so existing databases keep working. When `last_prompt` is first added it is backfilled from existing events, so pre-upgrade sessions keep their prompts.

- `host_id` is included from day one so cross-device data can coexist later without a migration.
- History starts as events-only (state transitions, token counts when the hook supplies them). Schema extends cleanly to full transcripts later by adding a `transcripts` table or expanding the `events.payload` blob.

### Alerts

On every state change the daemon updates the status surfaces:

- **Walker / status bar** via a JSON endpoint (`sm status`) walker can read cheaply on demand, plus a count file at `~/.local/state/sm/waiting-count` updated on every state change for zero-cost bar polling. `sm focus <id>` jumps straight to a waiting session — see [Switching to a waiting session](#switching-to-a-waiting-session).

> Push notifications (previously `notify-send`) were removed pending a better design. The count file and `sm status` are the only surfaces today.

## CLI

```
sm ls                 # list all known sessions, one line each
sm show <id>          # details + recent events for one session
sm mark <id> idle     # manual state override
sm status             # machine-readable (JSON) summary for walker / scripts
sm focus <id>         # raise the terminal window/tmux pane hosting the session
```

### Switching to a waiting session

`sm focus <id>` jumps to where a session lives. It reuses the agent process
fingerprint captured for liveness: from the stored pid it inspects the live
process to decide how to focus.

- **Bare terminal window**: walks the agent's process ancestors and raises the
  Hyprland window that owns them (`hyprctl dispatch hl.dsp.focus`).
- **Inside tmux** (`TMUX_PANE` set in the agent's environment): finds a client
  viewing the pane's session, raises that client's window, and selects the pane.

Focusing a window is a window-manager action, not agent control, so it stays
within the observe-only scope. A session whose process was never fingerprinted
(`pid 0`) or has since exited cannot be focused, and `sm focus` says so.

## Daemon

Runs as a systemd **user** service: `systemctl --user start sm`. Starts on login, owns the embedded NATS, the SQLite connection, and the alert fan-out.

## Remote machines

Hooks on other machines can feed the central daemon over Tailscale. The daemon
stays bound to loopback; `tailscale serve` forwards the port tailnet-only, so
the bus token never crosses an unencrypted link (NATS sends it cleartext
inside the TCP stream).

On the workstation running the daemon:

```sh
tailscale serve --bg --tcp 4222 tcp://localhost:4222
```

On each remote machine:

1. Install `sm` and wire the agent hooks as usual.
2. Copy `~/.local/share/sm/bus-token` from the workstation to the same path
   (keep it `0600`).
3. Point the hooks at the workstation, e.g. in the hook command:
   `SM_BUS_URL=nats://<workstation>.tailnet-name.ts.net:4222 sm hook claude`.

Sessions carry the hostname the hook ran on (`host_id`), so the workstation's
process-liveness reaper leaves remote sessions alone. Remote sessions are
never reaped automatically yet — a stale one needs `sm mark <id> dead` — and
`sm focus` cannot raise windows on another machine.

## Technical details

- Language: Go.
- Storage: SQLite (`modernc.org/sqlite`).
- Transport: embedded NATS (`nats-server` as a library).
- Behavior is specified as Gherkin scenarios in `bdd/features/`, run end to
  end against the real binary with `task bdd` (see CLAUDE.md for authoring).
- Hook scripts: small shell snippets shipped with the project; user adds them to their Claude Code / opencode config.
- Modular boundaries: agent adapters (claude, opencode, future) live behind a thin interface so adding a new agent only means writing hook scripts + an event mapper.

## Future extensions

- **Control plane**: send input to a `waiting` session; stop a session.
- **Auto-detect idle**: event-driven idle detection in the daemon (crashed sessions are already reaped via process liveness).
- **Resource tracking**: tokens, wall-time, cost per session (depends on what hooks expose).
- **Cross-device**: point hooks at a shared NATS, add `host_id`-aware queries. Schema already supports this.
- **Session summaries**: LLM-generated one-paragraph summary per session for context-switching.
- **Wrapper for hook-less tools**: PTY-wrap any process and emit events from output patterns, for tools without lifecycle hooks.
- **Full transcript capture**: store complete message history rather than event metadata only.

## Open questions

- Whether to ship hooks as a one-shot installer (`sm install-hooks`) that edits the user's Claude Code / opencode config, or just document the snippet.

