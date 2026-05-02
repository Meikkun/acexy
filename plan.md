# Reliability Hardening Execution Checklist

Use this checklist in order. Do not skip steps. Do not continue after a failed gate.

## Step 0 - Session Bootstrap (run once)

```bash
cd /home/mdn/Code/acexy
export ROOT="$PWD"
export MOD="$ROOT/acexy"
export ART="$ROOT/.review-artifacts/$(date +%Y%m%d-%H%M%S)"
mkdir -p "$ART"
git rev-parse --is-inside-work-tree
test -d "$MOD"
echo "ROOT=$ROOT"
echo "MOD=$MOD"
echo "ART=$ART"
```

PASS: `git rev-parse` prints `true`, `test -d "$MOD"` exits 0.
FAIL: stop and fix path/repo location before anything else.

---

## Step 1 - Create/Enter Working Branch

```bash
git checkout -b fix/reliability-hardening || git checkout fix/reliability-hardening
git branch --show-current | tee "$ART/01-branch.txt"
git status --short | tee "$ART/01-status-start.txt"
```

PASS: branch is `fix/reliability-hardening`.
FAIL: stop and resolve branch checkout issue.

---

## Step 2 - Capture Baseline Evidence (before coding)

```bash
(cd "$MOD" && go test ./...) 2>&1 | tee "$ART/02-baseline-go-test.txt"
test ${PIPESTATUS[0]} -eq 0

(cd "$MOD" && go vet ./...) 2>&1 | tee "$ART/02-baseline-go-vet.txt"
test ${PIPESTATUS[0]} -eq 0

(cd "$MOD" && go test -race ./...) 2>&1 | tee "$ART/02-baseline-go-test-race.txt" || true
grep -q "DATA RACE" "$ART/02-baseline-go-test-race.txt"
```

PASS: first two commands exit 0; race log contains `DATA RACE`.
FAIL: if first two fail, stop and investigate baseline environment first.

---

## Step 3 - Phase 1 Implementation (tests first)

Create these files with these exact test names:
- `acexy/proxy_env_test.go`
- `acexy/proxy_status_test.go`
- `acexy/lib/acexy/fetchstream_test.go`
- `acexy/lib/acexy/copier_test.go`

Required test names:
- `TestLookupEnvOrInt_InvalidUsesDefault`
- `TestLookupEnvOrDuration_InvalidUsesDefault`
- `TestLookupEnvOrBool_InvalidUsesDefault`
- `TestLookupEnvOrSize_InvalidUsesDefaultAndNonNil`
- `TestLookupLogLevel_InvalidUsesInfo`
- `TestHandleStatus_GlobalWhenNoParams`
- `TestHandleStatus_WithID`
- `TestHandleStatus_WithInfohash`
- `TestHandleStatus_BothIDAndInfohashReturnsBadRequest`
- `TestHandleStatus_EmptyValueReturnsBadRequest`
- `TestFetchStream_ConcurrentSameID_NoBackendLeak`
- `TestCopier_FlushesOnEOF`
- `TestCopier_TimeoutClosesSourceAndDestination`

Validation commands:

```bash
(cd "$MOD" && go test -list 'Test(LookupEnvOr|LookupLogLevel|HandleStatus|FetchStream_ConcurrentSameID_NoBackendLeak|Copier_)' ./...) \
  | tee "$ART/03-test-list.txt"

grep -q 'TestLookupEnvOrSize_InvalidUsesDefaultAndNonNil' "$ART/03-test-list.txt"
grep -q 'TestHandleStatus_BothIDAndInfohashReturnsBadRequest' "$ART/03-test-list.txt"
grep -q 'TestFetchStream_ConcurrentSameID_NoBackendLeak' "$ART/03-test-list.txt"
grep -q 'TestCopier_FlushesOnEOF' "$ART/03-test-list.txt"

(cd "$MOD" && go test ./... -run 'TestLookupEnvOrSize_InvalidUsesDefaultAndNonNil|TestHandleStatus_BothIDAndInfohashReturnsBadRequest' -count=1) \
  2>&1 | tee "$ART/03-expected-red.txt" || true
test ${PIPESTATUS[0]} -ne 0
```

PASS: tests are discoverable; selected known-regression tests fail pre-fix.
FAIL: if tests not listed, fix test names/file locations first.

