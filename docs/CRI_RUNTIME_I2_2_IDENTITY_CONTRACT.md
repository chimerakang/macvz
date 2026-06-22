# CRI-I2-2 Late-Rootfs Identity File and Handoff Writer Contract (#113)

Date: 2026-06-22

Outcome: `runtimeIdentityContractImplemented`

## Purpose

Define and implement the runtime contract for the *expected* identity the runtime
states and the *observed* identity the launched process reports, as proven by
CRI-R15 and specified by CRI-R16 (`docs/CRI_RUNTIME_R16_HANDOFF_DESIGN.md`). This
is the runtime side only; CRI status presentation and StartContainer wiring are
later tasks (CRI-I3).

Implemented in `pkg/runtime/identity.go`, building on the #109 path lifecycle and
#110 `HandoffMeta`.

## Two identity files

| File | Direction | Path | Written by |
| --- | --- | --- | --- |
| Staged rootfs identity | expected (input) | `<rootfs>/etc/macvz-container-identity` (`RootfsIdentityPath`) | runtime, before launch |
| Handoff evidence | observed (output) | `HandoffLayout.IdentityFile` = `<handoff>/identity`, mounted at `/run/macvz/handoff/identity` | the launched process |

Both use the same line-oriented format so the contract is symmetric: the runtime
states the expected identity, the process echoes the observed one.

## Evidence format

Line-oriented `key=value`, one per line:

```
identity=macvz-r9-id=late-alpha
expected=macvz-r9-id=late-alpha
proc_root=/
mounts=...
pid=1234
startedAt=2026-06-22T10:00:00Z
```

Parsing rules (`ParseHandoffEvidence`):

- Split on the **first** `=` so identity values that themselves contain `=`
  (R15's `macvz-r9-id=late-alpha`) round-trip intact.
- Keys are matched **exactly** (case-sensitive) after trimming spaces; values are
  trimmed of surrounding spaces and a trailing CR.
- Blank lines and `#` comments are ignored.
- `identity` is the only required key and is the observed identity. The first
  `identity` wins; duplicates are preserved as `identity#2`, … diagnostics rather
  than silently merged.
- Every other key (`expected`, `proc_root`, `mounts`, `pid`, `startedAt`, …) is a
  diagnostic, captured in first-seen order. **`proc_root` and mount listings are
  diagnostics, never success criteria** (R15: `proc_root=/` reflects the private
  mount namespace and is expected).
- A non-blank, non-comment line with no `=` → `ErrEvidenceMalformed`. No
  `identity` key, or an empty/absent file → `ErrEvidenceMissing`.

## Verification

`VerifyHandoffIdentity(meta, now)` reads `meta.IdentityFile`, records the observed
identity and result onto `meta` (via `HandoffMeta.Verify`, exact match), and
returns:

- `nil` + evidence when observed == expected (`meta.Status = Verified`).
- `ErrIdentityMismatch` wrapping **both** expected and observed identity when they
  differ (`Status = Mismatch`).
- `ErrEvidenceMissing` wrapping the **expected** identity when the file is
  absent/empty/has no identity (`Status = Missing`). Malformed evidence is a
  present-but-unusable channel: surfaced as `ErrEvidenceMalformed` with
  `Status = Missing`.

Runtime errors include expected/observed identity where safe; diagnostics never
affect the success decision.

## Failed-start diagnostics

`ReadStderrDiagnostics(handoffDir)` returns the optional `<handoff>/stderr`
capture for a failed start, best-effort: a missing file yields `""` and no error,
and the read is size-bounded (16 KiB) so a runaway process cannot produce an
unbounded diagnostic.

## Staging / recovery helpers

- `StageIdentityFile(rootfsDir, identity)` writes the expected identity into the
  prepared rootfs (creating `/etc`), in the canonical format. The guest-absolute
  identity path is cleaned and converted to a rootfs-relative host path before
  writing, so malformed guest paths cannot escape the prepared rootfs helper.
- `ReadStagedIdentity(rootfsDir)` reads it back, letting a restarted runtime
  recover the **recoverable** `ExpectedIdentity` field of `HandoffMeta` from the
  prepared rootfs spec.
- `FormatIdentity(identity)` renders the canonical single line.

## Tests (`pkg/runtime/identity_test.go`)

Parser: success (incl. `=`-containing value + proc_root diagnostic), extra
diagnostics with order preserved, missing identity, empty input, malformed
(no separator / empty key), duplicate-identity first-wins. Staging: stage→read
round-trip, empty-identity/empty-rootfs rejection, missing-file recovery error.
Verification:
verified, mismatch (error carries expected+observed), missing file, malformed,
bad metadata without panic.
Stderr: absent→empty, present, bounded.

`go test ./pkg/runtime/...`, `go vet ./...`, and full `go build ./...` /
`go test ./...` are green.

## Non-goals (honored)

- No CRI status presentation beyond runtime error values.
- `proc_root` is not required to match a host-visible path.
