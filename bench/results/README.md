# macOS LAN qualification, 2026-10-19

These measurements qualify specified modes on one pair of machines. The latest
candidate overlaps full source and destination SHA-256 with transfer and closes
the earlier bulk regression in the repeated 20 GiB comparison below. Earlier
candidate results remain for context. These are not universal UDP-versus-TCP or
WAN claims. Published v0.1.0 packages do not contain these changes.

## Current candidate: both full hashes overlap transfer

Source: `b3cb13403e29e3d91719f37a80f517d14578e390`, Go 1.27.0,
`CGO_ENABLED=0`, tags `netgo,osusergo,croc_no_tailcat`. Both endpoints used and now
have installed the identical ARM64 binary as `~/.local/bin/flock-dev`, SHA-256
`2933e7bf051a3f28fc7f622e949cac1b06d0569251b60162d46a63ff12c86002`.

For one file of at least 8 MiB, invitation version 2 declares paths and sizes
immediately. Full source SHA-256 runs during transfer and arrives over the
PAKE-authenticated encrypted control channel. Destination readback also overlaps
transfer. Completion requires that authenticated digest, complete chunk
coverage, full destination SHA-256, size and file identity checks. Missing,
incorrect, repeated or inventory-changing manifests fail. Source changes and
cancellation fail. The control feature is negotiated before data setup.

Small/many-file transfers retain version 1 and their upfront hashes. A large-file
sender can select `--legacy-invitation` to commit the digest before pairing or
serve an older recipient; the new receiver supports both versions. The final
completed manifest always contains the full SHA-256. Chunk encryption and QUIC
file-data requirements remain intact.

Three alternating A → B trials per tool transferred a full random
21,474,836,480-byte file (20 GiB / 21.47 GB). The source was temporary RAM-backed
APFS because the internal disk lacked space; the recipient used its internal
SSD. Generation/staging preceded timing. Both tools used the same persistent
controller-hosted relay for control; Flock data was direct QUIC/UDP, and pinned
croc 11.5.4 used eight TCP relay connections, `--quiet`, and no compression.
Hardware, routing and the croc binary match the earlier method below.

| Tool | Median command | Median including independent SHA-256 |
| --- | ---: | ---: |
| Flock / invitation v2 | 77.736 s | 86.290 s |
| croc eight TCP connections + quiet | 80.066 s | 88.757 s |

All six independent checks passed. Flock took **2.9% less time** in command time
and **2.8% less time** including the additional readback. Flock's range was
77.676–79.369 s; croc's was 79.994–83.089 s. This closes the measured regression;
it is a modest advantage on this route, not a 10× bulk-speed claim. The median
invitation-ready time fell from 9.063 s on the preceding candidate to **49 ms**;
the full source hash still runs and is mandatory before success.
[All six canonical 20 GiB receipts](2026-10-19-deferred-20gib.jsonl).

[Two further 64 MiB transfers](2026-10-19-deferred-bidirectional.jsonl) passed
with version 2 in both directions. [Four mixed-version checks](2026-10-19-deferred-compatibility.jsonl)
passed using 64 KiB and 64 MiB files: the new sender selected legacy invitations
for v0.1.0 recipients, and the older sender explicitly bound its advertised local
interface as described below. These were functional checks during large-fixture
generation; their timings are not performance claims.

Full Go tests, focused race tests, real UDP process tests, Cgo-free tests and
macOS/Linux/Windows CI passed for this implementation. Tests exercise delayed and
missing digests, wrong digests, altered inventory, duplicate manifests, changed
sources, both invitation versions and the destination coverage/error cases
listed below. One inherited legacy relay startup test missed its ephemeral
listener during the first full local suite; its isolated recheck and the full
suite rerun passed. No production fallback or verification bypass was added.

## Earlier candidate: builds and method

- Candidate transport source: `9d152c8971717ad7547241477cbb1bb33d09733a`.
  Go 1.27.0, `CGO_ENABLED=0`, `-trimpath`, tags
  `netgo,osusergo,croc_no_tailcat`. Both endpoints used the identical ARM64 binary:
  SHA-256 `09f1e4a1a946ccd7bc9ba573a3e0fbacc97a5d82007534f738e908537bddce73`.
- croc: official 11.5.4 macOS ARM64 binary on both endpoints, SHA-256
  `57e1b804550aeb736a1994096ac7a9aa534408be3cbb5e064d211270bd6fe43b`.
- A: Apple M3 Ultra / macOS 26.1. B: Apple M3 Ultra / macOS 26.3.
  IPv4 LAN with MTU 1500 and normal system routing, including a bridge interface.
  Both machines have multiple interfaces. This is a routed LAN comparison, not
  an isolated raw-NIC throughput test or a geographic WAN test.
