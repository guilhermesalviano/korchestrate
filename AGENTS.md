# AGENTS.md

Guidance for agent sessions working in this repository.

## Build and test

```sh
go build ./...   # compile every package
go vet ./...     # static checks
go test ./...    # full suite (packages are fast)
```

`kor` is a Go CLI. `cmd/kor` wires the cobra commands, `internal/pipeline`
drives the plan → execute → review state machine, `internal/agent` adapts the
agent CLIs, `internal/artifact` persists runs, and `internal/tui` /
`internal/web` render the dashboards. Match the surrounding style: comments
explain why, not what, and user-facing strings use plain ASCII.

## Run artifacts

Each run lives in `~/.local/state/korchestrate/runs/<run-id>/` (override with
`--artifacts-dir` or `artifacts_dir` in the config) and contains:

- `run.json` — the run record. `state`, `iteration`, `commit`, `usage`, and
  `errors`: an append-only history of `RunError` (`at`, `stage`, `agent`,
  `message`) that keeps recovered failures too. `error` is the most recent
  fatal message.
- `run.log.jsonl` — the audit timeline: one `artifact.LogEntry` per line.
  `artifact.ReadLog(dir)` parses it and tolerates malformed lines. Levels:
  `info`, `warn`, `error`. Stages: `run`, `preflight`, `worktree`, `planner`,
  `executor`, `reviewer`, `publish`, `commit`, `push`, `pr`. Events:
  `run.start`, `run.prompt`, `run.done`, `run.failed`, `run.aborted`,
  `state`, `stage.start`, `stage.end`, `stage.failed`, `stage.retry`,
  `agent.fallback`, `gate.plan`, `gate.review`, `review.verdict`,
  `executor.no_changes`, `executor.no_report`, `commit.done`, `push.done`,
  `pr.opened`, `worktree.*`, and so on.
- `plan.json` / `plan.md`, `diff.patch` and `diff.<iter>.patch`,
  `executor.report.<iter>.json`, `review.<iter>.json` / `review.json` — stage
  artifacts a resume reads.
- `*.events*.jsonl` — raw agent output per stage and iteration; noisy but the
  complete transcript.

## Writing logs

- Use the pipeline helpers in `internal/pipeline/runlog.go`
  (`logf`, `logError`, `logStageStart`, `logStageEnd`, `logStageRetry`) rather
  than touching `run.log.jsonl` directly. `runStage` records adapter failures
  and fallbacks already.
- `(*artifact.Run).SetState` logs state transitions automatically and
  `(*artifact.Run).AddError` appends to both `run.json` errors and the
  timeline. Record recovered failures (fallback agent, failed push, failed PR)
  with `AddError`; they must show up in `kor logs --errors`.
- Logging is best-effort: never fail a run because a log write failed.
- Keep messages short (they are capped at 4 KiB). Put large payloads in an
  artifact and reference it. Prompts and agent output are already on disk;
  never log secrets or credentials.

## Finding errors

```sh
kor list --failed              # failed/aborted runs with their latest error
kor status <run-id>            # error history and the log file path
kor logs <run-id> --errors     # warnings and errors with stage and timestamps
kor logs <run-id> --json | jq  # machine-readable timeline
kor logs <run-id> --follow     # tail an active run
```

Runs created before the timeline existed have no `run.log.jsonl`; `ReadLog`
returns no entries for them, and `kor status` falls back to the single `error`
field.
