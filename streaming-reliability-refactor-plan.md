# Acexy Streaming Reliability Refactor Plan

This plan is written as a strict step-by-step guide. Do not skip steps. Do not combine phases unless explicitly told. Each phase should end with tests passing before continuing.

## Goal

Make Acexy as reliable, fast, stable, and clean as possible for live MPEG-TS proxying by removing harmful server-side output aggregation, preventing data loss, improving slow-client handling, and preparing the architecture for a proper per-client broadcaster.

## Core Decision

The current `bufio.Writer` inside `Copier` must be removed from the streaming hot path.

It is not a real jitter buffer. With `ACEXY_BUFFER_SIZE=9MB`, it accumulates 9MB and flushes that as a giant write to clients. This can cause client write timeouts, stream gaps, video decoder stalls, and the observed "video stops but audio continues" symptom.

The new model should be:

```text
AceStream backend body
  -> io.CopyBuffer using small TS-aligned chunks
  -> Copier.Write
  -> PMultiWriter
  -> per-client io.Pipe
  -> HTTP ResponseWriter
  -> player
```

---

## Phase 0: Safety Preparation

### Objective

Create a safe working baseline before making any new streaming changes.

### Steps

1. Ensure you are on the reliability branch:

```bash
git branch --show-current
```

Expected:

```text
fix/reliability-hardening
```

2. Ensure the worktree is clean:

```bash
git status --short
```

Expected: no output.

3. Run the existing verification suite:

```bash
cd acexy
go test ./...
go test -race ./...
go vet ./...
test -z "$(gofmt -l .)"
```

4. If anything fails, stop immediately and fix the baseline before continuing.

5. Create a new branch from the current reliability branch:

```bash
git checkout -b fix/streaming-buffer-refactor
```

### Completion Criteria

- New branch exists.
- Existing tests pass before any new code changes.
- Worktree starts clean.

---

## Phase 1: Document The Current Data Flow

### Objective

Before changing code, write down the actual current byte path and failure mode. This prevents future confusion.

### Files

Create or update a short internal note, for example:

```text
.review-artifacts/streaming-buffer-refactor-notes.md
```

### Required Content

Document this exact current flow:

```text
AceStream backend HTTP response body
  -> io.Copy
  -> Copier.Write
  -> bufio.Writer using ACEXY_BUFFER_SIZE
  -> PMultiWriter.Write
  -> io.PipeWriter per client
  -> io.Copy to http.ResponseWriter
  -> player
```

Document this exact failure mechanism:

```text
Large ACEXY_BUFFER_SIZE values create large downstream writes.
A 9MB buffer creates up to 9MB PMultiWriter writes.
PMultiWriter gives each client 5 seconds to accept the write.
Slow or burst-sensitive clients can be evicted or receive discontinuous data.
MPEG-TS video is more sensitive to gaps than audio, causing possible video freeze while audio continues.
```

### Completion Criteria

- The current behavior and the target behavior are written down.
- No code has changed yet.

---

## Phase 2: Add Regression Tests For Giant Burst Prevention

### Objective

Add tests before changing implementation. These tests must fail or expose the current behavior before the refactor, then pass after.

### Target Files

- `acexy/lib/acexy/copier_test.go`
- Possibly a new test helper file if needed

### Test 1: Copier Does Not Emit Buffer-Sized Bursts

Add a test named:

```go
func TestCopier_DoesNotEmitBufferSizedBursts(t *testing.T)
```

### Test Design

Create a custom destination writer that records the size of every `Write(p)` call.

Example helper:

```go
type recordingWriter struct {
    mu     sync.Mutex
    writes []int
}

func (w *recordingWriter) Write(p []byte) (int, error) {
    w.mu.Lock()
    defer w.mu.Unlock()
    w.writes = append(w.writes, len(p))
    return len(p), nil
}
```

Use a source larger than the old buffer size.

Example:

```go
sourceSize := 2 * 1024 * 1024
bufferSize := 1024 * 1024
```

Before the refactor, `bufio.Writer` may emit very large writes around `bufferSize`.

After the refactor, maximum write size should be the configured copy chunk size, for example `188 * 256 = 48128`.

Test assertion:

```go
maxWrite := max(recorded writes)
if maxWrite > expectedMaxChunkSize {
    t.Fatalf("write burst too large: got %d, want <= %d", maxWrite, expectedMaxChunkSize)
}
```