Commit checkpoint:

```bash
git add acexy/proxy_env_test.go acexy/proxy_status_test.go acexy/lib/acexy/fetchstream_test.go acexy/lib/acexy/copier_test.go
git commit -m "test: add regression coverage for env parsing, status validation, copier, and concurrent stream fetch"
```

PASS: commit created successfully.
FAIL: fix compile/style errors and recommit.

---

## Step 4 - Phase 2 Implementation (env parsing hardening in `acexy/proxy.go`)

Implement:
- Parse-error fallback to defaults for int/duration/bool helpers.
- `LookupEnvOrSize` must never return `nil`.
- No panic path for invalid `ACEXY_BUFFER_SIZE`.
- Add/keep sane argument validations.

Validation:

```bash
(cd "$MOD" && go test ./... -run 'TestLookupEnvOr|TestLookupLogLevel' -count=1) \
  2>&1 | tee "$ART/04-env-tests.txt"
test ${PIPESTATUS[0]} -eq 0

(cd "$MOD" && ACEXY_BUFFER_SIZE=notasize go run . -help >"$ART/04-invalid-buffer-help.txt" 2>&1)
test $? -eq 0
! grep -q 'panic:' "$ART/04-invalid-buffer-help.txt"
```

PASS: env tests green; invalid buffer size does not panic.
FAIL: stop and fix env helper behavior.

Commit checkpoint:

```bash
git add acexy/proxy.go
git commit -m "fix: make env parsing resilient and prevent startup panic on invalid buffer size"
```

---

## Step 5 - Phase 3 Implementation (`/ace/status` strict validation in `acexy/proxy.go`)

Implement:
- no params => global status.
- exactly one valid param (`id` or `infohash`) => stream status.
- both set => `400`.
- present-but-empty value => `400`.

Validation:

```bash
(cd "$MOD" && go test ./... -run 'TestHandleStatus_' -count=1) \
  2>&1 | tee "$ART/05-status-tests.txt"
test ${PIPESTATUS[0]} -eq 0

PORT=18080
(cd "$MOD" && ACEXY_LISTEN_ADDR=":$PORT" timeout 15s go run . >"$ART/05-status-server.log" 2>&1) &
srv=$!
sleep 2
code=$(curl -s -o /dev/null -w "%{http_code}" "http://127.0.0.1:$PORT/ace/status?id=a&infohash=b")
kill "$srv" >/dev/null 2>&1 || true
test "$code" = "400"
```

PASS: tests green; manual request returns HTTP 400.
FAIL: stop and fix handler logic.

Commit checkpoint:

```bash
git add acexy/proxy.go acexy/proxy_status_test.go
git commit -m "fix: enforce strict /ace/status query validation rules"
```

---

## Step 6 - Phase 4 Implementation (remove ResponseWriter race path)

Implement in `acexy/proxy.go`, `acexy/lib/acexy/acexy.go`, and related tests:
- Decouple direct fanout writes from `http.ResponseWriter`.
- Ensure per-client output path is race-safe.
- Ensure writer cleanup order is deterministic.

Validation:

```bash
(cd "$MOD" && go test -race . -run 'TestDeadlockReproduction|TestMultiClientStreamSurvival' -count=3) \
  2>&1 | tee "$ART/06-core-race.txt"
test ${PIPESTATUS[0]} -eq 0
! grep -q 'DATA RACE' "$ART/06-core-race.txt"
```

PASS: command exits 0 and no `DATA RACE` in output.
FAIL: stop and resolve concurrency issue before next phase.

Commit checkpoint:

```bash
git add acexy/proxy.go acexy/lib/acexy/acexy.go acexy/repro_test.go
git commit -m "refactor: make client output path race-safe and deterministic"
```

---

## Step 7 - Phase 5 Implementation (`PMultiWriter` hardening)

Implement in `acexy/lib/pmw/pmw.go` and `acexy/lib/pmw/pmw_test.go`:
- No unsafe logging of writer internals.
- Deterministic timeout eviction behavior.
- Idempotent and lock-safe close/evict callback behavior.

Validation:

