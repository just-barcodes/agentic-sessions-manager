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

- **Claude Code**: `SessionStart`, `Notification`, `Stop`, `SessionEnd`, and (optionally) `PreToolUse` / `PostToolUse` hooks fire shell commands that publish to NATS.
- **opencode**: equivalent hooks publish to the same subjects.

**UUID handoff.** Every hook passes the agent's *native* session id (Claude Code provides one on stdin as `session_id`) plus the agent name. The daemon owns the mapping `(agent, native_id) → uuid` and persists it in the `sessions` table. On every event:

- known `(agent, native_id)` → reuse the existing UUID;
- unknown → assign a new UUID and create the session row right then.

No client-side state, no NATS request/reply round-trip. A missed `SessionStart` hook is non-fatal: the next event that arrives still produces a coherent session.

### State model

| State      | Meaning                                          | Set by              |
|------------|--------------------------------------------------|---------------------|
| `running`  | Active, agent is working                         | Hook events         |
| `waiting`  | Agent is waiting for human input or permission   | `Notification` hook |
| `idle`     | Alive but no recent activity (user judgment)     | `sm mark idle` (manual) |
| `finished` | Clean completion                                 | `Stop` / `SessionEnd` hook |
| `failed`   | Reported failure                                 | `session.error` (opencode) |
| `dead`     | Process gone without a clean exit                | reaper (process liveness) |

The daemon does not auto-detect `idle`. It *does* detect crashed sessions — those that die without a clean `SessionEnd`/`Stop` — by **process liveness rather than timers**: hooks record the agent's pid (plus its start time and the boot id) at first contact, and the daemon periodically reaps sessions whose process is gone, marking them `dead`. `sm ls` / `sm status` also reap on read. Sessions without a captured pid are left untouched.

### Transport

NATS pub/sub on `localhost:4222`.

- Subjects: `sm.session.<uuid>.event` (events from session), `sm.session.<uuid>.alert` (high-priority).
- NATS is overkill for single-host but chosen deliberately: zero rewrite when we later add cross-device support (just point hooks at a remote NATS).
- The daemon runs an embedded NATS server (no separate install) for the MVP.

### Storage

SQLite, single file at `~/.local/share/sm/sm.db`.

```sql
sessions(id TEXT PK, agent TEXT, native_id TEXT, cwd TEXT, host_id TEXT,
         started_at INTEGER, last_event_at INTEGER, status TEXT,
         pid INTEGER, pid_start INTEGER, boot_id TEXT)
events  (id INTEGER PK, session_id TEXT, ts INTEGER, kind TEXT, payload JSON)
-- UNIQUE(agent, native_id) where native_id != ''  → enforces 1:1 handoff
-- (pid, pid_start, boot_id) is the agent process fingerprint used for liveness
```

- `pid` / `pid_start` / `boot_id` are added by an additive migration on open, so existing databases keep working.

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
- **Auto-detect idle**: event-driven idle detection in the daemon (crashed sessions are already reaped via process liveness).
- **Resource tracking**: tokens, wall-time, cost per session (depends on what hooks expose).
- **Cross-device**: point hooks at a shared NATS, add `host_id`-aware queries. Schema already supports this.
- **Session summaries**: LLM-generated one-paragraph summary per session for context-switching.
- **Wrapper for hook-less tools**: PTY-wrap any process and emit events from output patterns, for tools without lifecycle hooks.
- **Full transcript capture**: store complete message history rather than event metadata only.

## Open questions

- Whether to ship hooks as a one-shot installer (`sm install-hooks`) that edits the user's Claude Code / opencode config, or just document the snippet.