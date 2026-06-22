package runtime

// identity.go implements the late-rootfs identity file and handoff writer
// contract (CRI-I2-2, #113), the runtime side of the evidence channel proven by
// CRI-R15 and specified by CRI-R16 (docs/CRI_RUNTIME_R16_HANDOFF_DESIGN.md).
//
// Two files carry identity:
//
//   - The runtime stages an *expected* identity file into the prepared rootfs at
//     RootfsIdentityPath before launch. It proves which rootfs was used.
//   - The launched process writes an *observed* identity into the handoff
//     evidence file (HandoffLayout.IdentityFile, mounted at
//     HandoffMountPoint/IdentityFile). It proves the process reported that fact
//     back to the runtime.
//
// Both use the same line-oriented key=value format. Verification is exact: the
// observed identity must equal the expected identity (HandoffMeta.Verify).
// proc_root and mount listings are diagnostics, never success criteria.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// RootfsIdentityPath is the in-rootfs (guest-absolute) path of the staged
// expected-identity file. CRI-R16 names it as the proof of which rootfs was
// used. It is relative-joined onto the prepared rootfs directory on the host.
const RootfsIdentityPath = "/etc/macvz-container-identity"

// Evidence keys (exact match, case-sensitive). identity is the only required
// key; expected is an optional self-report; everything else is a diagnostic.
const (
	// EvidenceIdentityKey carries the observed rootfs identity. Required.
	EvidenceIdentityKey = "identity"
	// EvidenceExpectedKey carries the process's self-reported expected identity.
	// Optional and advisory: the runtime trusts its own ExpectedIdentity, not
	// this field.
	EvidenceExpectedKey = "expected"
)

// StderrFile is the optional per-container stderr capture the runtime reads for
// diagnostics when a late process fails to start, under the handoff directory.
const StderrFile = "stderr"

// Errors returned by the identity/evidence contract. Callers compare with
// errors.Is; the wrapped detail includes expected/observed identity where safe.
var (
	// ErrEvidenceMissing means the handoff identity file was absent, empty, or
	// carried no identity key. Maps to IdentityMissing.
	ErrEvidenceMissing = errors.New("runtime: handoff identity evidence missing")

	// ErrEvidenceMalformed means a non-empty, non-comment evidence line had no
	// key=value separator and could not be parsed.
	ErrEvidenceMalformed = errors.New("runtime: handoff identity evidence malformed")

	// ErrIdentityMismatch means evidence was present but the observed identity did
	// not equal the expected identity.
	ErrIdentityMismatch = errors.New("runtime: rootfs identity mismatch")
)

// HandoffEvidence is the parsed handoff identity file. Identity is the observed
// rootfs identity; Diagnostics holds every other key (expected, proc_root,
// mounts, pid, startedAt, ...) so a failed start can report context without
// those values ever affecting the success decision.
type HandoffEvidence struct {
	// Identity is the observed identity (the value of the identity key). Empty
	// only when parsing returned ErrEvidenceMissing.
	Identity string
	// Diagnostics are all non-identity keys, in the file's key order via Keys.
	Diagnostics map[string]string
	// keys preserves first-seen order of Diagnostics for stable rendering.
	keys []string
}

// Get returns a diagnostic value and whether it was present.
func (e HandoffEvidence) Get(key string) (string, bool) {
	v, ok := e.Diagnostics[key]
	return v, ok
}

// DiagnosticKeys returns the diagnostic keys in first-seen order.
func (e HandoffEvidence) DiagnosticKeys() []string { return e.keys }

