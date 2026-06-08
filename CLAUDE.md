# relayScheduler (Go)

Task scheduling service. Stores tasks in a single `tasks.json`, runs them on
schedule by creating sessions/terminals through relay's front door (which routes
them to relayLLM), and broadcasts task lifecycle events over WebSocket.

## Architecture

```
main.go               Entry point, flags, Unix-socket listener, manifest registration
client.go             Outbound client to relay's frontend socket (projects, sessions, terminals)
task.go               Task + execution data types (chat and PTY session types)
schedule.go           Next-run calculation (daily, hourly, interval, weekly, cron, once, on_demand)
scheduler.go          Core scheduler: load tasks, execute, capture output, reschedule, broadcast
store.go              Mutex-protected CRUD over a single tasks.json file
logstore.go           Execution history persistence (per-task JSON, capped retention)
hub.go / wshandler.go WebSocket hub + upgrade handler (task lifecycle broadcasts)
api.go                HTTP routes for task CRUD, history, manual runs
manifest.go           Front-door manifest (routes) + RegisterManifest call
relay_bridge_client.go Minimal relay bridge client (service-token auth) for registration
auth.go               Bearer-auth middleware + token generation for the inbound listener
```

## Relay front-door integration

relayScheduler is a relay-enhanced service (see `../relay/plans/service-manifest-spec.md`).

- It serves its HTTP + WS API on a **Unix socket** (`--socket`, default
  `{data-dir}/relayscheduler.sock`), bearer-authed (`auth.go`).
- When relay spawns it, relay injects `RELAY_BRIDGE_SOCKET` + `RELAY_SERVICE_ID`
  + `RELAY_SERVICE_TOKEN`. The scheduler dials the bridge and sends
  `RegisterManifest` (`manifest.go`) declaring its routes:
  `/api/tasks`, `/api/tasks/`, `/ws/tasks`. relay's front-door dispatcher
  reverse-proxies those routes to the scheduler's socket (stripping the inbound
  frontend bearer and injecting the scheduler-declared internal token).
- **`/ws/tasks`, not `/ws`** — relayLLM already claims `/ws` and relay rejects
  duplicate routes. eve's server opens a second upstream WS to `/ws/tasks` and
  forwards task lifecycle frames to the browser (eve `relay-client.js`); the
  browser only ever holds one socket to eve.
- **Standalone** (no `RELAY_BRIDGE_SOCKET`): registration is a clean no-op; the
  listener still serves on its socket for direct clients.
- For *outbound* work (creating the session/terminal a task runs in), the
  scheduler dials relay's **frontend** socket (`RELAY_FRONTEND_SOCKET` /
  `RELAY_FRONTEND_TOKEN`), which relay routes to relayLLM. It references projects
  by id only; relay brokers the project-scoped token (see ADR-007 in `../relay`).

## How It Works

1. Load tasks from `tasks.json` via the `TaskStore`; schedule the enabled ones.
2. On trigger, resolve the project from relay (`GetProject` by id), then:
   - **chat** tasks → create a headless session, then drive the turn over the WS
     event stream (`RunChatAndWait`), accumulating the reply until completion,
     capped by `MaxDurationSeconds` (default 30m). Uses WS rather than the
     blocking POST /message so slow local-model runs aren't falsely failed by
     relayLLM's 5-minute synchronous response cap.
   - **PTY** tasks → create a terminal from a template, attach via WS, wait for exit.
3. Log execution to `task-logs/`, broadcast `task_started` / `task_completed` /
   `task_error` on the hub (delivered to eve via `/ws/tasks`).
4. Reschedule for the next run.

## Schedule Types

- `daily` — `{"type":"daily","time":"09:00"}`
- `hourly` — `{"type":"hourly","minute":30}`
- `interval` — `{"type":"interval","minutes":15}`
- `weekly` — `{"type":"weekly","day":"monday","time":"09:00"}`
- `cron` — `{"type":"cron","expression":"30 14 * * *"}` (simplified)
- `once` — `{"type":"once","at":"2026-01-01T09:00:00Z"}`
- `on_demand` — run only via the manual-run API

## API

```
GET    /api/tasks                       — list tasks (optional ?projectId=)
POST   /api/tasks                       — create task
GET    /api/tasks/{id}                  — get task
PUT    /api/tasks/{id}                  — update task
DELETE /api/tasks/{id}                  — delete task
GET    /api/tasks/{id}/history          — execution history
POST   /api/tasks/{id}/run              — run task immediately
DELETE /api/tasks/by-project/{projectId} — delete all tasks for a project
WS     /ws/tasks                        — task lifecycle event stream
```

## Flags

```
--socket       Inbound Unix socket (default {data-dir}/relayscheduler.sock, env RELAY_SCHEDULER_SOCKET)
--token        Inbound bearer (auto-generated if empty, env RELAY_SCHEDULER_TOKEN)
--data-dir     Data directory (default ~/Library/Application Support/relayScheduler, env RELAY_SCHEDULER_DATA)
--relay-socket Outbound: relay frontend socket (env RELAY_FRONTEND_SOCKET)
--relay-token  Outbound: relay frontend bearer (env RELAY_FRONTEND_TOKEN)
--relay-url    Outbound base URL when --relay-socket is empty (default http://localhost:3000, env RELAY_FRONTEND_URL)
```

Under relay, all of these are supplied via the environment at spawn; flags are for
standalone/dev runs.

## Build

```bash
./build.sh        # builds, codesigns (hardened runtime), and registers with relay
go build .        # plain build
```

## Headless Sessions

`client.go` sends `settings: {headless: true}` on `CreateSession`. relayLLM maps
that to CLI flags / hook behavior -- the scheduler stays unaware of CLI specifics.

## Ecosystem

relayScheduler is part of the Relay ecosystem.

- `../relay/` -- orchestrator. Spawns relayScheduler, dispatches `/api/tasks*` and
  `/ws/tasks` to it via the registered manifest, and brokers project tokens.
- `../relayLLM/` -- LLM engine. The sessions/terminals a task runs in execute here
  (reached through relay's front door, not directly).
- `../eve/` -- frontend. Reaches the task API through relay and bridges `/ws/tasks`
  to the browser for live task badges.