### Test 2: Copier Still Copies All Bytes

Add:

```go
func TestCopier_DirectCopyCopiesAllBytes(t *testing.T)
```

Use:

- `bytes.NewReader(input)`
- `bytes.Buffer` as destination
- `Copier.Copy()`

Assert:

```go
bytes.Equal(input, output.Bytes())
```

### Test 3: Empty Timeout Still Closes Source And Destination

Keep or extend existing timeout tests.

Verify:

- Source gets closed after no data is received for `EmptyTimeout`
- Destination gets closed if it implements `io.Closer`
- No data is lost before close

### Completion Criteria

- Tests exist.
- Tests are meaningful.
- It is acceptable if the new burst test fails before implementation.

---

## Phase 3: Define Streaming Chunk Size

### Objective

Use a fixed, small, MPEG-TS-aligned copy buffer size.

### Why

MPEG-TS packets are 188 bytes. Writes do not strictly need to align to 188 bytes because TCP is a byte stream, but alignment makes debugging easier and avoids unnecessary packet fragmentation at application boundaries.

### Decision

Add a constant in `acexy/lib/acexy/copier.go`:

```go
const MPEGTS_PACKET_SIZE = 188
const DEFAULT_COPY_PACKETS = 256
const DEFAULT_COPY_BUFFER_SIZE = MPEGTS_PACKET_SIZE * DEFAULT_COPY_PACKETS
```

This gives:

```text
188 * 256 = 48128 bytes
```

Why `48KB`:

- Small enough to avoid giant bursts
- Large enough to avoid excessive syscall/write overhead
- TS-aligned
- Much safer than 4MB or 9MB
- Requires only ~77 kbps to drain within 5 seconds

Alternative acceptable value:

```go
188 * 512 = 96256 bytes
```

Do not exceed ~128KB for this phase.

### Completion Criteria

- A single internal copy buffer constant exists.
- The value is TS-aligned.
- Tests refer to this constant instead of duplicating magic numbers.

---

## Phase 4: Remove `bufio.Writer` From Copier

### Objective

Make `Copier` forward bytes directly instead of aggregating output into large flushes.

### Target File

```text
acexy/lib/acexy/copier.go
```

### Current Problematic Fields

Remove:

```go
bufferedWriter *bufio.Writer
BufferSize     int
```

Or keep `BufferSize` temporarily only for compatibility, but do not use it inside the streaming hot path.

### Recommended Struct

```go
type Copier struct {
    Destination  io.Writer
    Source       io.Reader
    EmptyTimeout time.Duration

    mu     sync.Mutex
    timer  *time.Timer
    closed bool
}
```

### Copy Implementation

Replace:

```go
_, err := io.Copy(c, c.Source)
```

with:

```go
buf := make([]byte, DEFAULT_COPY_BUFFER_SIZE)
_, err := io.CopyBuffer(c, c.Source, buf)
```

### Write Implementation

Replace:

```go
return c.bufferedWriter.Write(p)
```

with:

```go
return c.Destination.Write(p)
```

### Timer Behavior

Keep the existing purpose:

- Every successful `Write` means backend is alive
- Reset the empty timeout timer
- If timer fires, close source and destination

### Correct Timer Pattern

Avoid resetting the timer from both the timer goroutine loop and `Write()` in confusing ways.

Use this simpler model:

1. `Copy()` creates the timer.
2. `Write()` resets the timer after data arrives.
3. Timer goroutine waits for timer firing or copy completion.
4. On timeout, it closes source and destination.

### Important Locking Rule

Never hold `c.mu` while calling:

```go
c.Destination.Write(p)
```

Why:

- Destination may block
- Destination may call back into other code
- Holding the mutex while writing can deadlock timer cleanup

### Correct Write Flow

Pseudo-code:

```go
func (c *Copier) Write(p []byte) (int, error) {
    if len(p) == 0 {
        return 0, nil
    }

    c.mu.Lock()
    if c.closed || c.timer == nil {
        c.mu.Unlock()
        return 0, io.ErrClosedPipe
    }
    resetTimerLocked()
    dst := c.Destination
    c.mu.Unlock()

    return dst.Write(p)
}
```

### Completion Criteria

- `Copier` no longer imports `bufio`.
- `ACEXY_BUFFER_SIZE` no longer controls output write size.
- `Copier` writes directly to destination.
- Timer behavior still works.
- No mutex is held while writing to destination.

