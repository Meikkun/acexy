# AGENTS.md

Behavioral guidelines for AI coding agents working on Acexy. The agent is the hands; the human is the architect. Favor reliability, simplicity, and reviewable diffs over raw speed.

## 1. Think Before Coding

- State assumptions before non-trivial work. If multiple interpretations exist, present options and ask.
- If files, tests, or requirements conflict, stop and name the conflict.
- Push back when the requested approach is brittle, unsafe, or overcomplicated; explain the downside and propose a safer path.

## 2. Work From Success Criteria

- Define expected outcome before editing. For bugs, reproduce first. For features, define inputs, outputs, and edge cases.
- For multi-step work, keep a brief plan with a verification check for each step.
- Use subagents for independent research, review, or implementation; prefer parallel execution for separable tasks.

## 3. Keep It Simple

- Write the smallest clear solution that satisfies the criteria.
- No speculative features, abstractions, or broad error handling for undefined scenarios.
- Prefer boring, obvious code over clever code.

## 4. Make Surgical Changes

- Touch only files and lines needed for the task. Match existing style.
- Do not reformat, rename, or "improve" adjacent code unless asked.
- Remove only unused code your own change creates. Mention unrelated issues separately.

## 5. Validate Before Claiming Done

- Run the relevant build, tests, or race check. Add or update tests when behavior changes.
- Do not treat compilation as proof of correctness.
- Lead reports with the outcome, then what changed and how it was verified.

## 6. Learn From Corrections

When corrected, add a concise entry to "Learned" below so future sessions avoid repeating the mistake.

---

## Project Shape

- Go module under `./acexy`, not at the repository root. Run Go commands with `workdir=/home/mdn/Code/acexy/acexy`.
- Binary entrypoint: `acexy/proxy.go`. Library: `acexy/lib/acexy`. Parallel multi-writer: `acexy/lib/pmw`.
- Docker builds from repo root (`Dockerfile` uses `COPY --link acexy/ ./`); do not move `go.mod` to root unless also changing Docker/CI.

## Commands

```bash
# All from ./acexy (workdir=/home/mdn/Code/acexy/acexy)
go mod download          # deps
go build -v ./...        # build
go test -v ./...         # full tests
go test -race ./...      # race check
go test ./... -run TestName              # focused test
go test . -run TestHandleStatus          # package-scoped
go test ./lib/acexy -run TestFetchStream # library-scoped
go run . -help           # flags/env reference
```

## Runtime & API Notes

- Proxies AceStream middleware behind `/ace/getstream`; exposes `/ace/status`.
- Callers must provide exactly one of `id` or `infohash`. Clients must NOT pass `pid` (injected automatically as UUID).
- MPEG-TS is default; M3U8 is experimental (`-m3u8` / `ACEXY_M3U8=true`).
- Invalid env values fall back to defaults instead of crashing. Preserve this behavior.

## Concurrency Gotchas

- `lib/acexy.Acexy` stream state is mutex-protected. Never hold `a.mutex` across HTTP calls to the AceStream backend.
- `Broadcaster` evicts slow clients via bounded queues. Healthy clients must keep receiving data when another stalls.
- Regression tests in `repro_test.go` include sleeps/timeouts; the full suite takes longer than unit tests.

## Learned

_(Add entries here when corrected)_
