# relayScheduler (Go)

Task scheduling service. Reads `.tasks.json` from project directories, runs LLM prompts on schedule via relayLLM HTTP API.

## Architecture

```
main.go        Entry point, flag parsing, project polling, HTTP server
client.go      relayLLM HTTP client (projects, sessions, messages)
task.go        Task and execution data types
schedule.go    Next-run calculation (daily, hourly, interval, weekly, cron)
scheduler.go   Core scheduler: load tasks, file watch, execute, reschedule
logstore.go    Execution history persistence (JSON files, 100-entry retention)
api.go         HTTP routes for task listing, history, manual runs
```

## How It Works

1. Polls relayLLM `/api/projects` to discover project directories
2. Reads `.tasks.json` from each project directory
3. Schedules enabled tasks using `time.AfterFunc`
4. On trigger: creates headless session via relayLLM, sends prompt, captures response
5. Logs execution to `task-logs/{projectId}-{taskId}.json`
6. Reschedules for next run
7. Watches `.tasks.json` files for live changes (debounced, via fsnotify)

## Schedule Types

- `daily` — `{"type":"daily","time":"09:00"}`
- `hourly` — `{"type":"hourly","minute":30}`
- `interval` — `{"type":"interval","minutes":15}`
- `weekly` — `{"type":"weekly","day":"monday","time":"09:00"}`
- `cron` — `{"type":"cron","expression":"30 14 * * *"}` (simplified)

## API

```
GET  /api/tasks                          — list scheduled tasks
GET  /api/tasks/:projectId/:taskId/history — execution history
POST /api/tasks/:projectId/:taskId/run   — run task immediately
```

## Flags

```
--llm-url   relayLLM base URL (default: http://localhost:3001, env: RELAY_LLM_URL)
--port      HTTP API port (default: 3002, env: RELAY_SCHEDULER_PORT)
--data-dir  Data directory (default: ~/.config/relayScheduler, env: RELAY_SCHEDULER_DATA)
--poll      Project poll interval (default: 30s)
```

## Build

```bash
go build .
```