---

## Phase 5: Handle `ACEXY_BUFFER_SIZE` Compatibility

### Objective

Avoid breaking users immediately while removing harmful behavior.

### Current Behavior

`ACEXY_BUFFER_SIZE` controls `Copier.BufferSize`.

### New Behavior

For this PR:

- Keep accepting `ACEXY_BUFFER_SIZE`
- Do not use it for output aggregation
- Mark it deprecated
- Optionally log a warning if it is set

### Recommended Code Behavior

In `proxy.go`, leave parsing intact for now to avoid CLI/env breakage.

When constructing `Acexy`, either:

1. Keep assigning `BufferSize` but stop using it inside `Copier`, or
2. Remove `BufferSize` from `Acexy` only if every reference is cleaned up and docs/tests are updated

Safer junior-friendly approach:

- Keep the field
- Stop using it
- Add a comment:

```go
// BufferSize is deprecated. Streaming now uses a fixed TS-aligned copy buffer
// to avoid large downstream write bursts.
BufferSize int
```

### Docs Update

Update README:

- Explain `ACEXY_BUFFER_SIZE` is deprecated
- Explain it no longer controls streaming output buffering
- Explain why large server-side output buffers were removed

Suggested wording:

```text
ACEXY_BUFFER_SIZE is deprecated. Older versions used it as a server-side output aggregation buffer. That caused large burst writes and could destabilize live MPEG-TS playback. Acexy now streams using small TS-aligned chunks and relies on the player buffer for playback smoothing.
```

### Completion Criteria

- Existing env config does not crash.
- Existing command-line flag still exists unless intentionally removed.
- README clearly explains deprecation.
- Users with `ACEXY_BUFFER_SIZE=9MB` do not get giant writes anymore.

---

## Phase 6: Fix PMultiWriter Partial Write Handling

### Objective

Prevent silent data loss if a writer reports a partial write.

### Target File

```text
acexy/lib/pmw/pmw.go
```

### Current Problem

Current code ignores `n`:

```go
_, err := w.Write(p)
done <- err
```

If `n < len(p)` and `err == nil`, data loss is silently treated as success.

### Required Change

Replace with:

```go
n, err := w.Write(p)
if err == nil && n != len(p) {
    err = io.ErrShortWrite
}
done <- err
```

### Add Test

Target file:

```text
acexy/lib/pmw/pmw_test.go
```

Add:

```go
func TestPMultiWriter_PartialWriteReturnsError(t *testing.T)
```

Create a writer that returns:

```go
return len(p) / 2, nil
```

Expected:

- PMultiWriter treats it as failure
- If all writers short-write, PMultiWriter returns error
- If one writer succeeds and one short-writes, PMultiWriter returns success for the whole stream but records/logs the failed writer behavior according to existing policy

### Completion Criteria

- Partial writes are never silently accepted.
- Tests cover all-short-write and mixed-success cases.

---

## Phase 7: Make PMultiWriter Timeout Configurable

### Objective

Avoid hard-coded 5-second timeout and make slow-client behavior transparent.

### Current Code

```go
writers := pmw.New(context.Background(), 5*time.Second)
```

### Recommended Change

Add a new Acexy config field:

```go
WriteTimeout time.Duration
```

Default:

```go
5 * time.Second
```

Add CLI/env:

```text
-write-timeout
ACEXY_WRITE_TIMEOUT
```

### Why

After removing giant bursts, 5 seconds is probably safe. But users with very slow networks may need tuning.

### Docs

Document:

```text
ACEXY_WRITE_TIMEOUT controls how long a client writer may block before being evicted. Lower values remove slow clients faster. Higher values tolerate slow clients longer.
```

### Test

Add env parsing test:

```go
func TestLookupEnvOrDuration_WriteTimeoutInvalidFallsBack(t *testing.T)
```

### Completion Criteria

- No hard-coded `5*time.Second` in stream setup.
- Default behavior remains 5 seconds.
- Invalid env value falls back safely.

---

## Phase 8: Add Runtime Diagnostics For Streaming

### Objective

Make future issues debuggable without race-prone logging.

### Add Safe Logs

In `Copier.Copy()`:

```go
slog.Debug("copy started", "copy_buffer_size", DEFAULT_COPY_BUFFER_SIZE)
```

Do not log mutable structs.

In PMultiWriter timeout:

