# devboxverify

Drives the installed relayScheduler through relay's front door, against the
devboxWorld test world, and reports one result per journey. It posts the
`devbox/verify` status check to a PR.

It does not install anything. It checks that the running service is the build
under test and refuses a mismatch.

## Usage

    go run ./cmd/devboxverify [--checkout DIR] [--post PR]

- `--checkout` is the relayScheduler checkout the running service must be
  built from. Default: the tool's own repo root.
- `--post PR` posts an evidence comment and the `devbox/verify` commit status.
  The PR head must equal the checkout HEAD.

Exit codes: 0 when at least one journey ran and every journey passed; 1 on any
FAIL, BLOCKED or not-run journey, or no PASS; 2 on usage, preflight or post
failure, or an interrupted run (which posts nothing).

## Environment

| Variable | Default | Use |
|---|---|---|
| `RELAY_BIN` | `/Applications/Relay.app/Contents/MacOS/relay` | `relay grant --json` |
| `DEVBOXWORLD_MARKER` | `~/.config/devboxWorld/machine.json` | test-machine check and the world checkout |
| `RELAYSCHEDULER_VERIFY_CREDENTIAL_FILE` | `~/.config/relayScheduler-verify/credential` | proxy-class bearer; the file must be mode 0600 |
| `DEVLOCK_BIN` | `devlock` on PATH | the WORLD lock |

## Output

One tab-separated line per event on stdout. The home directory prints as `~`.
The credential is never printed.

    PREFLIGHT <name> OK|FAIL <detail>
    JOURNEY <id> PASS|FAIL|BLOCKED <detail>
    TIMING journey <id> <ms>
    TIMING run <ms>
    SUMMARY pass= fail= blocked= notrun=
    SUMMARY interrupted
    POSTED <state> <url>

Preflight runs in this order and the first FAIL exits 2: `machine`, `session`,
`head`, `build`, `app`, `credential`, `world`, `pr` (only with `--post`), then
`lock`, then `build` again, since the service may have been swapped while the
run waited for the lock.

The `lock` step takes the WORLD lock with `devlock take WORLD --wait` (waits at
most 15 minutes) and releases it when the run ends, including on Ctrl-C.

## Journey

`scheduled-shell-routine-runs`: creates an on-demand terminal routine in the
Acme project that prints a nonce marker, runs it, waits for its history entry,
checks exit code 0 and the marker in the output, then deletes the routine and
checks it is gone. A routine left behind fails the journey.

## Prerequisites

- A proxy-class credential at `RELAYSCHEDULER_VERIFY_CREDENTIAL_FILE`, mode
  0600. Mint it at the console:
  `relay credential mint --name devbox-verify-scheduler --class proxy --ttl 168h`
- The devboxWorld Acme project is granted in relay, and the `world-probe`
  terminal template is allowed for it.
- A test machine: a VM with the devboxWorld marker.

## Operator swap

The harness checks the build that is installed. To grade a PR build:

1. From the PR worktree: `go build -o <registered relayscheduler path> .`
2. `relay service restart --id relay-scheduler`
3. Run the harness.
4. Restore: build from the `main` checkout the same way and restart again.

`build.sh` is not used for the swap: it re-registers, which raises a presence
prompt. If relay refuses a plain `go build` binary, sign it as `build.sh` does.
