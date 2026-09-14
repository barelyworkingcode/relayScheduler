# relayScheduler (Go)

Task scheduling service. Stores tasks in a single `tasks.json`, runs them on
schedule by creating sessions/terminals through relay's front door (which routes
them to relayLLM), and broadcasts task lifecycle events over WebSocket.

**README.md is the reference for task types, schedules, missed-run rules, the
run history record, API, and WS events.** Keep it in sync when behavior changes.

## Architecture

Flat `package main`, one file per concern:

```
main.go               Entry point: launch identity first, then flags, Unix-socket listener, manifest registration
launch.go             Relay launch identity: read the fd-3 secret, scrub env, Hello over the bridge
client.go             RelayClient: outbound calls to relay's front door (projects, sessions, terminals, /ws)
task.go               Task + Execution types, TaskView envelope, schedule structs
schedule.go           Schedule validation + next-run calculation
scheduler.go          Tick loop, missed-run handling, chat/PTY execution, reschedule, broadcasts
store.go              Mutex-protected CRUD over tasks.json (atomic writes)
logstore.go           Per-task run history (newest first, capped at 100)
hub.go / wshandler.go WebSocket hub + /ws/tasks upgrade handler
api.go                Task API on Go 1.22 method/wildcard mux patterns
manifest.go           Front-door manifest (routes) + RegisterManifest call
bridge.go             Minimal relay bridge client (tokenless; identity-authenticated) used for registration
auth.go               Bearer-auth middleware + token generation for the inbound listener
```

## Task model in one paragraph

Two task types. **Chat** (`sessionType` `"headless"` or empty) creates a headless
relayLLM session and sends `prompt`. **PTY** (`"pty"`) launches a relayLLM
terminal template (`templateId` + `extraArgs`) and waits for its exit code. Both
share scheduling, history (`Execution`), and the `view` envelope eve dispatches
on: `interactive` (join the session) or `readonly` (terminal viewer). Seven
schedule types: `daily`, `hourly`, `interval`, `weekly`, `cron` (only `M H * * *`
/ `M * * * *`), `once`, `on_demand`.

## Relay front-door integration

relayScheduler is a relay-enhanced service (protocol: `../relay/docs/service-manifest.md`).

- **Inbound.** Serves its HTTP + WS API on a Unix socket (`--socket`, default
  `{data-dir}/relayscheduler.sock`), bearer-authed (`auth.go`). It picks its own
  socket path and token and tells relay both via manifest registration.
- **Launch identity** (`../relay/docs/launch-identity.md`). relay puts no
  credential in the environment. It sets `RELAY_LAUNCH_FD=3`,
  `RELAY_BRIDGE_SOCKET`, `RELAY_SERVICE_ID`, `RELAY_FRONTEND_SOCKET`, and passes a
  64-hex launch secret on fd 3. `bootstrapLaunchIdentity` is the first thing
  `main` does: it unsets `RELAY_LAUNCH_FD` and the removed
  `RELAY_SERVICE_TOKEN` / `RELAY_MCP_TOKEN` / `RELAY_FRONTEND_TOKEN`, drains and
  closes fd 3, and sends `Hello`. Any failure while `RELAY_LAUNCH_FD` is set
  exits non-zero; never degrade to standalone. relay then recognises this exact
  process by its kernel audit token, so later bridge requests carry no `token`
  key and frontend requests carry no `Authorization` header. The secret is held
  only for the Hello round-trip and never logged. The scheduler spawns no child
  processes; if that changes, strip `RELAY_LAUNCH_FD` from their environment.
- **Registration.** After Hello, the scheduler sends `RegisterManifest` (no
  token, `serviceId` = `RELAY_SERVICE_ID`) declaring `/api/tasks`,
  `/api/tasks/`, `/ws/tasks`. relay's dispatcher reverse-proxies those routes to
  the socket, replacing the caller's bearer with the scheduler's internal token.
  Registered with `--capability frontend --capability manifest` (`build.sh`).
