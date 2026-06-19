package diagbundle

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixedClock() func() time.Time {
	return func() time.Time { return time.Date(2026, 6, 19, 23, 30, 0, 0, time.UTC) }
}

func TestDirNameFormat(t *testing.T) {
	b := NewBuilder().WithClock(fixedClock())
	if got := b.DirName("mac-a"); got != "macvz-bundle-mac-a-20260619T233000Z" {
		t.Fatalf("dir name: %q", got)
	}
	if got := b.DirName(""); got != "macvz-bundle-20260619T233000Z" {
		t.Fatalf("blank node dir name: %q", got)
	}
	if got := b.DirName("weird/node name"); !strings.HasPrefix(got, "macvz-bundle-weird-node-name-") {
		t.Fatalf("unsanitized dir name: %q", got)
	}
}

func TestBuildWritesRedactedFilesAndManifest(t *testing.T) {
	parent := t.TempDir()
	b := NewBuilder().WithClock(fixedClock()).
		Add("config.txt", func(context.Context) ([]byte, error) {
			return []byte("nodeName: mac-a\npassword: hunter2\n"), nil
		}).
		Add("net/wg.txt", func(context.Context) ([]byte, error) {
			return []byte("PrivateKey = secretKeyMaterialXX=\nPublicKey = pubKeyXX=\n"), nil
		})

	res, err := b.Build(context.Background(), parent, "mac-a")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if filepath.Base(res.Dir) != "macvz-bundle-mac-a-20260619T233000Z" {
		t.Fatalf("dir: %s", res.Dir)
	}

	cfg, _ := os.ReadFile(filepath.Join(res.Dir, "config.txt"))
	if strings.Contains(string(cfg), "hunter2") {
		t.Fatalf("secret leaked into bundle: %s", cfg)
	}
	if !strings.Contains(string(cfg), "nodeName: mac-a") {
		t.Fatalf("non-secret context dropped: %s", cfg)
	}

	wg, _ := os.ReadFile(filepath.Join(res.Dir, "net", "wg.txt"))
	if strings.Contains(string(wg), "secretKeyMaterialXX=") {
		t.Fatalf("wg private key leaked: %s", wg)
	}
	if !strings.Contains(string(wg), "pubKeyXX=") {
		t.Fatalf("public key should survive: %s", wg)
	}

	manifest, _ := os.ReadFile(filepath.Join(res.Dir, "manifest.txt"))
	for _, want := range []string{"config.txt", "net/wg.txt", "mac-a", Placeholder} {
		if !strings.Contains(string(manifest), want) {
			t.Fatalf("manifest missing %q:\n%s", want, manifest)
		}
	}
}

func TestBuildRecordsSourceErrorsWithoutAborting(t *testing.T) {
	parent := t.TempDir()
	b := NewBuilder().WithClock(fixedClock()).
		Add("ok.txt", func(context.Context) ([]byte, error) { return []byte("fine"), nil }).
		Add("broken.txt", func(context.Context) ([]byte, error) {
			return []byte("partial token: leakme"), errors.New("command failed")
		})

	res, err := b.Build(context.Background(), parent, "n")
	if err != nil {
		t.Fatalf("Build should not abort on source error: %v", err)
	}

	// The good source still landed.
	if ok, _ := os.ReadFile(filepath.Join(res.Dir, "ok.txt")); string(ok) != "fine" {
		t.Fatalf("ok source content: %q", ok)
	}
	// The error sidecar exists and the partial output was still redacted.
	sidecar, err := os.ReadFile(filepath.Join(res.Dir, "broken.txt.error"))
	if err != nil {
		t.Fatalf("expected error sidecar: %v", err)
	}
	if !strings.Contains(string(sidecar), "command failed") {
		t.Fatalf("sidecar missing error: %s", sidecar)
	}
	partial, _ := os.ReadFile(filepath.Join(res.Dir, "broken.txt"))
	if strings.Contains(string(partial), "leakme") {
		t.Fatalf("partial output not redacted: %s", partial)
	}

	// FileResults reflect the error.
	var sawErr bool
	for _, f := range res.Files {
		if f.Name == "broken.txt" && f.Err != "" {
			sawErr = true
		}
	}
	if !sawErr {
		t.Fatal("broken source error not recorded in result")
	}
}

func TestArchiveRoundTrips(t *testing.T) {
	parent := t.TempDir()
	b := NewBuilder().WithClock(fixedClock()).
		Add("a.txt", func(context.Context) ([]byte, error) { return []byte("alpha"), nil }).
		Add("sub/b.txt", func(context.Context) ([]byte, error) { return []byte("beta"), nil })
	res, err := b.Build(context.Background(), parent, "mac-a")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	dest := filepath.Join(parent, "bundle.tar.gz")
	if err := Archive(res.Dir, dest); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	names := map[string]string{}
	f, _ := os.Open(dest)
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar read: %v", err)
		}
		if hdr.Typeflag == tar.TypeReg {
			data, _ := io.ReadAll(tr)
			names[hdr.Name] = string(data)
		}
	}

	base := "macvz-bundle-mac-a-20260619T233000Z"
	if names[base+"/a.txt"] != "alpha" {
		t.Fatalf("a.txt missing/wrong in archive: %v", names)
	}
	if names[base+"/sub/b.txt"] != "beta" {
		t.Fatalf("nested file missing in archive: %v", names)
	}
	if _, ok := names[base+"/manifest.txt"]; !ok {
		t.Fatalf("manifest not archived: %v", names)
	}
}
