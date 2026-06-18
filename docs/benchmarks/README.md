# P1 Density & RAM-overhead benchmark

Measures the headline value props of macvz on real Apple Silicon hardware:
micro-VM **density**, **per-VM RAM overhead**, and **boot-latency distribution**.

The harness is [`cmd/mvz-bench`](../../cmd/mvz-bench). It drives the same
`runtime.Driver` the kubelet uses (apple/container backend), so the numbers
reflect the real runtime path, not a synthetic mock.

## How to reproduce

```sh
go build -o bin/mvz-bench ./cmd/mvz-bench

# Fixed fleet: boot N VMs with bounded parallelism, sample at steady state.
./bin/mvz-bench -count 12 -mem 256 -concurrency 4 -out docs/benchmarks/p1-results.json

# Density ceiling: boot sequentially until failure or the memory safety budget.
./bin/mvz-bench -find-ceiling -mem 256 -safety-fraction 0.3 -out docs/benchmarks/p1-ceiling.json
```

Key flags (`-h` for all):

| flag | meaning | default |
|------|---------|---------|
| `-count` | VMs to launch (fixed mode) | 10 |
| `-mem` | per-VM guest memory, MiB | 256 |
| `-concurrency` | VMs booting in parallel (fixed mode) | 4 |
| `-find-ceiling` | boot sequentially until failure/budget | off |
| `-safety-fraction` | never reserve more than this fraction of RAM | 0.5 |
| `-keep` | leave VMs running after measuring | off |
| `-out` | write JSON results to path | — |

**Safety:** the harness refuses to reserve more than `-safety-fraction` of total
RAM (`guest-mem × VM-count`) and always tears down every VM it created, even on
error or `Ctrl-C`.

## What is measured

- **Boot latency** — wall time from `Create` to the VM reporting `Running` with
  an assigned IP, per VM, summarized as a distribution.
- **Per-VM RAM overhead** — resident set size (RSS) of the per-VM
  `container-runtime-linux` helper process that backs each micro-VM. This is the
  host memory actually consumed per idle VM (the guest's 256 MiB is reserved but
  faulted in lazily, so RSS ≪ reservation for an idle workload).
- **System used-memory delta** — change in macOS used memory
  (active + wired + compressed, via `vm_stat`) across the run, divided by VM
  count. This is a looser upper bound: it also captures image/page cache and
  kernel structures, so it runs higher than the per-VM helper RSS.
- **Density ceiling** — how many VMs boot before a `Create`/`Start` fails. In
  `-find-ceiling` mode the harness pushes until either a real failure or the
  memory safety budget; the recorded `ceilingReached` distinguishes the two.

## Results

Hardware: **Apple M-series, 10 cores, 32 GiB RAM**, macOS (Darwin 25.5).
Image: `docker.io/library/alpine:3.20`. apple/container 1.0.0.
Workload per VM: `sleep` (idle). Raw data: [`p1-results.json`](p1-results.json),
[`p1-ceiling.json`](p1-ceiling.json).

### Fixed fleet — 12 VMs @ 256 MiB, concurrency 4

| metric | value |
|--------|-------|
| VMs launched | 12 / 12 |
| boot latency (Create→Running) | min **2.24 s**, p50 **2.88 s**, p90 3.13 s, max 3.23 s, mean **2.81 s** |
| per-VM RAM overhead (helper RSS) | p50 **21.2 MiB**, p90 23.0 MiB, max 37.3 MiB, mean **22.8 MiB** |
| system used-memory / VM | ~102 MiB |

### Density probe — sequential to 30% RAM budget (38 VMs @ 256 MiB)

| metric | value |
|--------|-------|
| VMs launched | **38** (no failure; capped by 30% RAM safety budget) |
| boot latency (Create→Running) | min **0.76 s**, p50 **0.83 s**, p90 0.96 s, max 1.02 s, mean **0.84 s** |
| per-VM RAM overhead (helper RSS) | p50 **17.0 MiB**, p90 22.5 MiB, max 37.9 MiB, mean **19.7 MiB** |
| system used-memory / VM | ~52 MiB |

## Takeaways

- **Boots in seconds.** Sequential boots land at **~0.8 s** (Create→Running);
  even at concurrency 4 the mean stays under **3 s**. This validates the P1
  acceptance "Go boots an Alpine micro-VM in seconds."
- **Per-VM overhead is ~17–23 MiB** of real host RAM for an idle Alpine VM —
  one to two orders of magnitude below a full Docker-Desktop-style Linux VM.
- **Density is reservation-bound, not overhead-bound.** 38 idle VMs consumed
  only ~2 GiB of real system memory; the limiting factor is the guest memory
  *reservation* (256 MiB × N), not the runtime's per-VM cost. Right-sizing guest
  memory, or oversubscription, is the lever for higher density. The probe hit no
  hardware failure within a safe budget; push `-safety-fraction` higher (on an
  otherwise-idle host) to find the hard ceiling.

> Numbers are point measurements on a shared developer machine and will vary
> with host load, guest workload, and guest memory size. Re-run the harness to
> reproduce on your own hardware.