- **`/ws/tasks`, not `/ws`.** relayLLM already claims `/ws` and relay rejects
  duplicate routes. eve's server opens a second upstream WS to `/ws/tasks` and
  forwards task frames to the browser (eve `relay-client.js`).
- **Standalone** (no `RELAY_LAUNCH_FD`): no Hello, registration is a no-op; the
  listener still serves direct clients.
- **Outbound.** To run a task, the scheduler dials relay's front door
  (`RELAY_FRONTEND_SOCKET`), which routes sessions and terminals to relayLLM.
  Under relay it is authenticated by identity and `--relay-token` is ignored;
  a relay-launched scheduler with no `RELAY_FRONTEND_SOCKET` is misregistered
  (no `frontend` capability) and exits non-zero rather than falling back to TCP;
  standalone it presents `--relay-token` / `RELAY_FRONTEND_TOKEN` as a bearer.
  Projects are referenced by id only; relay brokers the project-scoped token.

## Execution flow

1. `LoadAllTasks` schedules enabled tasks and resets any stale `running` status
   to `error`.
2. Every 30s, `checkAndFireTasks` fires due tasks. A run more than
   `missedThreshold` (10m) late is skipped unless `CatchUp`; a skipped `once`
   task is disabled. The `running` set prevents a task overlapping itself.
3. `executeTask` deletes the previous run's session/terminal, resolves the
   project, then:
   - **chat** → `CreateSession` (headless), then `RunChatAndWait` drives the turn
     over WS until `message_complete`, capped by `MaxDurationSeconds` (default
     30m). WS, not the blocking POST /message, because relayLLM caps that at 5
     minutes and slow local-model runs were falsely failed.
   - **PTY** → `CreateTerminal`, `AttachTerminalAndWait` for the exit code, then
     `GetTerminalLog` for a 16 KB preview.
   - Failures before a session/terminal exists go through `failRun`.
4. Log to `task-logs/`, set `LastStatus`, broadcast `task_started` /
   `task_completed` / `task_error`, then `rescheduleOrDisable` (re-reads the task
   so API edits made mid-run win).

Invariants worth keeping:
- `CalculateNextRun` returns past `once` times so catch-up works across restarts;
  `ValidateSchedule` is what rejects a past `once` at the API boundary.
- POST and PUT share `decodeTask`, so an update can't store a task create would reject.
- `store.Update` preserves run state (`LastRun`, `LastStatus`, `LastSessionID`,
  `LastTerminalID`) when the body omits it.

## Flags

```
--socket       Inbound Unix socket (default {data-dir}/relayscheduler.sock, env RELAY_SCHEDULER_SOCKET)
--token        Inbound bearer (auto-generated and never logged if empty, env RELAY_SCHEDULER_TOKEN)
--data-dir     Data directory (default os.UserConfigDir()/relayScheduler, env RELAY_SCHEDULER_DATA)
--relay-socket Outbound: relay frontend socket (env RELAY_FRONTEND_SOCKET)
--relay-token  Outbound: relay frontend bearer, standalone only (env RELAY_FRONTEND_TOKEN)
--relay-url    Outbound base URL when --relay-socket is empty (default http://localhost:3000, env RELAY_FRONTEND_URL)
```

Under relay, only `RELAY_FRONTEND_SOCKET` comes from the environment;
`--relay-token` is ignored, and `--socket`, `--token`, and `--data-dir` use
their defaults.

## Build and test

```bash
./build.sh        # builds, codesigns (hardened runtime), and registers with relay
go build .        # plain build
go test ./...     # hermetic: fake WS/HTTP servers, temp-dir stores
```

Keep `gofmt -l .` empty; `.gitattributes` forces LF on `.go` files.

## Ecosystem

- `../relay/` — orchestrator. Spawns relayScheduler, dispatches `/api/tasks*` and
  `/ws/tasks` to it via the registered manifest, and brokers project tokens.
- `../relayLLM/` — LLM engine. The sessions/terminals a task runs in execute here
  (reached through relay's front door, not directly). Terminal templates live in
  its `settings.json` `pty` section.
- `../eve/` — frontend. Reaches the task API through relay and bridges `/ws/tasks`
  to the browser for live task badges.
