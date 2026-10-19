# macOS LAN qualification, 2026-10-19

These are measurements of the specified modes on this pair of machines, not a
universal UDP-versus-TCP claim. The source branch is faster on the many-file
workload; croc remains faster on the bulk file. Published v0.1.0 packages do not
contain these changes.

## Builds and method

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
