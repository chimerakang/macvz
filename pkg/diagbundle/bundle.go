package diagbundle

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Source is one collector in a bundle. Name is the relative path the output is
// written to within the bundle (e.g. "runtime/container-status.txt"); Collect
// gathers the raw bytes, which the Builder redacts before writing. A Collect
// error is recorded in the bundle (so the failure itself is debuggable) rather
// than aborting the whole bundle — a missing helper must not prevent collecting
// runtime and control-plane state.
type Source struct {
	// Name is the relative file path within the bundle.
	Name string
	// Collect returns the raw (un-redacted) content for this source.
	Collect func(ctx context.Context) ([]byte, error)
}

// FileResult records what happened to one source in the assembled bundle.
type FileResult struct {
	Name  string
	Bytes int
	Err   string
}

// Result summarises an assembled bundle.
type Result struct {
	// Dir is the bundle directory.
	Dir string
	// Archive is the tar.gz path, empty when archiving was not requested.
	Archive string
	// Files records each source's outcome, in bundle order.
	Files []FileResult
}

// Builder assembles a redacted diagnostic bundle from a set of sources.
type Builder struct {
	redactor *Redactor
	sources  []Source
	// now supplies the bundle timestamp; injected for deterministic tests.
	now func() time.Time
}

// NewBuilder returns a Builder using the default redactor. Add collectors with
// Add before calling Build.
func NewBuilder() *Builder {
	return &Builder{redactor: DefaultRedactor(), now: time.Now}
}

// WithRedactor overrides the redactor (used by tests).
func (b *Builder) WithRedactor(r *Redactor) *Builder {
	b.redactor = r
	return b
}

// WithClock overrides the timestamp source (used by tests).
func (b *Builder) WithClock(now func() time.Time) *Builder {
	b.now = now
	return b
}

// Add appends a source to the bundle.
func (b *Builder) Add(name string, collect func(ctx context.Context) ([]byte, error)) *Builder {
	b.sources = append(b.sources, Source{Name: name, Collect: collect})
	return b
}

// DirName is the bundle directory name for the builder's timestamp, of the form
// "macvz-bundle-<node>-YYYYMMDDd'T'HHMMSSZ". A blank node is omitted.
func (b *Builder) DirName(node string) string {
	ts := b.now().UTC().Format("20060102T150405Z")
	if node == "" {
		return "macvz-bundle-" + ts
	}
	return "macvz-bundle-" + sanitize(node) + "-" + ts
}

// Build runs every source, redacts its output, and writes the bundle under
// parentDir in a timestamped directory. Each source becomes a file; failures
// are captured as ".error" sidecar files and in the manifest, never aborting
// the run. A manifest.txt index is always written. Build does not archive; call
// Archive on the returned directory to produce a tar.gz.
func (b *Builder) Build(ctx context.Context, parentDir, node string) (Result, error) {
	dir := filepath.Join(parentDir, b.DirName(node))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Result{}, fmt.Errorf("create bundle dir: %w", err)
	}

	res := Result{Dir: dir}
	for _, s := range b.sources {
		fr := FileResult{Name: s.Name}
		raw, err := s.Collect(ctx)
		// Always redact whatever was produced, even on error: a command that fails
		// may still have emitted sensitive partial output on the way.
		redacted := b.redactor.RedactBytes(raw)

		target := filepath.Join(dir, filepath.FromSlash(s.Name))
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			fr.Err = err.Error()
			res.Files = append(res.Files, fr)
			continue
		}
		if writeErr := os.WriteFile(target, redacted, 0o600); writeErr != nil {
			fr.Err = writeErr.Error()
			res.Files = append(res.Files, fr)
			continue
		}
		fr.Bytes = len(redacted)
		if err != nil {
			fr.Err = err.Error()
			// Record the collection error alongside the (possibly empty) output so a
			// reviewer sees why a section is thin.
			errText := b.redactor.Redact(err.Error())
			_ = os.WriteFile(target+".error", []byte(errText+"\n"), 0o600)
		}
		res.Files = append(res.Files, fr)
	}

	manifest := b.renderManifest(node, res.Files)
	if err := os.WriteFile(filepath.Join(dir, "manifest.txt"), []byte(manifest), 0o600); err != nil {
		return res, fmt.Errorf("write manifest: %w", err)
	}
	return res, nil
}

// renderManifest builds the human-readable index written as manifest.txt.
func (b *Builder) renderManifest(node string, files []FileResult) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "MacVz diagnostic bundle\n")
	fmt.Fprintf(&sb, "node:      %s\n", emptyDash(node))
	fmt.Fprintf(&sb, "generated: %s\n", b.now().UTC().Format(time.RFC3339))
	fmt.Fprintf(&sb, "\nThis bundle was redacted: PEM private keys, WireGuard private/preshared\n")
	fmt.Fprintf(&sb, "keys, JWT/bearer tokens, and sensitive config values are replaced with %s.\n", Placeholder)
	fmt.Fprintf(&sb, "Review the contents before sharing; redaction is best-effort.\n\n")
	fmt.Fprintf(&sb, "contents:\n")

	sorted := append([]FileResult(nil), files...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	for _, f := range sorted {
		status := fmt.Sprintf("%d bytes", f.Bytes)
		if f.Err != "" {
			status = "ERROR: " + f.Err
		}
		fmt.Fprintf(&sb, "  %-40s %s\n", f.Name, status)
	}
	return sb.String()
}

// Archive packages dir into a gzip-compressed tar at dest. Paths inside the
// archive are kept relative to dir's parent, so unpacking recreates the
// timestamped bundle directory.
func Archive(dir, dest string) error {
	out, err := os.Create(dest)
	if err != nil {
		return fmt.Errorf("create archive: %w", err)
	}
	defer func() { _ = out.Close() }()

	gz := gzip.NewWriter(out)
	defer func() { _ = gz.Close() }()
	tw := tar.NewWriter(gz)
	defer func() { _ = tw.Close() }()

	base := filepath.Dir(dir)
	return filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(base, path)
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		_, err = io.Copy(tw, f)
		return err
	})
}

// sanitize keeps a string safe for use in a file name: alphanumerics, dash,
// dot, and underscore survive; anything else becomes a dash.
func sanitize(s string) string {
	var sb strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			sb.WriteRune(r)
		default:
			sb.WriteRune('-')
		}
	}
	return sb.String()
}

func emptyDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