```go
slog.Warn(
    "writer timed out, evicting",
    "writer_type", fmt.Sprintf("%T", w),
    "chunk_size", len(p),
    "timeout", pmw.writeTimeout,
)
```

In partial write:

```go
slog.Warn(
    "writer short write",
    "writer_type", fmt.Sprintf("%T", w),
    "written", n,
    "expected", len(p),
)
```

### Completion Criteria

- Logs include chunk size on timeout.
- No log passes full mutable structs like `pmw`, `writer`, `stream`, or `copier`.
- Race tests remain clean.

---

## Phase 9: Update Tests For Deprecated Buffer Behavior

### Objective

Ensure tests prove `ACEXY_BUFFER_SIZE=9MB` no longer causes 9MB downstream writes.

### Add Integration-Like Test

Target:

```text
acexy/lib/acexy/copier_test.go
```

Test name:

```go
func TestCopier_LargeConfiguredBufferDoesNotCreateLargeWrites(t *testing.T)
```

Setup:

- Create `Copier` with `BufferSize: 9 * 1024 * 1024` if field still exists
- Source: at least 10MB
- Destination: recording writer
- EmptyTimeout: generous, e.g. `5 * time.Second`

Assert:

- Total output bytes equal input bytes
- No individual destination write exceeds `DEFAULT_COPY_BUFFER_SIZE`

### Completion Criteria

- This test would fail on the old `bufio.Writer` behavior.
- It passes after refactor.
- It directly protects the user's real config case.

---

## Phase 10: Full Verification After Short-Term Refactor

### Objective

Prove the immediate fix is safe.

### Commands

Run from module directory:

```bash
go test ./...
go test -race ./...
go vet ./...
test -z "$(gofmt -l .)"
```

Run stress tests:

```bash
go test -race . -run 'TestRapidSwitchingWithSlowBackend|TestThreeClientStress|TestMultiClientStreamSurvival' -count=10 -timeout 600s
```

Run copier-specific tests:

```bash
go test -race ./lib/acexy -run 'TestCopier_' -count=50
```

Run PMultiWriter tests:

```bash
go test -race ./lib/pmw -count=50
```

### Completion Criteria

- All commands pass.
- No `DATA RACE`.
- No hanging tests.
- No goroutine leak symptoms from tests.

---

## Phase 11: Commit Short-Term Fix

### Objective

Create a clean commit for the immediate streaming stabilization fix.

### Commit Message

```bash
git add acexy README.md
git commit -m "fix: stream directly with bounded TS-aligned chunks"
```

### Commit Should Include

- Copier no longer uses `bufio.Writer`
- Fixed copy buffer size
- PMultiWriter short-write handling
- Optional configurable write timeout
- Tests
- README updates

### Completion Criteria

- Commit exists.
- Commit message accurately describes the change.

---

# Medium-Term Refactor: Replace PMultiWriter With Per-Client Queues

The previous phases fix the immediate bug. The following phases implement the best long-term architecture.

---

## Phase 12: Design The Stream Broadcaster

### Objective

Replace synchronous fanout with per-client bounded queues so one slow client cannot affect others.

### New Concept

```text
Broadcaster
  - receives chunks from backend
  - stores/distributes chunks to clients
  - each client has its own bounded queue
  - each client has its own writer goroutine
```

### New Types

Create new package or file:

```text
acexy/lib/acexy/broadcaster.go
```

Define:

```go
type BroadcastChunk struct {
    Data []byte
}

type ClientWriter struct {
    id        string
    out       io.Writer
    queue     chan BroadcastChunk
    done      chan struct{}
    closed    chan struct{}
    closeOnce sync.Once
}

type Broadcaster struct {
    mu        sync.RWMutex
    clients   map[io.Writer]*ClientWriter
    closed    bool
    queueSize int
    onEvict   func(io.Writer)
}
```

### Key Rule

Backend writes must never block indefinitely on client I/O.

### Completion Criteria

- Broadcaster design is documented before coding.
- Queue size and eviction behavior are decided.

---

## Phase 13: Implement Per-Client Queue Fanout

### Objective

Make backend writes enqueue data to clients instead of directly writing to client pipes.

### Write Behavior

When backend calls:

```go
Broadcaster.Write(p)
```

The broadcaster should:

