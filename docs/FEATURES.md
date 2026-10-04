# relayScheduler feature map

What a person does with relayScheduler, where it surfaces, and the devbox
journey that proves it works today. A PR that adds or changes a feature
updates this file.

Columns:

- **Surface**: where the feature shows. `eve` is the web UI; `API` is the task
  API reached through relay's front door; `service` is the process itself.
- **Reach**: the user action or request that triggers it.
- **Door**: how a journey can drive it without the screen. Every task feature
  is `HTTP` on `/api/tasks*` through relay's front door (proxy-class
  credential), or `WS` on `/ws/tasks`.
- **Simple door**: what an everyday user does in eve, or `none`.
- **Power door**: the task API field or route, or `none`. relayScheduler has no
  CLI, so the API is the power door.
- **Journey**: an existing `devboxverify` journey, or `none`.

There is no Gate column, unlike relay's map. relayScheduler has no owner-gated
operation; the harness holds one proxy-class credential and nothing more.

Simple doors were checked against eve's UI code. Where eve shows less than
the API offers, the cell says so.

## User-visible features

| Feature | Surface | Reach | Door | Simple door | Power door | Journey |
|---|---|---|---|---|---|---|
| Terminal routine runs and ends; exit code and 16 KB output kept in history | eve, API | Project page > "+ New routine" > Advanced > Type "Terminal (shell)" > Template, Extra Args; the run ends and records `exitCode` and `response` | HTTP | eve Project page > Routines > "+ New routine" > Advanced > Terminal (shell). eve shows the last run's result and opens its terminal view; it does not show the exit code as a field | `POST /api/tasks` with `sessionType:"pty"`, `templateId`, `extraArgs` | scheduled-shell-routine-runs |
| Run now | eve, API | Run a routine on demand | HTTP | eve "Run Now" (Project page row button, Routines page sheet), "Run" in the New routine dialog's Routines tab, Today card "Refresh" | `POST /api/tasks/{id}/run` | scheduled-shell-routine-runs |
| Run history | eve, API | Look at what a run did | HTTP | eve shows the last run only: row result text, "Open last run" / "View Last Run", click on the row. No list of older runs | `GET /api/tasks/{id}/history` (newest first, 100 kept) | scheduled-shell-routine-runs |
| Delete a routine | eve, API | Remove a routine and its last session or terminal | HTTP | eve routine dialog, Routines tab > "Delete". Not on the Routines page or project row | `DELETE /api/tasks/{id}` | scheduled-shell-routine-runs |
| Edit a routine | eve, API | Change a routine; run state is kept | HTTP | eve "Edit" (Project page row, Routines page sheet, dialog Routines tab) > "Update routine" | `PUT /api/tasks/{id}` | none |
| Output file to `output` | eve, API | A terminal routine that writes `outputFile`; on exit 0 it is read into history | HTTP | eve dialog > Advanced > Terminal > "Output file (optional, shows as a card on Today)"; console projects only; result shows on a Today card | `outputFile` on `POST`/`PUT /api/tasks` | none |
| Run cap | eve, API | Stop a run that goes too long (default 30 min) | HTTP | eve dialog > Advanced > Terminal > "Timeout (minutes)". Terminal routines only; chat routines get the default | `maxDurationSeconds` | none |
| Chat routine (prompt, model) | eve, API | A headless session sends `prompt` on schedule | HTTP | eve "+ New routine" (Prompt, Model under Advanced > Chat (LLM)); "Make this a routine" from a session | `POST /api/tasks` headless | none |
| Blank model refused | eve, API | Create or edit a chat routine with no model | HTTP | eve toast "Choose a model before saving this routine." | API 400 on `POST`/`PUT` | none |
| Relay tools for chat routines | API | A chat routine may call relay tools | HTTP | none (eve keeps the flag on edit but has no control for it) | `useRelayTools` | none |
| Schedule types (7) | eve, API | Pick when a routine runs: daily, hourly, interval, weekly, cron, once, on demand | HTTP | eve dialog > Schedule | `schedule` | none |
| Enabled | eve, API | Pause or resume a routine | HTTP | eve dialog > "Enabled" checkbox | `enabled` | none |
| Catch up missed runs | API | Run a task missed by over 10 minutes | HTTP | none (eve has no control; it keeps the flag on a chat routine edit) | `catchUp` | none |
| List routines | eve, API | See a project's routines | HTTP | eve Project page > Routines; Routines page ("All routines") | `GET /api/tasks[?projectId=]` | none |
| Live run events | eve, API | Badges, Running, and failure notices update as runs start and end | WS | eve badges, Running part, failure notices | WS `/ws/tasks` (`task_started`, `task_completed`, `task_error`) | none |
| Open a run | eve, API | Join a chat run or watch a terminal run | HTTP | eve opens the run from "Open last run" | `view` field (`interactive` or `readonly`) | none |
| Service flags | service | Run standalone or relocate data and sockets | none | none | flags and env (`--socket`, `--data-dir`, ...) | none |

## Not user-visible

Background or API-only. No door a person reaches, so no simple door.

- Delete by project (`DELETE /api/tasks/by-project/{projectId}`), called by eve when a
  project is removed.
- Reset of a stale `running` status to `error` at startup.
- Disabling a missed `once` task.
- Launch identity and manifest registration with relay.
