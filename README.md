# Flock

Agent-first file transfer, forked from [croc](https://github.com/schollz/croc).
Flock carries file bytes over **native QUIC/UDP** and gives agents a versioned
JSON Lines interface with generated invitations, progress, deadlines, and
verified receive results.

**Experimental first implementation.** Directly reachable IPv4 peers work on a
LAN or an overlay such as Tailscale. Internet NAT discovery, UDP relay fallback,
and geographic WAN qualification are future work. There is no demonstrated
speed advantage over croc yet; UDP alone does not guarantee one.

## Build

Requires Go 1.27 or newer (inherited from the pinned croc base).

```sh
git clone https://github.com/mhadifilms/flock.git
cd flock
go build -o flock ./cmd/flock
./flock schema
```

Until the initial feature PR is merged, check out `feat/agent-first-udp` before
building. `go build .` still builds the inherited croc command. There are no
Flock release binaries or packages yet.

## Two agents, two processes

Start the sender with an IPv4 address the recipient can reach. It automatically
hosts a temporary TCP control relay and opens a UDP listener:

```sh
./flock send --advertise 100.64.0.10 ./folder
```

Read stdout as JSON Lines. The `invitation` event contains an `invite` object.
Pass that object, or the whole event, to the recipient over your existing trusted
agent channel. Keep the sender process running.

On the other computer:

```sh
./flock receive --out ./received --invite-file invitation.json
# Or pass the same JSON object on stdin:
./flock receive --out ./received < invitation.json
```

There is no human code to choose or type, no clipboard dependency, and no
confirmation prompt. The invitation is a generated 256-bit bearer capability
with an expiry, control address, relative paths, sizes, and SHA-256 hashes.
It grants access to this transfer: treat it as a secret when handing it off.

For example, an agent reading a sender subprocess can extract the invitation:

```python
for line in sender.stdout:
    event = json.loads(line)
    if event["event"] == "invitation":
        handoff = json.dumps(event["invite"])
        # Deliver handoff to the recipient agent; continue draining sender.stdout.
```

Do not pipe the entire sender event stream into the recipient. Hand over the
single invitation object and continue consuming each process's events.

## Agent contract

Stdout always contains JSON Lines, including argument errors and help. Stderr
is reserved for diagnostics. `flock schema` describes commands, event names,
and exit codes. All events have `version: 1` and `event` fields.

| Event | Meaning |
| --- | --- |
| `preparing` | Inventory and hash source files |
| `invitation` | Generated transfer capability and expected file manifest |
| `connecting` | Recipient is pairing |
| `transport` | QUIC listener/connection state |
| `progress` | Cumulative application bytes transferred |
| `completed` | Role-specific result, manifest, and byte count |
| `error` | Stable error code and diagnostic message |
| `ready` | Standalone control relay is listening |

The recipient emits `completed` with `sha256_verified: true` **only after every
received file has passed full SHA-256 and size verification**, and expected empty
directories exist. A sender's `completed` reports transport completion with
`sha256_verified: false`; consume the recipient's result for verified delivery.
The sender does not yet receive a separate post-verification receipt.

Exit status is `0` for success, `1` for failure, `124` for timeout, and `130` for
cancellation. `--timeout` defaults to `30m`, includes invitation input and
verification, and accepts positive durations up to `24h`. SIGINT/SIGTERM cancel
the process. A broken stdout pipe cancels the transfer.

Receive destinations must be new directories. This first agent interface does
not expose resume or overwrite. Failed transfers can leave partial files in the
new destination. Symlinks and special files are rejected. Invitations are
limited to 4 MiB and 10,000 file/directory entries.

## Transport

Flock reuses croc's PAKE, encrypted control messages, chunk framing, file handling,
and transport negotiation. It adds a QUIC adapter using
[quic-go](https://github.com/quic-go/quic-go), with TLS 1.3 mutual authentication
bound to the PAKE session. The authenticated control channel exchanges UDP
endpoints; repeated outbound probes help establish bidirectional UDP traffic.
The default is eight bidirectional streams on one QUIC connection; set
`--streams 1` through `--streams 16` to experiment.

TCP carries pairing/control only. File data requires QUIC and **does not silently
fall back to TCP**. The peer must advertise Flock support. This is not a croc
wire-compatible file-data mode, although the inherited croc CLI remains available.

The advertised IPv4 address must be reachable at the actual UDP listener port.
The default embedded control listener uses a random TCP port; both that TCP port
and the UDP port must be reachable. To pin them for firewall rules:

```sh
./flock send --advertise 100.64.0.10 \
  --control-listen 0.0.0.0:9009 --udp-listen 0.0.0.0:9010 ./folder
```

An existing croc control relay can be selected with `--relay HOST:PORT`. Flock
also provides a control-only relay:

```sh
./flock relay --listen 0.0.0.0:9009
./flock send --relay relay.example:9009 --advertise 100.64.0.10 ./folder
```

If configured, set the same `FLOCK_RELAY_PASSWORD` on the relay and both peers.
The relay carries encrypted control messages; no file-data relay is started by
`flock relay`. A control relay does not make an unreachable UDP endpoint reachable.
Public NAT traversal and QUIC relaying are not implemented in this first slice.

## Validation and performance

```sh
go test ./...
go test -race ./internal/flockcli ./src/codephrase ./src/croc \
  -run 'Flock|Agent|Invitation|ReceiveDeadline|TailcatMultiStreamEndToEnd'
```

Integration tests launch independent sender, receiver, and control relay
processes with real TCP control and UDP data sockets. They cover one/eight
streams, an embedded control relay, nested/empty entries, JSON-only errors,
changed manifests, bad final checksums, expired/unsafe invitations, and blocked
UDP without TCP data fallback. Session-authentication tests reject altered
endpoint offers and wrong TLS session keys.

The first native macOS-to-macOS LAN check transferred an incompressible 64 MiB
file plus nested/empty entries in **2.598 seconds end to end (24.64 MiB/s)**,
including startup and SHA-256 work. Both processes exited successfully; a separate
remote `shasum -a 256` matched the source. This is a single functional measurement,
not a croc comparison or a WAN performance result. Benchmarking and tuning remain
necessary before making speed claims.

## Upstream and license

Flock is an MIT-licensed fork, not an upstream croc release. See [UPSTREAM.md](UPSTREAM.md)
for the pinned base and upstream documentation. The original [LICENSE](LICENSE)
and upstream attribution are preserved. The Go module path remains the upstream
path in this first slice to avoid unrelated import churn; build Flock from this
checkout using `./cmd/flock`.
