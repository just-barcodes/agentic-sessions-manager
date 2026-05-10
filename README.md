# Agentic Sessions Manager

A background service that tracks multiple agentic coding sessions (Claude Code, opencode, etc.) running anywhere on the machine, alerts when they need attention, and exposes a CLI to inspect them.

## Goals

- Single place to see the status of every agent session, regardless of where it's running (terminal, tmux pane, VS Code, etc.).
- Push-based alerts when a session is waiting for input or finishes — never poll.
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
                          ┌──────────────┼─────────────┐
                          ▼              ▼             ▼
                    notify-send      walker bar     sm watch
```

### Session registration

Sessions self-register; the manager never spawns them. You start `claude` or `opencode` however you normally do (standalone terminal, tmux, VS Code terminal — irrelevant), and the agent's lifecycle hooks publish events to NATS.

- **Claude Code**: `SessionStart`, `Notification`, `Stop`, and (optionally) `PreToolUse` / `PostToolUse` hooks fire shell commands that publish to NATS.
- **opencode**: equivalent hooks publish to the same subjects.
- The first event from a new session is treated as registration: the daemon assigns a UUID and persists it. Subsequent events reference that UUID via a small per-session identifier the hook script tracks (e.g., written to a temp file keyed on the agent's native session ID).

### State model

| State      | Meaning                                          | Set by              |
|------------|--------------------------------------------------|---------------------|
| `running`  | Active, agent is working                         | Hook events         |
| `waiting`  | Agent is waiting for human input or permission   | `Notification` hook |
| `idle`     | Alive but no recent activity (user judgment)     | `sm mark idle` (manual) |
| `finished` | Clean completion                                 | `Stop` hook         |
| `failed`   | Crashed or non-zero exit                         | `Stop` hook         |

MVP does **not** auto-detect `idle` or `failed-by-crash` — no polling, no timers. If a session dies without firing `Stop`, it stays `running` in the database until you mark it.

### Transport

NATS pub/sub on `localhost:4222`.

- Subjects: `sm.session.<uuid>.event` (events from session), `sm.session.<uuid>.alert` (high-priority).
- NATS is overkill for single-host but chosen deliberately: zero rewrite when we later add cross-device support (just point hooks at a remote NATS).
- The daemon runs an embedded NATS server (no separate install) for the MVP.

### Storage

SQLite, single file at `~/.local/share/sm/sm.db`.

```sql
sessions(id TEXT PK, agent TEXT, cwd TEXT, started_at, status, last_event_at, host_id TEXT)
events  (id INTEGER PK, session_id TEXT, ts, kind TEXT, payload JSON)
```

- `host_id` is included from day one so cross-device data can coexist later without a migration.
- History starts as events-only (state transitions, token counts when the hook supplies them). Schema extends cleanly to full transcripts later by adding a `transcripts` table or expanding the `events.payload` blob.

### Alerts

When a session enters `waiting` or `finished`, the daemon fans out to:

- **Desktop notification** via `notify-send` (libnotify).
- **Walker / status bar** via a JSON endpoint (`sm status --json`) walker can read cheaply on demand, plus a count file at `~/.local/state/sm/waiting-count` updated on every state change for zero-cost bar polling.

## CLI

```
sm ls                 # list all known sessions, one line each
sm show <id>          # details + recent events for one session
sm watch              # tail event stream (blocks, prints as events arrive)
sm mark <id> idle     # manual state override
sm status --json      # machine-readable summary for walker / scripts
```

## Daemon

Runs as a systemd **user** service: `systemctl --user start sm`. Starts on login, owns the embedded NATS, the SQLite connection, and the alert fan-out.

## Technical details

- Language: Go.
- Storage: SQLite (`modernc.org/sqlite` or `mattn/go-sqlite3`).
- Transport: embedded NATS (`nats-server` as a library).
- Hook scripts: small shell snippets shipped with the project; user adds them to their Claude Code / opencode config.
- Modular boundaries: agent adapters (claude, opencode, future) live behind a thin interface so adding a new agent only means writing hook scripts + an event mapper.

## Future extensions

- **Control plane**: send input to a `waiting` session; stop a session.
- **Auto-detect idle/crashed**: event-driven timeouts in the daemon (no session-side polling).
- **Resource tracking**: tokens, wall-time, cost per session (depends on what hooks expose).
- **Cross-device**: point hooks at a shared NATS, add `host_id`-aware queries. Schema already supports this.
- **Session summaries**: LLM-generated one-paragraph summary per session for context-switching.
- **Wrapper for hook-less tools**: PTY-wrap any process and emit events from output patterns, for tools without lifecycle hooks.
- **Full transcript capture**: store complete message history rather than event metadata only.

## Open questions

- Exact shape of the per-session UUID handoff: does the hook script call `sm register` synchronously on `SessionStart` and cache the UUID in a temp file keyed by Claude's session id, or do we derive a stable id from `(host, claude_session_id)` and skip the round-trip?
- Whether to ship hook scripts as a one-shot installer (`sm install-hooks`) that edits the user's Claude Code / opencode config, or just document the snippets.