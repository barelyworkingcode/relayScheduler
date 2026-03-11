# relayScheduler

Task scheduling service for relayLLM. Reads `.tasks.json` from project directories, runs LLM prompts on schedule via the relayLLM HTTP API. Managed as a background service by [Relay](https://github.com/barelyworkingcode/relay).

## Build

```bash
go build .
./build.sh    # build + register with Relay
```

## Configuration

| Flag | Env Var | Default | Description |
|------|---------|---------|-------------|
| `--llm-url` | `RELAY_LLM_URL` | `http://localhost:3001` | relayLLM base URL |
| `--port` | `RELAY_SCHEDULER_PORT` | `3002` | HTTP API port |
| `--data-dir` | `RELAY_SCHEDULER_DATA` | `~/.config/relayScheduler` | Data directory |
| `--poll` | - | `30s` | Project poll interval |

## Task File

Place a `.tasks.json` in any relayLLM project directory:

```json
[
  {
    "id": "daily-summary",
    "name": "Daily Summary",
    "prompt": "Summarize today's changes",
    "model": "sonnet",
    "enabled": true,
    "schedule": {"type": "daily", "time": "09:00"}
  }
]
```

## Schedule Types

| Type | Fields | Example |
|------|--------|---------|
| `daily` | `time` | `{"type":"daily","time":"09:00"}` |
| `hourly` | `minute` | `{"type":"hourly","minute":30}` |
| `interval` | `minutes` | `{"type":"interval","minutes":15}` |
| `weekly` | `day`, `time` | `{"type":"weekly","day":"monday","time":"09:00"}` |
| `cron` | `expression` | `{"type":"cron","expression":"30 14 * * *"}` |

## API

```
GET  /api/tasks                              -- list scheduled tasks
GET  /api/tasks/:projectId/:taskId/history   -- execution history
POST /api/tasks/:projectId/:taskId/run       -- run task immediately
```

## Ecosystem

relayScheduler is part of the Relay ecosystem. It requires [relayLLM](https://github.com/barelyworkingcode/relayLLM) for project discovery and LLM execution.

- **[Relay](https://github.com/barelyworkingcode/relay)** -- MCP orchestrator. Manages relayScheduler as a background service.
- **[relayLLM](https://github.com/barelyworkingcode/relayLLM)** -- LLM engine. Provides the projects and sessions API that relayScheduler calls.

## License

[MIT](./LICENSE)
