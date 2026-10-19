# Flock

Agent-first file transfer over **native QUIC/UDP**, with JSON Lines events,
automatic pairing invitations, deadlines, and verified receive results.

**Early release.** Directly reachable IPv4 peers work on a LAN or an overlay
such as Tailscale. Internet NAT discovery, UDP relay fallback, and geographic
WAN qualification are future work. Performance depends on workload and route; see
the [measured LAN results](bench/results/README.md).

## Install

### Homebrew (macOS and Linux)

```sh
brew install mhadifilms/tap/flock
flock --version
```

This tap installs a checksum-verified native binary. No Go or Python runtime is
required. To upgrade: `brew upgrade mhadifilms/tap/flock`.

### Go

With Go 1.27 or newer:

```sh
go install github.com/mhadifilms/flock@v0.1.0
flock --version
```

Prebuilt macOS, Linux, and Windows binaries and `SHA256SUMS` are available from
[Flock releases](https://github.com/mhadifilms/flock/releases). PyPI is not needed
for this native Go tool.

### From source

```sh
git clone https://github.com/mhadifilms/flock.git
cd flock
go build -o flock .
./flock schema
```

## Two agents, two processes

Start the sender with an IPv4 address the recipient can reach. It automatically
hosts a temporary TCP control relay and opens a UDP listener:

```sh
flock send --advertise 100.64.0.10 ./folder
```

Read stdout as JSON Lines. The `invitation` event contains an `invite` object.
Pass that object, or the whole event, to the recipient over your existing trusted
agent channel. Keep the sender process running.

On the other computer:

```sh
flock receive --out ./received --invite-file invitation.json
# Or pass the same JSON object on stdin:
flock receive --out ./received < invitation.json
```

There is no human code to choose or type, no clipboard dependency, and no
confirmation prompt. The invitation is a generated 256-bit bearer capability
with an expiry, control address, relative paths and sizes. For one large file,
version 2 lets transfer begin while the full source SHA-256 is computed; that
digest arrives over the authenticated encrypted control channel. The receiver
cannot succeed without matching it against full destination readback. Other
transfers use version 1 with SHA-256 hashes already in the invitation.
It grants access to this transfer: treat it as a secret when handing it off.

To include a large file's SHA-256 before pairing, or send to an older receiver,
use `flock send --legacy-invitation --advertise IPV4 FILE`. The new receiver
accepts both invitation versions. The final `completed` manifest includes every
full SHA-256 in both modes.

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
The sender does not yet receive a separate post-verification receipt. Large files
can be read back and hashed in completed regions while later regions arrive;
coverage, size, file identity and the full SHA-256 must all match before success.
Small files retain post-transfer verification.

Exit status is `0` for success, `1` for failure, `124` for timeout, and `130` for
cancellation. `--timeout` defaults to `30m`, includes invitation input and
verification, and accepts positive durations up to `24h`. SIGINT/SIGTERM cancel
the process. A broken stdout pipe cancels the transfer.

Receive destinations must be new directories. This first agent interface does
not expose resume or overwrite. Failed transfers can leave partial files in the
new destination. Symlinks and special files are rejected. Invitations are
limited to 4 MiB and 10,000 file/directory entries.

## Transport

Flock uses PAKE, encrypted control messages, chunk framing, file handling,
and authenticated transport negotiation. It adds a QUIC adapter using
[quic-go](https://github.com/quic-go/quic-go), with TLS 1.3 mutual authentication
bound to the PAKE session. The authenticated control channel exchanges UDP
endpoints; repeated outbound probes help establish bidirectional UDP traffic.
The default is eight bidirectional streams on one QUIC connection; set
`--streams 1` through `--streams 16` to experiment.

TCP carries pairing/control only. File data requires QUIC and **does not silently
fall back to TCP**. The peer must advertise Flock support. Both peers must run Flock for this file-data mode.

The advertised IPv4 address must be reachable at the actual UDP listener port.
The default embedded control listener uses a random TCP port; both that TCP port
and the UDP port must be reachable. To pin them for firewall rules:

```sh
flock send --advertise 100.64.0.10 \
  --control-listen 0.0.0.0:9009 --udp-listen 0.0.0.0:9010 ./folder
```

An existing compatible control relay can be selected with `--relay HOST:PORT`. Flock
also provides a control-only relay:

```sh
flock relay --listen 0.0.0.0:9009
flock send --relay relay.example:9009 --advertise 100.64.0.10 ./folder
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

The macOS performance branch batches ready QUIC datagrams and avoids looking up
hardware identity for every file. Repeated tests between two Macs cover both
transfer directions, a 1 GiB incompressible file, and a 1,026-file directory.
Every received file passes an independent SHA-256 comparison. The directory is
substantially faster than croc's normal transfer mode and its explicit ZIP mode;
croc remains faster on the bulk file in these LAN tests.

See [results, build fingerprints, and limits](bench/results/README.md) and the
[reproducible two-host runner](bench/README.md). These changes are source-branch
work and are **not included in the published v0.1.0 packages**. They do not
establish WAN performance or a universal speedup from using UDP.

## License and provenance

Flock is MIT-licensed. Original copyright notices remain in [LICENSE](LICENSE);
[UPSTREAM.md](UPSTREAM.md) records the fork's provenance. Go module dependencies
retain their respective licenses, included with the release archives.

## Building releases

`python3 packaging/flock_release.py --version 0.1.0 --output dist` creates
native archives for macOS, Linux, and Windows on ARM64 and AMD64, plus
`SHA256SUMS`. The version must match `internal/flockcli.Release`. The script
builds and packages only; publishing and updating the Homebrew formula are
separate steps. Inherited upstream packaging scripts are kept as source history
and are not used for Flock distribution.
