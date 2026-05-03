# AGENTS.md

## Working Style
- State assumptions explicitly before coding when behavior is ambiguous. If multiple interpretations are possible, call them out instead of picking silently.
- Push back when a requested approach is brittle, unsafe, or overcomplicated; explain the downside and propose a safer path.
- Prefer the smallest change that solves the verified problem. No speculative features, abstractions, or broad error handling for undefined scenarios.
- Prefer boring, obvious code over clever code.
- Keep edits surgical: touch only files and lines needed for the task, match the existing style, and do not clean up unrelated code while you are there.
- Remove only the unused code your own change creates. If you notice unrelated dead code or problems, mention them separately instead of folding them into the edit.
- Define success in verifiable terms before changing code. For bug fixes, reproduce with a test or other concrete check first when practical, then verify the fix with the narrowest useful command.
- For multi-step work, keep a brief goal-driven plan with a verification check for each step.
- Use subagents for independent research, review, or implementation; prefer parallel execution for separable tasks.
- Lead reports with the outcome, then what changed and how it was verified. Call out assumptions, tradeoffs, or risks only when they matter.
- When corrected, add a concise entry here so future sessions avoid repeating the mistake.

## Project Shape
- This is a Go module under `./acexy`, not at the repository root; run Go commands with `workdir=/home/mdn/Code/acexy/acexy`.
- The binary entrypoint is `acexy/proxy.go`; library code lives in `acexy/lib/acexy` and the parallel multi-writer is `acexy/lib/pmw`.
- Docker builds from the repo root because `Dockerfile` uses `COPY --link acexy/ ./`; do not move `go.mod` to root unless also changing Docker/CI.

## Commands
- Download deps: `go mod download` from `./acexy`.
- Build like CI: `go build -v ./...` from `./acexy`.
- Full test like CI: `go test -v ./...` from `./acexy`.
- Race check like CI: `go test -race ./...` from `./acexy`.
- Focused test: `go test ./... -run TestName` from `./acexy`; package-scoped examples are `go test . -run TestHandleStatus` and `go test ./lib/acexy -run TestFetchStream`.
- Local run: `go run .` from `./acexy`; useful flags/env are defined in `proxy.go` and can be listed with `go run . -help`.

## Runtime And API Notes
- Acexy proxies AceStream middleware endpoints behind `/ace/getstream` and exposes `/ace/status`; callers must provide exactly one of `id` or `infohash` where required.
- Clients must not pass `pid`; `GetStream` injects a unique UUID `pid` before calling the AceStream backend.
- MPEG-TS mode is default; M3U8 is selected by `-m3u8` or `ACEXY_M3U8=true` and is documented as experimental.
- Invalid env values intentionally fall back to defaults instead of crashing; keep that behavior when changing config parsing.

## Concurrency/Test Gotchas
- Stream state in `lib/acexy.Acexy` is shared and mutex-protected; avoid holding `a.mutex` across HTTP calls to the AceStream backend.
- `PMultiWriter` evicts slow writers after its write timeout; healthy clients should keep receiving data when another client stalls or disconnects.
- Regression tests in `repro_test.go` use `httptest` backends and include sleeps/timeouts, so the full suite can take noticeably longer than small unit tests.
