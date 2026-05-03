# Streaming Buffer Refactor Notes

## Current Data Flow

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

## Current Failure Mechanism

Large ACEXY_BUFFER_SIZE values create large downstream writes.
A 9MB buffer creates up to 9MB PMultiWriter writes.
PMultiWriter gives each client 5 seconds to accept the write.
Slow or burst-sensitive clients can be evicted or receive discontinuous data.
MPEG-TS video is more sensitive to gaps than audio, causing possible video freeze while audio continues.

## Target Data Flow

```text
AceStream backend body
  -> io.CopyBuffer using small TS-aligned chunks
  -> Copier.Write
  -> PMultiWriter
  -> per-client io.Pipe
  -> HTTP ResponseWriter
  -> player
```

## Target Behavior

- No giant server-side output buffer
- No large burst writes
- No slow-client global backpressure
- No silent partial-write data loss
- Clean timeout behavior
- Race-safe lifecycle
- Configurable client queue tolerance
- Clear documentation