1. Copy `p` once per client or use immutable chunk ownership safely
2. Try to enqueue to each client queue
3. If a client queue is full, evict that client
4. Return success if at least one client accepted the chunk
5. Return `len(p), nil` if there are no clients, preserving current behavior

### Important Memory Rule

Never reuse `p` without copying if it may be modified by caller after `Write` returns.

Use:

```go
chunkData := append([]byte(nil), p...)
```

Then send the same immutable `chunkData` to all clients.

### Client Writer Goroutine

Each client has:

```go
for chunk := range queue {
    _, err := out.Write(chunk.Data)
    if err != nil {
        evict client
        return
    }
}
```

### Completion Criteria

- Slow client queue fills independently.
- Slow client eviction does not block backend.
- Healthy clients continue receiving data.

---

## Phase 14: Replace PMultiWriter Usage

### Objective

Switch stream fanout from PMultiWriter to Broadcaster.

### Target File

```text
acexy/lib/acexy/acexy.go
```

Replace:

```go
writers *pmw.PMultiWriter
```

with:

```go
broadcaster *Broadcaster
```

Update:

```go
ongoingStream.writers.Add(out)
ongoingStream.writers.Remove(out)
ongoingStream.writers.Close()
```

to broadcaster equivalents:

```go
ongoingStream.broadcaster.Add(out)
ongoingStream.broadcaster.Remove(out)
ongoingStream.broadcaster.Close()
```

### Completion Criteria

- PMultiWriter no longer used in stream hot path.
- PMultiWriter package can remain for compatibility/tests, or be removed in a later cleanup.
- Existing stream lifecycle behavior remains unchanged.

---

## Phase 15: Define Queue Size Policy

### Objective

Make queue behavior predictable.

### Recommended Default

Use memory-based queue size.

Example:

```go
ClientQueueChunks = 64
ChunkSize = 48128 bytes
Max per-client queue = ~3MB
```

This gives each client a small lag cushion without allowing unbounded memory growth.

### Add Config

```text
ACEXY_CLIENT_QUEUE_SIZE
-client-queue-size
```

Default:

```text
64
```

### Eviction Rule

If a client queue is full when a new chunk arrives:

```text
Evict the client immediately.
```

Reason:

- This is a live stream
- Falling behind means old chunks become increasingly useless
- Better to disconnect/reconnect than lag indefinitely

### Completion Criteria

- Queue size is configurable.
- Default memory cost is documented.
- Full queue eviction has tests.

---

## Phase 16: Add Broadcaster Tests

### Target File

```text
acexy/lib/acexy/broadcaster_test.go
```

### Required Tests

1. `TestBroadcaster_WritesToAllClients`

Expected:

- Two clients receive identical data.

2. `TestBroadcaster_SlowClientEvicted`

Use a writer that blocks or a queue size of 1.

Expected:

- Slow client is evicted
- Healthy client keeps receiving data

3. `TestBroadcaster_NoClientsDoesNotError`

Expected:

- `Write(p)` returns `len(p), nil`

4. `TestBroadcaster_CloseIsIdempotent`

Expected:

- Calling `Close()` multiple times does not panic
- Client queues close exactly once

5. `TestBroadcaster_RemoveClient`

Expected:

- Removed client stops receiving data
- Other clients continue

6. `TestBroadcaster_Race`

Use:

```bash
go test -race ./lib/acexy -run TestBroadcaster -count=100
```

### Completion Criteria

- Broadcaster has strong unit coverage.
- Race detector is clean.

---

## Phase 17: Add End-To-End Slow Client Tests

### Objective

Prove one slow client cannot freeze video delivery for healthy clients.

### Target File

```text
acexy/repro_test.go
```

### Test Name

```go
func TestSlowClientDoesNotBlockHealthyClient(t *testing.T)
```

### Test Setup

- Start mock backend streaming continuously
- Start proxy
- Client A reads continuously
- Client B connects but does not read, or reads very slowly
- Verify Client A continues receiving bytes for several seconds
- Verify Client B eventually disconnects or is evicted

### Assertions

- Client A receives increasing byte count
- Client A does not stall for more than acceptable threshold
- Stream remains active while Client A is alive
- No data races

### Completion Criteria

- Test fails or is flaky on synchronous fanout
- Test passes with broadcaster architecture

---

## Phase 18: Add Optional MPEG-TS Packet Boundary Handling

### Objective

Improve observability and chunk quality without overengineering.

### Important

Do not parse full MPEG-TS payloads. Only preserve 188-byte chunk boundaries.

