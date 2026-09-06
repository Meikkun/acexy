# `acexy` - An AceStream Proxy Written In Go! ⚡

[![Go Build](https://github.com/Javinator9889/acexy/actions/workflows/build.yaml/badge.svg)](https://github.com/Javinator9889/acexy/actions/workflows/build.yaml)
[![Docker Release](https://github.com/Javinator9889/acexy/actions/workflows/release.yaml/badge.svg?event=release)](https://github.com/Javinator9889/acexy/actions/workflows/release.yaml)

## Table of Contents

- [How It Works? 🛠](#how-it-works-)
- [Key Features 🔗](#key-features-)
- [Usage 📐](#usage-)
- [Optimizing 🚀](#optimizing-)
  - [Alternative 🧃](#alternative-)
- [Configuration Options ⚙](#configuration-options-)

## How It Works? 🛠

This project is a wrapper around the
[AceStream middleware HTTP API](https://docs.acestream.net/developers/start-playback/#using-middleware), allowing both
[HLS](https://en.wikipedia.org/wiki/HTTP_Live_Streaming) and
[MPEG-TS](https://en.wikipedia.org/wiki/HTTP_Live_Streaming) playback
of a stream.

I was tired of the limitations of AceStream and some of the problems that 
exist when playing a stream 📽. For example, it is only possible to play
the same channel for **1 single client**. For having multiple clients
playing **different streams**, you must manually add a unique `pid` per 
client. If there was an error during the transmission, the **whole stream
goes down**, etc.

I found quite frustrating the experience of using AceStream in a home network
with a single server and multiple clients, to try to optimize resources. This
is the topology for which I am using AceStream:

![AceStream Topology For My Network](doc/img/topology.svg)

There are some problems:

* Only **one client** can play the same stream at a time 🚫.
* Having each client to run AceStream on their own is a waste of resources
  and saturates the network 📉.
* Multiple clients can play different streams if they have a unique `pid`
  (Player ID) associated 🔓.
* The standard AceStream HTTP API is not resilient enough against errors,
  if the transmission stops it stops for every client ❌.

## Key Features 🔗

When using `acexy`, you automatically have:

* A single, centralized server running **all your AceStream streams** ⛓.
* Automatic assignation of a unique `pid` (Player ID) **per client per stream** 🪪.
* **Stream Multiplexing** 🕎: The same stream can be reproduced *at the
  same time in multiple clients*.
* **Resilient, error-proof** streaming thanks to the HTTP Middleware 🛡.
* *Blazing fast, minimal proxy* ☄ written in Go!

With this proxy, the following architecture is now possible:

![acexy Topology](doc/img/acexy.svg)

## Usage 📐

`acexy` is available and published as a Docker image. Make sure you have
the latest [Docker](https://docker.com) image installed and available.

The Acexy container will connect against an AceStream server. You need to
deploy either a Docker image, and link Acexy within the same network; Or
have a running AceStream version on your host and run Acexy in host-networked
mode.

> **INFO**: There is a `docker-compose.yml` file in the repo you can directly
> use to launch the whole block. This is **the recommended setup starting
> from `v0.2.0`**.

To run the services block, first grab the `docker-compose.yml` file, and run:

```shell
wget https://raw.githubusercontent.com/Javinator9889/acexy/refs/heads/main/docker-compose.yml
docker compose run -d
```

If you don't want to use Docker Compose, assuming you already have an
AceStream server, another way could be:

```shell
docker run --network host ghcr.io/javinator9889/acexy
```

> **NOTE**: For your convenience, a `docker-compose.yml` file is given with
> all the possible adjustable parameters. It should be ready to run, and it's
> the recommended way starting from `v0.2.0`.

By default, the proxy will work in MPEG-TS mode. For switching between them,
you must add the **`-m3u8` flag** or set **`ACEXY_M3U8=true` environment
variable**.

> **NOTE**: The HLS mode - `ACEXY_M3U8` or `-m3u8` flag - is in a non-tested
> status. Using it is discouraged and not guaranteed to work.

There are two available endpoints:

* **`/ace/getstream`** - takes the same parameters as the standard
[AceStream Middleware/HTTP API](https://docs.acestream.net/developers/api-reference/). Therefore,
for running a stream, just open the following link in your preferred application - such as VLC:

```
http://127.0.0.1:8080/ace/getstream?id=dd1e67078381739d14beca697356ab76d49d1a2
```

where `dd1e67078381739d14beca697356ab76d49d1a2` is the ID of the AceStream
channel.

* **`/ace/status`** - returns the current status of the proxy. You can query the
status of a specific stream by providing the `id` or `infohash` parameter, or
get the global status by omitting both. **Note**: providing both `id` and
`infohash` at the same time, or providing empty values, will result in a
`400 Bad Request` response.

### Environment Variables

Environment variables can be used to configure the proxy. If an environment
variable contains an invalid value (e.g. a non-numeric value for a numeric
setting), the proxy will **fall back to the default value** instead of crashing.

### Graceful Shutdown

Acexy handles `SIGTERM` and `SIGINT` for graceful shutdown. When a shutdown
signal is received:

1. Admission closes and active sessions are cancelled.
2. Playback bodies and client pipes close; backend stops run concurrently.
3. HTTP handlers and cleanup share **one** `ACEXY_SHUTDOWN_TIMEOUT` budget.
   Remaining HTTP connections and cleanup requests are cancelled at its deadline.

Engine-stop failures are not active viewers: `pending_cleanup` reports unresolved
cleanup separately. Stops have at most two three-second attempts, bounded further
by shutdown. A failed stop never restarts the engine or another healthy session.
If metadata acquisition might have reached the engine but no valid command URL
was recovered, cleanup remains unresolved and idle maintenance is refused.
Viewer cancellation allows an already-dispatched metadata request to finish
within its startup deadline so a late command URL can still be stopped.
Definite pre-dispatch failures (such as a failed connection) do not invent
backend ownership. Unrecoverable identity requires operator reconciliation;
Acexy does not guess a stop URL or declare the engine idle.

In Docker Compose, running `docker compose down` sends `SIGTERM` to the
container, triggering this graceful shutdown automatically.

### Admission and live writes

`ACEXY_MAX_CONNECTIONS` includes both registered clients and pending admissions.
`ACEXY_MAX_CONCURRENT_CHANNELS` includes metadata requests in flight. Zero still
means unlimited. Concurrent requests for the same ID share one metadata request
and one playback request; cancelling one waiter does not cancel other viewers.

`ACEXY_WRITE_TIMEOUT` is renewed before each downstream write, not a maximum
stream duration. A stalled write, cancellation or queue eviction closes only
that viewer. Live responses flush chunks and set `X-Accel-Buffering: no`.
Response-writer wrappers must support `http.ResponseController` deadlines (or
provide `Unwrap`); unsupported writers fail explicitly rather than disabling
deadlines. Queue capacity remains 64 chunks by default; chunk sizes vary with
upstream reads, with a 48,128-byte copy-buffer ceiling.
Normal upstream EOF drains queued payloads before closing client pipes and
finishes HTTP framing normally. EOF draining is bounded by the write timeout;
request cancellation, eviction and shutdown retain the immediate abort path.

Upstream malformed responses and unsuccessful playback status codes return 502;
startup timeouts return 504, before video headers are committed. Invalid client
requests retain 400/405 responses and admission limits return 503.

Library callers may keep using `FetchStream`, `StartStream`, `StopStream` and
`WaitStream`. `StartStream` now enforces `Acexy.MaxConnections`. For cancellable
startup, call `ReserveAdmission(id, params)`, defer its `Release`, then use
`FetchReservedStream(ctx, reservation)` and
`StartReservedStream(ctx, reservation, stream, writer)`. Stop the registered
writer when finished. All APIs use the same session registry. A legacy
metadata-only `FetchStream` remains pending until started or shutdown and
therefore prevents idle maintenance.

### Private maintenance control

Disabled by default. Set `ACEXY_CONTROL_SOCKET` (or `-control-socket`) to a private
Unix socket path, for example `/run/acexy-control.sock`. The socket and its
`<socket>.state` file are mode `0600`; the parent directory must be trusted.
Maintenance transitions are atomically persisted and synced before success is
returned. Invalid state fails startup; persisted state is restored before public
HTTP admission opens. Keep the state file when restarting Acexy.

The following HTTP routes exist **only on the Unix socket**, never on public TCP:

| Method / route | Effect |
|---|---|
| `GET /status` | Current status; does not change the gate |
| `POST /quiesce` | Block new distinct channels; allow existing-channel joins |
| `POST /enter` | Close all admission only when clients, reservations, pending starts and cleanup are zero |
| `POST /exit` | Explicitly reopen admission; the caller must first verify engine readiness |

Successful calls return 200 with the normal global status JSON. Busy `enter`, or
attempting to downgrade maintenance through `quiesce`, returns 409. Persistence
failure returns 500 and fails admission closed. Transition errors include current
status plus `error: "maintenance_transition_failed"`. Wrong methods return 405.
Only explicit `exit` reopens a maintenance gate; process restart does not.

Additive global and per-stream status fields are always present:

* `pending_admissions`, `pending_starts`, `pending_cleanup`: numeric counts.
* `maintenance`: `open`, `quiescing`, or `maintenance`; `maintenance_phase`
  remains an identical compatibility alias. The state file uses `maintenance_phase`.
* `admission_paused`: boolean; true also during shutdown. In quiescing mode,
  existing-channel joins are still permitted.

Global status retains `streams`, `total_clients`, and existing stream information.
Counts are snapshots, not live pointers. Unknown or unresolved cleanup is not
permission to stop an engine. This interface does not stop processes or delete
files: an external maintenance owner must confirm the engine process group is
gone before deletion, then restart it and check readiness before `exit`.
Automatic maintenance is safe only if all engine playback passes through Acexy.
Idle-aware storage cleanup is not a filesystem hard quota.

## Optimizing 🚀

The AceStream Engine running behind of the proxy has a number of ports that can
be exposed to optimize the performance. Those are, by default:

- `8621/tcp`
- `8621/udp`

> NOTE: They can be adjusted through the `EXTRA_FLAGS` variable - within Docker - by
> using the `--port` flag.

Exposing those ports should help getting a more stable streaming experience. Notice
that you will need to open up those ports on your gateway too.

For reference, this is how you should run the Docker command:

```shell
docker run -t -p 8080:8080 -p 8621:8621 ghcr.io/javinator9889/acexy
```

### Alternative 🧃

AceStream underneath attempts to use UPnP IGD to connect against a remote machine.
The problem is that this is not working because of the bridging layer added by Docker
(see: https://docs.docker.com/engine/network/drivers/bridge/).

If you are running a single instance of Acexy - and a single instance of AceStream -
it should be safe for you to run the container with *host networking*. This means:

- The container **can access** any other application bridged to your main network.
- You **don't need** to expose any ports.
- Performance **is optimized** a little bit.

> NOTE: This only works on Linux environments. See https://docs.docker.com/engine/network/drivers/host/
> for more information.

The command is quite straightforward:

```shell
docker run -t --network host ghcr.io/javinator9889/acexy
```

That should enable AceStream to use UPnP freely.

## Configuration Options ⚙

Acexy has tons of configuration options that allow you to customize the behavior. All of them have
default values that were tested for the optimal experience, but you may need to adjust them
to fit your needs.

> **PRO-TIP**: You can issue `acexy -help` to have a complete view of all the available options.

As Acexy was thought to be run inside a Docker container, all the variables and settings are
adjustable by using environment variables.


<table>
  <thead>
    <tr>
      <th>Flag</th>
      <th>Environment Variable</th>
      <th>Description</th>
      <th>Default</th>
    </tr>
  </thead>
  <tbody>
    <tr>
      <th><code>-license</code></th>
      <th>-</th>
      <th>Prints the program license and exits</th>
      <th>-</th>
    <tr>
    <tr>
      <th><code>-help</code></th>
      <th>-</th>
      <th>Prints the help message and exits</th>
      <th>-</th>
    <tr>
    <tr>
      <th><code>-addr</code></th>
      <th><code>ACEXY_LISTEN_ADDR</code></th>
      <th>Address where Acexy is listening to. Useful when running in <code>host</code> mode.</th>
      <th><code>:8080</code></th>
    <tr>
    <tr>
      <th><code>-scheme</code></th>
      <th><code>ACEXY_SCHEME</code></th>
      <th>
        The scheme of the AceStream middleware. If you have configured AceStream to work in HTTPS,
        you will have to tweak this value.
      </th>
      <th><code>http</code></th>
    <tr>
    <tr>
      <th><code>-acestream-host</code></th>
      <th><code>ACEXY_HOST</code></th>
      <th>
        Where the AceStream middleware is located. Change it if you need Acexy to connect to a
        different AceStream Engine.
      </th>
      <th><code>localhost</code></th>
    <tr>
    <tr>
      <th><code>-acestream-port</code></th>
      <th><code>ACEXY_PORT</code></th>
      <th>
        The port to connect to the AceStream middleware. Change it if you need Acexy to connect
        to a different AceStream Engine.
      </th>
      <th><code>6878</code></th>
    <tr>
    <tr>
      <th><code>-m3u8-stream-timeout</code></th>
      <th><code>ACEXY_M3U8_STREAM_TIMEOUT</code></th>
      <th>
        When running Acexy in M3U8 mode, the timeout to consider a stream is done.
      </th>
      <th><code>60s</code></th>
    <tr>
    <tr>
      <th><code>-m3u8</code></th>
      <th><code>ACEXY_M3U8</code></th>
      <th>
        Enable M3U8 mode in Acexy. <b>WARNING</b>: This mode is experimental and may not work as expected.
      </th>
      <th>Disabled</th>
    <tr>
    <tr>
      <th><code>-empty-timeout</code></th>
      <th><code>ACEXY_EMPTY_TIMEOUT</code></th>
      <th>
        Timeout to consider a stream is finished once empty information is received from
        the middleware. Useless when in M3U8 mode.
      </th>
      <th><code>1m</code></th>
    <tr>
    <tr>
      <th><code>-buffer-size</code></th>
      <th><code>ACEXY_BUFFER_SIZE</code></th>
      <th>
        <b>Deprecated</b>. Older versions used this as a server-side output aggregation buffer.
        That caused large burst writes and could destabilize live MPEG-TS playback. Acexy now
        streams using small TS-aligned chunks and relies on the player buffer for playback smoothing.
        The flag is accepted for compatibility but no longer controls streaming output buffering.
      </th>
      <th><code>4.2MiB</code> (ignored)</th>
    <tr>
    <tr>
      <th><code>-no-response-timeout</code></th>
      <th><code>ACEXY_NO_RESPONSE_TIMEOUT</code></th>
      <th>
        Time to wait for the AceStream middleware to return a response for a newly opened stream.
        This must be as low as possible unless your Internet connection is really bad
        (ie: You have very big latencies).
      </th>
      <th><code>10s</code></th>
    <tr>
    <tr>
      <th><code>-write-timeout</code></th>
      <th><code>ACEXY_WRITE_TIMEOUT</code></th>
      <th>
        Controls how long a client writer may block before being evicted.
        Lower values remove slow clients faster. Higher values tolerate slow clients longer.
      </th>
      <th><code>5s</code></th>
    <tr>
    <tr>
      <th><code>-client-queue-size</code></th>
      <th><code>ACEXY_CLIENT_QUEUE_SIZE</code></th>
      <th>
        Per-client queue size for the stream broadcaster.
        Larger values tolerate slower clients but use more memory.
        Entries hold variable-sized chunks up to 48,128 bytes.
        A full 64-entry queue can retain about 3MB of payload; immutable chunks
        shared by multiple queues are not copied per client.
      </th>
      <th><code>64</code></th>
    <tr>
    <tr>
      <th><code>-max-connections</code></th>
      <th><code>ACEXY_MAX_CONNECTIONS</code></th>
      <th>
        Maximum registered clients plus pending admissions. 0 means unlimited.
      </th>
      <th><code>0</code> (unlimited)</th>
    <tr>
    <tr>
      <th><code>-max-concurrent-channels</code></th>
      <th><code>ACEXY_MAX_CONCURRENT_CHANNELS</code></th>
      <th>
        Maximum distinct sessions, including metadata starts in flight. 0 means unlimited.
      </th>
      <th><code>0</code> (unlimited)</th>
    <tr>
    <tr>
      <th><code>-shutdown-timeout</code></th>
      <th><code>ACEXY_SHUTDOWN_TIMEOUT</code></th>
      <th>
        Time to wait for graceful shutdown after receiving SIGTERM or SIGINT.
        Sessions are cancelled and backend cleanup shares this one timeout.
      </th>
      <th><code>10s</code></th>
    <tr>
    <tr>
      <th><code>-control-socket</code></th>
      <th><code>ACEXY_CONTROL_SOCKET</code></th>
      <th>Private Unix maintenance socket. Its durable gate state is kept in the adjacent .state file.</th>
      <th>Empty (disabled)</th>
    <tr>
  </tbody>
</table>

> **Streaming Behavior**: Acexy does not use a large server-side playback buffer.
> Large output buffers caused burst writes and could destabilize live MPEG-TS playback.
> Acexy forwards variable-sized chunks using a TS-aligned 48,128-byte copy buffer and isolates slow clients
> using per-client bounded queues. Player buffering should be configured in the player,
> not in Acexy.
>
> `ACEXY_BUFFER_SIZE` is deprecated and no longer controls streaming output buffering.

> **NOTE**: The list of options is extensive but could be outdated. Always refer to the
> Acexy binary `-help` output when in doubt.