// ParseHandoffEvidence parses line-oriented key=value evidence. Rules:
//
//   - A line is split on the FIRST '=' so identity values that themselves
//     contain '=' (e.g. "macvz-r9-id=late-alpha") round-trip intact.
//   - Keys are matched exactly (case-sensitive) after trimming surrounding
//     spaces; values are trimmed of surrounding spaces and a trailing CR.
//   - Blank lines and lines beginning with '#' are ignored.
//   - The first identity key wins; later identity lines are kept as diagnostics
//     under "identity#2", ... so duplicates are visible but not silently merged.
//   - A non-blank, non-comment line with no '=' is malformed (ErrEvidenceMalformed).
//   - No identity key at all is ErrEvidenceMissing.
func ParseHandoffEvidence(r io.Reader) (HandoffEvidence, error) {
	ev := HandoffEvidence{Diagnostics: map[string]string{}}
	haveIdentity := false
	dupIdentity := 1

	sc := bufio.NewScanner(r)
	// Allow long mount listings without tripping bufio's default 64KiB line cap.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		raw := sc.Text()
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return HandoffEvidence{}, fmt.Errorf("%w: line without separator: %q", ErrEvidenceMalformed, trimmed)
		}
		key := strings.TrimSpace(k)
		val := strings.TrimSpace(v)
		if key == "" {
			return HandoffEvidence{}, fmt.Errorf("%w: empty key: %q", ErrEvidenceMalformed, trimmed)
		}
		if key == EvidenceIdentityKey {
			if !haveIdentity {
				ev.Identity = val
				haveIdentity = true
				continue
			}
			dupIdentity++
			key = fmt.Sprintf("%s#%d", EvidenceIdentityKey, dupIdentity)
		}
		if _, seen := ev.Diagnostics[key]; !seen {
			ev.keys = append(ev.keys, key)
		}
		ev.Diagnostics[key] = val
	}
	if err := sc.Err(); err != nil {
		return HandoffEvidence{}, fmt.Errorf("runtime: read handoff evidence: %w", err)
	}
	if !haveIdentity {
		return HandoffEvidence{}, fmt.Errorf("%w: no %q key", ErrEvidenceMissing, EvidenceIdentityKey)
	}
	return ev, nil
}

// ReadHandoffEvidence reads and parses the evidence file at path. A missing or
// empty file is reported as ErrEvidenceMissing (not a generic I/O error) so the
// caller can treat "process never wrote evidence" uniformly with "no identity
// key".
func ReadHandoffEvidence(path string) (HandoffEvidence, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return HandoffEvidence{}, fmt.Errorf("%w: file %s absent", ErrEvidenceMissing, path)
		}
		return HandoffEvidence{}, fmt.Errorf("runtime: read handoff evidence %s: %w", path, err)
	}
	if len(b) == 0 {
		return HandoffEvidence{}, fmt.Errorf("%w: file %s empty", ErrEvidenceMissing, path)
	}
	return ParseHandoffEvidence(strings.NewReader(string(b)))
}

// FormatIdentity renders an identity value as a single evidence line. It is the
// canonical content of the staged rootfs identity file and the minimum a process
// must write to the handoff file.
func FormatIdentity(identity string) string {
	return EvidenceIdentityKey + "=" + identity + "\n"
}