### Rule

The copy buffer size already ensures reads are TS-aligned in size, but `io.Reader` does not guarantee read length alignment.

Therefore, if strict boundary alignment is desired, add a small packet aligner:

- Maintain `remainder []byte`
- Append incoming bytes
- Only write full `188` byte multiples
- Keep leftover bytes for next write
- Flush leftover only on EOF

### Recommendation

Do not implement this in the first refactor unless tests show packet boundary issues.

Why:

- MPEG-TS over TCP is a byte stream
- Decoders should handle arbitrary chunk boundaries
- Data loss, not chunk boundary, is the real issue

### Completion Criteria

- Decision is documented.
- Do not add TS parser complexity prematurely.

---

## Phase 19: Remove Or Deprecate PMultiWriter From Streaming

### Objective

Clean up after broadcaster is stable.

### Options

1. Keep `pmw` package if it is generally useful.
2. Remove it from streaming path.
3. Update comments to state it is not used for live client fanout.

### If Removing

Only remove if:

```bash
grep -R "pmw." acexy
```

shows no required usage.

### Completion Criteria

- No dead code in streaming path.
- Tests still pass.

---

## Phase 20: Final Documentation Update

### README Must Explain

1. Acexy streams directly using bounded chunks.
2. `ACEXY_BUFFER_SIZE` is deprecated or removed.
3. Slow clients are isolated.
4. Client queue size controls per-client lag tolerance.
5. Player buffering should be configured in the player, not Acexy.
6. Recommended values for unstable networks.

### Suggested Docs

```text
Acexy does not use a large server-side playback buffer. Large output buffers caused burst writes and could destabilize live MPEG-TS playback. Acexy forwards stream data in small bounded chunks and isolates slow clients using per-client queues.
```

### Completion Criteria

- README matches actual behavior.
- No stale references to old buffer behavior.

---

## Phase 21: Final Verification

### Run All Tests

```bash
go test ./...
go test -race ./...
go vet ./...
test -z "$(gofmt -l .)"
```

### Stress Tests

```bash
go test -race . -run 'TestRapidSwitchingWithSlowBackend|TestThreeClientStress|TestMultiClientStreamSurvival|TestSlowClientDoesNotBlockHealthyClient' -count=20 -timeout 900s
```

### Runtime Manual Test

Run with your real problematic config:

```bash
ACEXY_BUFFER_SIZE=9MB go run .
```

Test playback with:

- One client
- Two clients on same stream
- One intentionally slow/disconnected client
- Channel switching
- Long playback, at least 30 minutes

### Logs To Watch

Look for:

```text
writer timed out
client evicted
short write
DATA RACE
panic
```

### Completion Criteria

- Automated tests pass.
- Manual playback no longer shows video freeze/audio continue.
- Slow clients do not affect healthy clients.
- No race detector failures.

---

## Phase 22: Commit Strategy

Use separate commits:

### Commit 1

```bash
git commit -m "test: cover streaming burst and short-write behavior"
```

Contains:

- Copier burst tests
- PMultiWriter partial write tests

### Commit 2

```bash
git commit -m "fix: stream with bounded TS-aligned chunks"
```

Contains:

- Remove `bufio.Writer`
- Use `io.CopyBuffer`
- Keep empty-timeout behavior
- Deprecate buffer behavior

### Commit 3

```bash
git commit -m "fix: treat partial client writes as stream errors"
```

Contains:

- PMultiWriter short-write handling

### Commit 4

```bash
git commit -m "feat: isolate clients with bounded stream queues"
```

Contains:

- Broadcaster
- Per-client queue implementation
- Stream lifecycle integration

### Commit 5

```bash
git commit -m "docs: document streaming buffer and slow-client behavior"
```

Contains:

- README updates
- Config docs

---

## Final Recommended End State

The best final architecture is:

```text
AceStream backend
  -> Copier using io.CopyBuffer with TS-aligned chunks
  -> Broadcaster
  -> bounded per-client queues
  -> per-client writer goroutines
  -> HTTP ResponseWriter
```

And the best behavior is:

- No giant server-side output buffer
- No large burst writes
- No slow-client global backpressure
- No silent partial-write data loss
- Clean timeout behavior
- Race-safe lifecycle
- Configurable client queue tolerance
- Clear documentation

This is the most reliable, fast, stable, and clean direction for Acexy.
