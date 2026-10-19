# Transfer benchmarks

`compare.py` uses two computers with existing machine-key SSH access and
Python 3. Stage the exact binaries on each machine first. On macOS, copy to a
new path and rename it into place; overwriting a running executable can invalidate
its cached code signature. Record `git rev-parse HEAD`, binary SHA-256, OS/CPU,
and the network route with any published results.

```sh
go build -trimpath -tags netgo,osusergo,croc_no_tailcat -o /tmp/flock-candidate .
scp /tmp/flock-candidate bench-peer:/tmp/flock-candidate-new
ssh bench-peer 'mv /tmp/flock-candidate-new /tmp/flock-candidate'
# Install/stage the same pinned croc release on both computers, then:
python3 bench/compare.py \
  --ssh bench-peer --local-ip LOCAL_IPV4 --remote-ip REMOTE_IPV4 \
  --flock /tmp/flock-candidate --remote-flock /tmp/flock-candidate \
  --croc /path/to/pinned/croc --remote-croc /path/to/pinned/croc \
  --variants flock,croc4,croc8 --rounds 3 --output results.jsonl
```

For a quick functional check, add `--mib 64 --files 32 --rounds 1`.
For many-file batching comparisons, run `--variants flock,croczip --fixtures small`.
Add `--croc-quiet` to compare croc with all of its output disabled; the receipts
record this flag. This is a useful additional baseline for agent use.
To compare an installed Flock baseline, add `baseline` to `--variants` and set
`--baseline-flock` and `--remote-baseline-flock`. The runner never installs or
updates binaries. Both local and remote Python invocations bound each transfer;
use smaller fixtures on a slow WAN rather than exceeding the 120-second deadline.

Fixtures are independently generated random bytes: a 64 KiB startup file, a
1 GiB bulk file, and 1,024 approximately 4 KiB files plus a zero-byte file, a
Unicode filename and an empty directory. Generation, source inventory, and source
staging happen before timing. Each round reverses tool order. Both directions run
by default. The same persistent local relay serves every tool: Flock uses TCP
only for authenticated control and requires QUIC/UDP for file bytes; croc is
forced to its TCP relay data path with compression disabled and four/eight
connections. `croczip` adds croc's explicit ZIP batching mode.

`command_seconds` covers process startup, invitation handoff over SSH, transfer,
and native completion. Flock includes full sender and final receiver SHA-256;
croc retains its normal xxhash behavior. `verified_seconds` additionally includes
an independent SHA-256 pass over the received files for **both** tools. Every
file must match the source path, size and hash, and the empty directory must
survive. Neither time is a disk-durability/fsync measurement.

Output JSON Lines contain only timing/verification metrics. Private logs and
fixtures are retained at the printed temporary paths for investigating failures;
logs can contain bearer invitations and must not be published. Remove those exact
local/remote temporary directories when done. Allow disk space for each receive
copy (roughly `rounds × directions × variants × bulk size`). Run benchmarks
without concurrent transfers or builds; repeat on other hardware and routes before
generalizing a speedup. LAN, an overlay, and a geographic WAN are separate results.

## Profiling

Build the wrapper with `go build -o /tmp/flock-profile ./bench/profile`. It runs
the real agent CLI and writes a CPU profile when `FLOCK_CPU_PROFILE=/private/path`
is set. Stage/run it on each endpoint to profile both processes, then inspect
with `go tool pprof -top /private/path`. Profiles and invitation logs are private
benchmark artifacts, not release assets.

## macOS batching

The macOS adapter groups ready UDP packets without waiting for a batching timer.
It connects the socket only after the authenticated peer endpoint is known,
preserves ECN markings, copies queued payloads, retries transient send-buffer
backpressure, and uses Go's socket readiness
and cancellation machinery. There is a bounded send queue and a prefetched read
batch. QUIC encryption, congestion control, retransmission, and the wire protocol
are unchanged; this also interoperates with Flock 0.1.0.

Apple's `sendmsg_x`/`recvmsg_x` APIs are private and can change. Missing symbols or
unsupported operations use ordinary UDP calls; `FLOCK_DISABLE_UDP_BATCH=1` disables
the adapter for diagnosis or comparison. This changes packet handling only and
never permits TCP file-data fallback. Other platforms retain quic-go's normal
UDP path. `purego` calls libSystem without requiring Cgo or a compiler on the
recipient machine. Errno is captured in the same foreign call to avoid observing
runtime changes between calls. Oversized path-MTU probes retain quic-go's loss
semantics instead of closing the connection. Tests inject partial sends, buffer
exhaustion, interruption, and oversized probes, including cancellation during a
buffer retry.

Sources: [Apple message-batch API and ABI](https://github.com/apple-oss-distributions/xnu/blob/main/bsd/sys/socket_private.h),
[quic-go packet handling](https://github.com/quic-go/quic-go/blob/v0.64.0/sys_conn_helper_darwin.go),
[purego](https://github.com/ebitengine/purego).
