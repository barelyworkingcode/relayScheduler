# Code Review Log

## 2026-03-26 — HIGH priority fixes

### 1. Concurrent WebSocket write race (`hub.go`)
- **Bug:** `SendStatus` wrote to a WebSocket connection without holding `h.mu`, while `Broadcast` (called from `executeTask` goroutines) wrote to the same connections under `h.mu`. Gorilla websocket forbids concurrent writers — this could cause panics in production.
- **Fix:** Hold `h.mu` around the `WriteMessage` call in `SendStatus`.

### 2. `RunTaskNow` silent success for missing tasks (`scheduler.go`)
- **Bug:** Returned `nil` error when a task wasn't found, causing the API to return `200 success` for manual runs against non-existent task IDs.
- **Fix:** Return `fmt.Errorf("task not found: %s", taskID)` so the API handler returns a 500 with a clear message.

## 2026-03-26 — HIGH priority fix (round 2)

### 1. No concurrent execution guard (`scheduler.go`, `api.go`)
- **Bug:** `RunTaskNow` could fire a task already in progress (from a scheduled run or prior manual trigger). A scheduled tick could also fire a task already running via manual trigger. This caused: (a) `executeTask` calling `EndSession` on the live session of the first execution, killing it mid-flight; (b) two goroutines writing the same LogStore file with no synchronization, losing log entries; (c) conflicting `SetLastRun` status updates; (d) duplicate WebSocket broadcasts confusing clients.
- **Fix:** Added a `running` map to `Scheduler` tracking taskIDs currently executing. `checkAndFireTasks` skips tasks already in the running set and marks fired tasks before releasing the lock. `RunTaskNow` atomically checks+marks under the lock, returning an error if already running. `executeTask` clears the flag via defer. API returns 409 Conflict for the already-running case.

## 2026-03-26 — HIGH priority fixes (round 3)

### 1. Non-atomic file writes can corrupt `tasks.json` on crash (`store.go`, `logstore.go`)
- **Bug:** `os.WriteFile` is not atomic. A crash or kill mid-write leaves a partially-written file that `json.Unmarshal` can't parse, losing all task definitions (or log history).
- **Fix:** Write to a `.tmp` file first, then `os.Rename` (atomic on POSIX). Applied to both `TaskStore.writeLocked` and `LogStore.Log`.

### 2. Run endpoint returns 500 for nonexistent task (`api.go`)
- **Bug:** `POST /api/tasks/{id}/run` for a missing task returned 500 (server error). Only "already running" was special-cased for 409; "not found" fell through to the generic 500 branch.
- **Fix:** Added check for "not found" in the error string to return 404.

### 3. DRY: collapsed three identical setter methods (`store.go`)
- **Issue:** `SetLastRun`, `SetLastSessionID`, `SetEnabled` each repeated the same lock/read/find-by-ID/mutate/write pattern (~65 lines total).
- **Fix:** Extracted `updateTask(id, fn)` helper; setters are now one-liners delegating to it (~30 lines total).

## 2026-03-26 — HIGH priority fix (round 4)

### 1. on_demand tasks auto-fire on creation and get incorrectly disabled (`scheduler.go`, `schedule.go`)
- **Bug (a):** `CalculateNextRun` returned `now` for `on_demand`, so `scheduleTaskLocked` treated it as immediately due. Any on_demand task would fire within 30 seconds of creation (or on server restart), consuming LLM resources and creating an unrequested session.
- **Bug (b):** `rescheduleOrDisable` grouped `on_demand` with `once`, disabling the task after execution. This made on_demand tasks show as disabled in the UI after their first (unwanted) run, confusing the intended "run only when manually triggered" semantics.
- **Fix:** (1) `scheduleTaskLocked` now skips `on_demand` tasks entirely — they never enter the tick-based schedule. (2) `rescheduleOrDisable` only disables `once` tasks; `on_demand` tasks return early without disabling or rescheduling. (3) `CalculateNextRun` returns an error for `on_demand` as a safety net. Manual runs via `RunTaskNow` are unaffected.

## 2026-03-26 — HIGH priority fix (round 5)

### 1. Stale task data in `rescheduleOrDisable` causes zombie tasks and reverted updates (`scheduler.go`)
- **Bug:** `rescheduleOrDisable` used the task copy captured at fire time. If the task was deleted, updated (schedule/config changed), or disabled via API during execution: (a) deleted tasks were resurrected in the scheduler and executed indefinitely in an infinite loop, consuming unbounded LLM resources; (b) API updates to the schedule were silently reverted by the stale copy on reschedule; (c) disabling a running task via API had no effect — it was rescheduled with `Enabled: true` from the stale copy.
- **Fix:** `rescheduleOrDisable` now re-reads the task from the store before rescheduling. If the task was deleted (`nil`), has an error, or was disabled, it returns immediately without rescheduling.

