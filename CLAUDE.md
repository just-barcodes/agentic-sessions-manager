# CLAUDE.md

`sm` — the agentic sessions manager. A Linux daemon + CLI that tracks the live
state (running / waiting / idle / finished / failed / dead) of local AI coding
agents (Claude Code, opencode) so you can see which sessions need attention and
jump to the one that's waiting.

## Build & test

Uses [Task](https://taskfile.dev) (`Taskfile.yml`):

- `task build` — build the `./sm` binary (`go build -o ./sm ./cmd/sm`)
- `task test` — `go vet ./...` + `go build ./...` (the named suite is `go test ./...`)
- `task install` — install to `~/.local/bin/sm`
- `task run` — run the daemon in the foreground
- `task fmt` — `gofmt -w .`
- `task smoke` — emit a 3-event sequence end to end (daemon must be running)

Run the real Go test suite with `go test ./...`. Always `gofmt` before committing.

## Architecture

Event-driven, single-host. Hooks emit events onto an embedded NATS bus; the
daemon consumes them, persists to SQLite, and fans state changes out to alert
sinks. The CLI reads the store directly and publishes via the bus.

```
agent hook ──> sm hook ──(NATS)──> daemon ──> store (SQLite)
                                      └──────> alert sinks (waiting-count file)
sm ls/show/status/focus ──> store (read)
sm emit ──(NATS)──> daemon
```

### Package map (`internal/`)

- `session` — core domain: `Session`, `State`, `Event`, and the `Transition`
  state machine. **Leaf package, no I/O** — safe to import anywhere.
- `liveness` — process fingerprinting/probing via `/proc` (pid, start time, boot
  id). `Alive` / `IsProcessDead`.
- `store` — SQLite wrapper for sessions and events; owns the schema, migrations,
  and the terminal-state visibility filter.
- `bus` — NATS pub/sub wrapper. Subjects: `sm.session.<uuid>.event`; daemon
  subscribes to `sm.session.*.event`.
- `daemon` — ties bus + store + alert sinks together; runs the reaper sweep.
- `cli` — user-facing subcommands (`ls`, `show`, `mark`, `status`, `focus`, `emit`).
- `hook` — `sm hook <agent>`: parses Claude/opencode hook JSON from stdin into
  bus events.
- `alert` — `Sink` interface for delivering state changes (e.g. the waiting-count
  file read by status bars).
- `focus` — locates and raises the terminal window / tmux pane hosting a
  session's agent (Hyprland + tmux + `/proc`).

`cmd/sm/main.go` is the entry point that dispatches subcommands.

### Conventions

- Standard Go layout; module `github.com/just-barcodes/agentic-sessions-manager`.
- The `session` package must stay I/O-free so every package can import it.
- Owner-only on disk: the DB and data dir are `0600`/`0700`; keep new files that
  hold prompt text or paths the same.
- Times are stored as Unix seconds in SQLite.
- Event-driven state transitions go through `session.Transition`; terminal
  states come from `session.TerminalStates()` so filters stay in sync. The one
  sanctioned exception is `sm mark`, a manual override that writes a
  user-chosen state validated by `session.ParseState` (not the event machine).

## Pointers

- `README.md` — install, hook setup, usage.
- `docs/state-model-design.md` — the session state model and transition rationale.
- `contrib/` — `sm.service` (systemd --user unit) and `opencode-plugin/`.

Two scoped `.claude` dirs exist for dogfooding, separate from repo-level config:
`agent-tests/.claude/settings.json` wires Claude's hooks to `sm hook claude` so a
Claude session run inside `agent-tests/` feeds real events into `sm`;
`contrib/.claude/settings.local.json` is just a local permission allowlist.