- One persistent controller-hosted control relay for all tools, co-located with
  A. Flock uses eight QUIC streams for file data. croc is forced to TCP relay
  mode with eight connections, local discovery off and transfer compression off.
  The ZIP comparison explicitly enables croc's directory ZIP mode. These results
  do not compare every croc transport configuration.
- Three repetitions per tool/workload/direction, alternating tool order.
  Generation and staging precede timing. Fixtures: 1 GiB random bytes; and 1,024
  approximately 4 KiB random files plus a Unicode file, zero-byte file and empty
  directory (1,026 files / 4,242,303 bytes). Every receive passed independent
  path/size/SHA-256 comparison and preserved the empty directory.

`Command` includes startup, SSH handoff and native completion. Flock additionally
includes its own sender/final-receiver full SHA-256 passes; croc uses its normal
xxhash behavior. `Verified` adds the same independent SHA-256 pass for both tools.
Neither measures fsync durability. All values below are medians in seconds.
See [the runner and reproduction instructions](../README.md).

## Normal file mode

| Direction / workload | Flock command | croc command | Flock verified | croc verified |
| --- | ---: | ---: | ---: | ---: |
| A → B / 1 GiB | 5.128 | 4.437 | 5.808 | 5.104 |
| B → A / 1 GiB | 5.268 | 4.748 | 5.772 | 5.235 |
| A → B / 1,026 files | 3.042 | 38.795 | 3.341 | 39.099 |
| B → A / 1,026 files | 2.439 | 41.051 | 2.542 | 41.154 |

Flock's directory command completes **12.8–16.8× faster** in this comparison;
including independent verification, **11.7–16.2× faster**. The 1 GiB command takes
**11–16% longer** than croc. Receipts: [24 transfers](2026-10-19-lan.jsonl).

## 20 GiB single-file transfer

A further single A → B trial per tool transferred one full random file of
21,474,836,480 bytes (20 GiB / 21.47 GB). The source was a temporary RAM-backed
APFS volume because A's internal disk lacked space; B received onto its internal
SSD. Source generation and inventory hashing preceded timing. These are not
repeated medians or storage-durability measurements. croc used eight TCP relay
connections with `--quiet` and compression disabled; Flock used direct QUIC/UDP
with its normal full SHA-256 verification. Builds and routing match above.

| Tool | Command | Including independent SHA-256 |
| --- | ---: | ---: |
| Flock | 97.451 s | 106.203 s |
| croc eight connections + quiet | 81.097 s | 89.742 s |

Both destination SHA-256 checks passed. Flock took **20.2% longer** in command
time and **18.3% longer** including the independent check. Its command-time rate
was approximately **210 MiB/s**, versus **253 MiB/s** for croc. This confirms a
successful large transfer, and does not establish a bulk-speed advantage.
[Both 20 GiB receipts](2026-10-19-20gib.jsonl).

## Explicit ZIP mode

ZIP mode uses a separate interleaved run of the same directory fixture. All
files, Unicode names, the zero-byte entry and the empty directory survived.

| Direction | Flock command | croc ZIP command | Flock verified | croc ZIP verified |
| --- | ---: | ---: | ---: | ---: |
| A → B | 2.667 | 7.033 | 2.958 | 7.314 |
| B → A | 2.406 | 5.783 | 2.509 | 5.882 |

Flock completes **2.4–2.6× faster** in command time and **2.3–2.5× faster** with
independent verification. [12 ZIP comparison receipts](2026-10-19-zip.jsonl).

## Quiet-mode agent baseline

A further three-repetition A → B directory comparison adds croc's `--quiet`
flag to both croc endpoints. This reduces its ordinary-file runtime materially,
so the 12.8–16.8× figures above must not be described as wins over every croc
configuration. Flock continues emitting its normal JSON Lines and doing full
SHA-256 verification.

| Tool / directory mode | Command | Verified |
| --- | ---: | ---: |
| Flock | 3.090 | 3.369 |
| croc eight connections + quiet | 27.595 | 27.896 |
| croc ZIP + quiet | 7.119 | 7.412 |

Against this quieter baseline, Flock is **8.9× faster** in ordinary-file command
time and **2.3× faster** in ZIP command time. With independent verification, the
figures are **8.3×** and **2.2×**. This extra comparison covers A → B only;
[all nine receipts](2026-10-19-quiet.jsonl) passed independent SHA-256 checks.

## Intermediate candidate: destination verification overlaps

The source at `1cb78ae7873c4c75b2310ed302417bf6643fc66e` overlaps full
SHA-256 readback of completed destination regions with the remaining transfer.
A coverage bitset rejects duplicate chunks and prevents hashing unwritten holes;
completion waits for every byte, the full digest, size and file identity checks.
Small files retain post-transfer verification. For one large file, the engine
uses its negotiated sampled transfer hint rather than scanning the whole source
again for xxhash. The invitation still contains a full source SHA-256. No chunk
encryption, authenticated wire format or required verification is removed.