// StageIdentityFile writes the expected identity into the prepared rootfs at
// rootfsDir + RootfsIdentityPath, creating the parent directory. The staged file
// is the same line-oriented format the handoff parser reads, so the contract is
// symmetric: the runtime states the expected identity, the process echoes the
// observed one. identity must be non-empty.
func StageIdentityFile(rootfsDir, identity string) error {
	if rootfsDir == "" {
		return fmt.Errorf("runtime: stage identity: empty rootfs dir")
	}
	if identity == "" {
		return fmt.Errorf("runtime: stage identity: empty identity")
	}
	dst, err := rootfsGuestPath(rootfsDir, RootfsIdentityPath)
	if err != nil {
		return fmt.Errorf("runtime: stage identity: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("runtime: stage identity dir: %w", err)
	}
	if err := os.WriteFile(dst, []byte(FormatIdentity(identity)), 0o644); err != nil {
		return fmt.Errorf("runtime: stage identity file: %w", err)
	}
	return nil
}

// ReadStagedIdentity reads back the expected identity staged into rootfsDir. It
// is the inverse of StageIdentityFile and lets a restarted runtime recover the
// expected identity from the prepared rootfs spec (the recoverable field in
// HandoffMeta). A missing file is ErrEvidenceMissing.
func ReadStagedIdentity(rootfsDir string) (string, error) {
	dst, err := rootfsGuestPath(rootfsDir, RootfsIdentityPath)
	if err != nil {
		return "", fmt.Errorf("runtime: read staged identity: %w", err)
	}
	ev, err := ReadHandoffEvidence(dst)
	if err != nil {
		return "", err
	}
	return ev.Identity, nil
}

// VerifyHandoffIdentity reads the handoff evidence file recorded on meta, records
// the observed identity and verification result back onto meta (stamped with
// now), and returns nil only when the observed identity exactly matches
// meta.ExpectedIdentity. On failure it returns an error wrapping ErrEvidenceMissing
// or ErrIdentityMismatch and including expected/observed identity where safe;
// meta.Status reflects the outcome (Missing or Mismatch) so the caller need not
// re-derive it. proc_root and mount diagnostics never affect the result.
func VerifyHandoffIdentity(meta *HandoffMeta, now time.Time) (HandoffEvidence, error) {
	if meta == nil {
		return HandoffEvidence{}, errors.New("runtime: verify handoff identity: nil metadata")
	}
	if meta.IdentityFile == "" {
		meta.Verify("", now)
		return HandoffEvidence{}, fmt.Errorf("%w: empty identity file path (expected %q)", ErrEvidenceMissing, meta.ExpectedIdentity)
	}
	ev, err := ReadHandoffEvidence(meta.IdentityFile)
	if err != nil {
		if errors.Is(err, ErrEvidenceMissing) {
			meta.Verify("", now) // resolves to IdentityMissing
			return HandoffEvidence{}, fmt.Errorf("%w (expected %q)", err, meta.ExpectedIdentity)
		}
		// Malformed evidence is a present-but-unusable channel: treat as missing
		// identity for status, but surface the malformed cause.
		meta.Verify("", now)
		return HandoffEvidence{}, err
	}
	switch meta.Verify(ev.Identity, now) {
	case IdentityVerified:
		return ev, nil
	case IdentityMismatch:
		return ev, fmt.Errorf("%w: expected %q, observed %q", ErrIdentityMismatch, meta.ExpectedIdentity, ev.Identity)
	default: // IdentityMissing (empty observed value despite a present key)
		return ev, fmt.Errorf("%w: empty observed identity (expected %q)", ErrEvidenceMissing, meta.ExpectedIdentity)
	}
}

// WaitForHandoffIdentity blocks until the handoff identity is verified, a
// terminal failure occurs, or ctx is done, polling the evidence file every
// interval. A missing or empty evidence file is retried (the late process may
// not have written it yet); a mismatch or malformed file is terminal and returns
// immediately. meta is updated in place with the final result via
// VerifyHandoffIdentity, so meta.Status reflects the outcome. The now function
// supplies VerifiedAt for each attempt (tests pass a fixed clock).
//
// On ctx expiry it returns the last attempt's evidence and an error wrapping
// ErrEvidenceMissing that names the expected identity and the ctx cause, so a
// timeout is reported as "evidence never arrived", not a generic deadline error.
func WaitForHandoffIdentity(ctx context.Context, meta *HandoffMeta, now func() time.Time, interval time.Duration) (HandoffEvidence, error) {
	if meta == nil {
		return HandoffEvidence{}, errors.New("runtime: wait for handoff identity: nil metadata")
	}
	if now == nil {
		now = time.Now
	}
	if interval <= 0 {
		interval = 50 * time.Millisecond
	}
	for {
		if err := ctx.Err(); err != nil {
			return HandoffEvidence{}, fmt.Errorf("%w: evidence did not arrive before deadline (expected %q): %v",
				ErrEvidenceMissing, meta.ExpectedIdentity, err)
		}
		ev, err := VerifyHandoffIdentity(meta, now())
		if err == nil {
			return ev, nil
		}
		if !errors.Is(err, ErrEvidenceMissing) {
			// Mismatch or malformed: present-but-wrong evidence is terminal; waiting
			// longer cannot turn a wrong identity into the right one.
			return ev, err
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return ev, fmt.Errorf("%w: evidence did not arrive before deadline (expected %q): %v",
				ErrEvidenceMissing, meta.ExpectedIdentity, ctx.Err())
		case <-timer.C:
		}
	}
}

// ReadStderrDiagnostics returns the contents of the optional stderr capture under
// handoffDir for a failed start, best-effort: a missing file yields "" and no
// error, since stderr capture is a convenience, not a contract. The result is
// size-bounded so a runaway process cannot make a diagnostic unbounded.
func ReadStderrDiagnostics(handoffDir string) string {
	const maxBytes = 16 * 1024
	f, err := os.Open(filepath.Join(handoffDir, StderrFile))
	if err != nil {
		return ""
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxBytes))
	if err != nil {
		return ""
	}
	return strings.TrimRight(string(b), "\n")
}