```bash
(cd "$MOD" && go test -race ./lib/pmw -count=20) \
  2>&1 | tee "$ART/07-pmw-race.txt"
test ${PIPESTATUS[0]} -eq 0
! grep -q 'DATA RACE' "$ART/07-pmw-race.txt"
```

PASS: 20x race run clean.
FAIL: stop and fix PMW synchronization.

Commit checkpoint:

```bash
git add acexy/lib/pmw/pmw.go acexy/lib/pmw/pmw_test.go
git commit -m "fix: harden PMultiWriter eviction, close semantics, and logging safety"
```

---

## Step 8 - Phase 6 Implementation (stream lifecycle leak/lock fixes)

Implement in `acexy/lib/acexy/acexy.go` and tests:
- No leaked duplicate backend sessions on concurrent same-ID fetch.
- Avoid holding global lock during slow network operations.
- Preserve stream map consistency on close errors.

Validation:

```bash
(cd "$MOD" && go test -race ./lib/acexy -run 'TestFetchStream_ConcurrentSameID_NoBackendLeak' -count=20) \
  2>&1 | tee "$ART/08-fetchstream-race.txt"
test ${PIPESTATUS[0]} -eq 0

(cd "$MOD" && go test -race . -run 'TestRapidSwitchingWithSlowBackend' -count=5) \
  2>&1 | tee "$ART/08-slow-backend-race.txt"
test ${PIPESTATUS[0]} -eq 0
```

PASS: both race commands exit 0.
FAIL: stop and fix stream lifecycle logic.

Commit checkpoint:

```bash
git add acexy/lib/acexy/acexy.go acexy/lib/acexy/fetchstream_test.go acexy/repro_test.go
git commit -m "fix: prevent concurrent fetch leaks and reduce lock scope in stream lifecycle"
```

---

## Step 9 - Phase 7 Implementation (`Copier` flush/timer correctness)

Implement in `acexy/lib/acexy/copier.go` and `acexy/lib/acexy/copier_test.go`:
- Flush buffered data on normal completion.
- Eliminate unsafe timer reset concurrency.
- Keep close operations idempotent and safe.

Validation:

```bash
(cd "$MOD" && go test -race ./lib/acexy -run 'TestCopier_' -count=20) \
  2>&1 | tee "$ART/09-copier-race.txt"
test ${PIPESTATUS[0]} -eq 0
```

PASS: copier tests pass 20x under race.
FAIL: stop and fix copier concurrency/flush logic.

Commit checkpoint:

```bash
git add acexy/lib/acexy/copier.go acexy/lib/acexy/copier_test.go
git commit -m "fix: make copier flush behavior and timer handling race-safe"
```

---

## Step 10 - Phase 8 Implementation (M3U8 timeout non-blocking)

Implement in `acexy/proxy.go` and add tests:
- Replace blocking timeout wait in handler with non-blocking timer callback.
- Ensure timer cancellation on early disconnect.
- No goroutine leaks from timeout path.

Validation:

```bash
(cd "$MOD" && go test -list 'TestM3U8' .) | tee "$ART/10-m3u8-test-list.txt"
grep -q 'TestM3U8' "$ART/10-m3u8-test-list.txt"

(cd "$MOD" && go test -race . -run 'TestM3U8' -count=10) \
  2>&1 | tee "$ART/10-m3u8-race.txt"
test ${PIPESTATUS[0]} -eq 0
```

PASS: M3U8 tests exist and pass under race.
FAIL: stop and fix timeout handling and/or tests.

Commit checkpoint:

```bash
git add acexy/proxy.go acexy/repro_test.go
git commit -m "fix: make m3u8 stream timeout handling non-blocking and idempotent"
```

---

## Step 11 - Phase 9 Implementation (cleanup + formatting)

Implement cleanup:
- Remove dead/unused elements.
- Simplify redundant routing logic carefully.
- Keep behavior stable and test-backed.

Validation:

```bash
(cd "$MOD" && gofmt -w .)
(cd "$MOD" && test -z "$(gofmt -l .)")
(cd "$MOD" && go vet ./...) 2>&1 | tee "$ART/11-go-vet.txt"
test ${PIPESTATUS[0]} -eq 0
(cd "$MOD" && go test ./...) 2>&1 | tee "$ART/11-go-test.txt"
test ${PIPESTATUS[0]} -eq 0
```