Both endpoints used the identical Go 1.27.0 / Cgo-free ARM64 binary, SHA-256
`e2045fd1b6c1436641016b122c522dabb6576ff67dd6fc5d1552cd218895e56c`.
The later Windows test correction changes no production code. The following
comparison uses the same two hosts, route and pinned croc 11.5.4 build as above.
One full random 20 GiB file came from a RAM-backed APFS source; the recipient
used its internal SSD. Generation/staging preceded timing. Three trials per
tool alternated order; this update measures A → B only. croc used eight TCP
relay connections, `--quiet`, and no compression.

| Tool | Median command | Median including independent SHA-256 |
| --- | ---: | ---: |
| Flock | 86.295 s | 94.987 s |
| croc eight connections + quiet | 82.214 s | 90.789 s |

All six transfers passed independent destination SHA-256 checks. Flock still
took **5.0% longer** in command time and **4.6% longer** including the additional
check. Its median source preparation alone was **9.063 seconds** before the
invitation: full source SHA-256 remains on the critical path. The new median
uses **11.4% less time** than the earlier candidate's 97.451-second single trial;
that earlier measurement was not a repeated controlled baseline. These results
do not establish parity or a bulk-speed advantage over croc.
[All six current-candidate receipts](2026-10-19-bulk-verification.jsonl).

The candidate is installed separately as `~/.local/bin/flock-dev` on both
machines, with matching binary SHA-256 readback. A further interleaved
three-trial directory check passed all independent hashes: median **3.241 s**
for this candidate versus **3.072 s** for the preceding candidate (binary
`09f1e4a1a946ccd7bc9ba573a3e0fbacc97a5d82007534f738e908537bddce73`).
Ranges were 2.723–4.159 s and 3.014–3.318 s, respectively. This noisy run does
not establish a directory improvement from the new hashing change.
[Six directory-check receipts](2026-10-19-bulk-directory-check.jsonl).

[Four additional mixed-version checks](2026-10-19-bulk-compatibility.jsonl)
passed with 64 KiB and 64 MiB files, using both the current sender with the
installed v0.1.0 recipient and v0.1.0 sender with the current recipient. On this
multi-interface pair, the older sender required
`--udp-listen ADVERTISED_LOCAL_IPV4:0`; its default wildcard bind timed out twice
and is not qualified by this run. The current sender binds the advertised local
interface automatically. Failed setups are retained privately and are not
counted as successful receipts.

Read-ahead, parallel-connection and frame-coalescing experiments did not show
repeatable throughput gains and were discarded. Qualification of the retained
change includes corruption, missing/duplicate/out-of-order chunks, file identity
replacement, wrong lengths, cancellation, bounded coverage memory, real UDP
process transfers, full Go tests, focused race tests and Cgo-free tests. Windows
blocks replacement of an open destination at the kernel level; its test accepts
that protection instead of requiring Unix rename semantics.

## Before the changes

The installed Flock v0.1.0 ARM64 binary was
`6c1a2f254686c4a9b1a31964df1a7564a9e47a409751fdf46a3ab0730e4904fa`.
Three earlier A → B repetitions gave median command times of **22.732 seconds**
for 1 GiB and **28.389 seconds** for the directory. The earlier runner omitted
one local Python timeout wrapper, so the tiny startup overhead is not identical;
fixtures, binary, relay mode and independent verification are otherwise
comparable. The final candidate's bulk command is approximately **4.4× faster**
than that installed baseline. [Baseline receipts](2026-10-19-flock01.jsonl).

## Compatibility and reliability

[Four mixed-version transfers](2026-10-19-compatibility.jsonl) exercised the new
sender with a v0.1.0 receiver and a v0.1.0 sender with the new receiver, using
64 KiB and 64 MiB random files. Every independent SHA-256 check passed. The
wire protocol and final verification contract are unchanged. The candidate is
also installed as `~/.local/bin/flock-dev` on both test machines with the binary
SHA-256 above; the packaged v0.1.0 installation is separate. The development
binary retains the base distribution version string until a release is made.

Qualification also includes the full Go suite, focused agent/transport race
tests, the Cgo-free release configuration, Linux/Windows cross-builds, and CI
on macOS, Linux and Windows. Native socket tests cover ECN, owned packet buffers,
truncated reads, repeated deadlines, ordinary-read fallback, partial
sends, transient `ENOBUFS`/`EINTR`, oversized MTU probes, and closing a full queue
or cancelling during send-buffer backpressure. Earlier prototypes failed under
sustained load; those failures prompted the errno/backpressure fixes and are not
counted as successful qualification runs.

Geographic WAN performance remains **unqualified**: an off-site SSH endpoint was
not reachable during this run. Reachable IPv4/overlay operation does not prove
public NAT traversal, UDP relay availability, or WAN congestion behavior.