### 2. Fragile string-matching for API error classification (`scheduler.go`, `api.go`)
- **Issue:** `RunTaskNow` returned plain `fmt.Errorf` strings, and the API handler used `strings.Contains` to classify errors into 404/409/500 status codes. Changing an error message would silently break status code mapping.
- **Fix:** Introduced `ErrTaskNotFound` and `ErrTaskRunning` sentinel errors. `RunTaskNow` wraps them with `%w`. API handler uses `errors.Is()` for classification — idiomatic Go and refactor-safe.

## 2026-03-26 — HIGH + cleanup (round 6)

### 1. No schedule validation on Create/Update API (`api.go`, `schedule.go`)
- **Bug:** `POST /api/tasks` and `PUT /api/tasks/{id}` accepted any JSON blob as `schedule` without validation. A task with `{"type":"bogus"}` would get `201 Created`, be persisted to disk, but silently never schedule — no error feedback to the user.
- **Fix:** Added `ValidateSchedule()` which checks the schedule type is known and parameters are valid (delegates to `CalculateNextRun` for schedulable types, passes `on_demand` through). Create and Update handlers now return 400 for invalid schedules.

### 2. DRY: Schedule type parsed inline 3 times (`scheduler.go`, `schedule.go`)
- **Issue:** `scheduleTaskLocked`, `rescheduleOrDisable`, and `CalculateNextRun` each independently unmarshalled `{"type":"..."}` from raw JSON. Two of the three sites ignored the error.
- **Fix:** Extracted `ScheduleType()` helper. All three call sites now use it. Removed `encoding/json` import from `scheduler.go`.

### 3. Dead code removed (`scheduler.go`, `store.go`)
- `GetAllTasks()` — defined on Scheduler but never called from any file. Removed (~17 lines).
- `List()` — trivial alias for `Load()`. Removed, updated the one caller in `api.go` to call `Load()` directly.

## 2026-03-26 — HIGH priority fix (round 7)

### 1. Cron parser silently misinterprets unsupported expressions (`schedule.go`)

- **Bug:** `nextCron` used `strconv.Atoi` on cron fields and discarded parse errors with `_`. Expressions like `*/5`, `*/2`, or `1-5` silently converted to 0 via `strconv.Atoi`, causing tasks to run at entirely wrong times. Day-of-week, day-of-month, and month fields were completely ignored. A malformed expression with <2 fields fell back to "run in 1 hour" with no error. Since `ValidateSchedule` delegates to `CalculateNextRun`, all these expressions passed validation — the task was accepted, persisted, and silently ran at wrong times.
  - `"*/5 * * * *"` (every 5 min) → actually ran hourly at :00
  - `"0 */2 * * *"` (every 2 hours) → actually ran daily at 00:00
  - `"0 0 * * 1"` (Mondays at midnight) → actually ran daily at midnight
- **Fix:** Changed `nextCron` to return `(time.Time, error)`. Now validates: exactly 5 fields required, rejects step/range/list syntax (`/`, `-`, `,`), rejects non-wildcard day/month/dow fields (with guidance to use `weekly`/`daily` types), rejects wildcard minute, validates minute 0-59 and hour 0-23. Errors propagate through `CalculateNextRun` → `ValidateSchedule`, so unsupported expressions are rejected at the API boundary with clear error messages.

## 2026-03-27 — HIGH priority fixes (round 8)

### 1. `parseTime` silently swallows errors — invalid daily/weekly schedules execute at wrong times (`schedule.go`)
- **Bug:** Same class as the cron fix in round 7, but for daily/weekly code paths. `parseTime` returned `(0,0)` for any unparseable string with no error. `nextWeekly` silently defaulted to Monday for invalid day names. A task with `{"type":"daily","time":"abc:def"}` or `{"type":"weekly","day":"notaday","time":"25:99"}` passed `ValidateSchedule` and silently scheduled at midnight Monday instead of being rejected.
- **Fix:** `parseTime` now returns `(int, int, error)`, validating format (HH:MM), hour (0-23), and minute (0-59). `nextDaily` and `nextWeekly` return `(time.Time, error)` and propagate. `nextWeekly` rejects unknown day names. Also added validation for `hourly` minute (0-59) and `interval` minutes (must be positive) in `CalculateNextRun` — previously `minute:99` silently normalized and `minutes:-5` silently became 60.

### 2. No write deadline on WebSocket connections — zombie client blocks all broadcasts (`hub.go`)
- **Bug:** `Broadcast` held `h.mu` while iterating and calling `WriteMessage` on every client. A dead/stalled TCP connection could hang `WriteMessage` for minutes (OS-level TCP timeout). During the hang, `h.mu` was locked, blocking all broadcasts, registrations, and unregistrations — one zombie client froze real-time updates for every connected client.
- **Fix:** Added 10-second write deadline (`conn.SetWriteDeadline`) before each `WriteMessage` in both `Broadcast` and `SendStatus`. Stalled connections now fail fast and get removed.