PASS: no gofmt drift, vet clean, tests green.
FAIL: stop and fix before continuing.

Commit checkpoint:

```bash
git add acexy
git commit -m "chore: remove redundancies and normalize formatting"
```

---

## Step 12 - Phase 10 Implementation (CI hardening)

Update `.github/workflows/build.yaml`:
- enforce `set -o pipefail` where `tee` is used.
- use deterministic dependency install (`go mod download`).
- add race test execution in CI.

Validation:

```bash
grep -n 'pipefail' "$ROOT/.github/workflows/build.yaml" | tee "$ART/12-ci-pipefail.txt"
test ${PIPESTATUS[0]} -eq 0

grep -n 'go mod download' "$ROOT/.github/workflows/build.yaml" | tee "$ART/12-ci-mod-download.txt"
test ${PIPESTATUS[0]} -eq 0

grep -n -- '-race' "$ROOT/.github/workflows/build.yaml" | tee "$ART/12-ci-race.txt"
test ${PIPESTATUS[0]} -eq 0
```

PASS: all 3 grep checks find expected lines.
FAIL: stop and fix workflow file.

Commit checkpoint:

```bash
git add .github/workflows/build.yaml
git commit -m "ci: prevent masked failures and add race coverage"
```

---

## Step 13 - Phase 11 Implementation (docs alignment)

Update `README.md`:
- ensure config defaults match code (`ACEXY_NO_RESPONSE_TIMEOUT`, others).
- document strict `/ace/status` behavior.
- document invalid env fallback behavior.

Validation:

```bash
grep -n 'ACEXY_NO_RESPONSE_TIMEOUT' "$ROOT/README.md" | tee "$ART/13-readme-timeout-var.txt"
test ${PIPESTATUS[0]} -eq 0

grep -n '10s' "$ROOT/README.md" | tee "$ART/13-readme-10s.txt"
test ${PIPESTATUS[0]} -eq 0

grep -n '/ace/status' "$ROOT/README.md" | tee "$ART/13-readme-status.txt"
test ${PIPESTATUS[0]} -eq 0
```

PASS: expected updated references exist.
FAIL: stop and align docs with actual behavior.

Commit checkpoint:

```bash
git add README.md
git commit -m "docs: align configuration defaults and status endpoint contract"
```

---

## Step 14 - Final Release Gate (mandatory)

```bash
(cd "$MOD" && go test ./...) 2>&1 | tee "$ART/14-final-go-test.txt"
test ${PIPESTATUS[0]} -eq 0

(cd "$MOD" && go test -race ./...) 2>&1 | tee "$ART/14-final-go-test-race.txt"
test ${PIPESTATUS[0]} -eq 0
! grep -q 'DATA RACE' "$ART/14-final-go-test-race.txt"

(cd "$MOD" && go vet ./...) 2>&1 | tee "$ART/14-final-go-vet.txt"
test ${PIPESTATUS[0]} -eq 0

(cd "$MOD" && test -z "$(gofmt -l .)")
```

PASS: all four gates pass.

Manual runtime verification:

```bash
(cd "$MOD" && ACEXY_BUFFER_SIZE=notasize go run . -help >"$ART/14-final-invalid-buffer.txt" 2>&1)
test $? -eq 0
! grep -q 'panic:' "$ART/14-final-invalid-buffer.txt"

PORT=18081
(cd "$MOD" && ACEXY_LISTEN_ADDR=":$PORT" timeout 15s go run . >"$ART/14-final-server.log" 2>&1) &
srv=$!
sleep 2
code_ok=$(curl -s -o /dev/null -w "%{http_code}" "http://127.0.0.1:$PORT/ace/status")
code_bad=$(curl -s -o /dev/null -w "%{http_code}" "http://127.0.0.1:$PORT/ace/status?id=a&infohash=b")
kill "$srv" >/dev/null 2>&1 || true
test "$code_ok" = "200"
test "$code_bad" = "400"
```

PASS: no panic on invalid env; `/ace/status` returns 200 and invalid combo returns 400.

---

## Step 15 - Final Sanity and Handoff

```bash
git status --short | tee "$ART/15-final-status.txt"
git log --oneline -n 12 | tee "$ART/15-final-log.txt"
```

PASS: only intended files changed; commit history matches phased plan.
