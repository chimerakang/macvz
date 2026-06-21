package container

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/chimerakang/macvz/pkg/runtime"
)

// realImageLsJSON is a sample `container image ls --format json` payload with
// numeric byte sizes, matching the shape apple/container emits.
const realImageLsJSON = `[
  {"reference":"docker.io/library/nginx:1.27-alpine","name":"nginx","size":12000000},
  {"reference":"ghcr.io/headlamp-k8s/headlamp:v0.30.0","name":"headlamp","size":80000000}
]`

func TestImageCacheUsageSumsSizesAndCounts(t *testing.T) {
	f := &fakeRunner{outputs: map[string][]byte{"image ls": []byte(realImageLsJSON)}}
	u, err := driverWith(f).ImageCacheUsage(context.Background())
	if err != nil {
		t.Fatalf("ImageCacheUsage: %v", err)
	}
	if u.Count != 2 {
		t.Errorf("Count = %d, want 2", u.Count)
	}
	if u.TotalBytes != 92_000_000 {
		t.Errorf("TotalBytes = %d, want 92000000", u.TotalBytes)
	}
	if u.Timestamp.IsZero() {
		t.Error("Timestamp not set")
	}
}

func TestImageCacheUsageBuildsArgs(t *testing.T) {
	f := &fakeRunner{outputs: map[string][]byte{"image ls": []byte("[]")}}
	if _, err := driverWith(f).ImageCacheUsage(context.Background()); err != nil {
		t.Fatalf("ImageCacheUsage: %v", err)
	}
	if args := lastCall(f); !argsContain(args, "image", "ls", "--format", "json") {
		t.Errorf("image ls args = %v, want image ls --format json", args)
	}
}

func TestImageCacheUsageEmptyStore(t *testing.T) {
	for name, out := range map[string]string{"empty array": "[]", "no output": ""} {
		t.Run(name, func(t *testing.T) {
			f := &fakeRunner{outputs: map[string][]byte{"image ls": []byte(out)}}
			u, err := driverWith(f).ImageCacheUsage(context.Background())
			if err != nil {
				t.Fatalf("ImageCacheUsage: %v", err)
			}
			if u.Count != 0 || u.TotalBytes != 0 {
				t.Errorf("empty store = count %d bytes %d, want 0/0", u.Count, u.TotalBytes)
			}
		})
	}
}

func TestImageCacheUsageToleratesStringAndVariantSizes(t *testing.T) {
	// One image reports its size as a quoted string; another nests it under a
	// platform variant; a third has no parseable size (counts, contributes 0).
	const mixed = `[
	  {"name":"a","size":"100"},
	  {"name":"b","variants":[{"size":40},{"size":60}]},
	  {"name":"c","size":"not-a-number"}
	]`
	f := &fakeRunner{outputs: map[string][]byte{"image ls": []byte(mixed)}}
	u, err := driverWith(f).ImageCacheUsage(context.Background())
	if err != nil {
		t.Fatalf("ImageCacheUsage: %v", err)
	}
	if u.Count != 3 {
		t.Errorf("Count = %d, want 3", u.Count)
	}
	if u.TotalBytes != 200 {
		t.Errorf("TotalBytes = %d, want 200 (100 + 40+60 + 0)", u.TotalBytes)
	}
}

func TestImageCacheUsageAcceptsCommonJSONShapes(t *testing.T) {
	cases := map[string]string{
		"array":  `[{"name":"a","size":10},{"name":"b","size":20}]`,
		"object": `{"name":"a","size":30}`,
		"ndjson": "{\"name\":\"a\",\"size\":40}\n{\"name\":\"b\",\"size\":60}\n",
	}
	want := map[string]struct {
		count int
		bytes uint64
	}{
		"array":  {2, 30},
		"object": {1, 30},
		"ndjson": {2, 100},
	}
	for name, out := range cases {
		t.Run(name, func(t *testing.T) {
			f := &fakeRunner{outputs: map[string][]byte{"image ls": []byte(out)}}
			u, err := driverWith(f).ImageCacheUsage(context.Background())
			if err != nil {
				t.Fatalf("ImageCacheUsage: %v", err)
			}
			if u.Count != want[name].count || u.TotalBytes != want[name].bytes {
				t.Errorf("usage = count %d bytes %d, want %d/%d", u.Count, u.TotalBytes, want[name].count, want[name].bytes)
			}
		})
	}
}

func TestImageCacheUsageMapsRunErrors(t *testing.T) {
	f := &fakeRunner{errs: map[string]error{"image ls": &CommandError{Stderr: "boom", ExitCode: 1}}}
	if _, err := driverWith(f).ImageCacheUsage(context.Background()); err == nil {
		t.Fatal("expected an error when the image ls command fails")
	}
}

func TestImageCacheUsageRejectsMalformedJSON(t *testing.T) {
	f := &fakeRunner{outputs: map[string][]byte{"image ls": []byte("{not json")}}
	if _, err := driverWith(f).ImageCacheUsage(context.Background()); err == nil {
		t.Fatal("expected a parse error for malformed image list JSON")
	}
}

func TestNodeFilesystemUsesInjectedStatfs(t *testing.T) {
	want := runtime.FilesystemUsage{
		Path:           "/home/test",
		Timestamp:      time.Unix(1700000000, 0),
		TotalBytes:     200 << 30,
		UsedBytes:      50 << 30,
		AvailableBytes: 140 << 30,
		TotalInodes:    2000,
		UsedInodes:     500,
	}
	var sampled string
	d := &Driver{
		dataRoot: "/home/test",
		statfs: func(path string) (runtime.FilesystemUsage, error) {
			sampled = path
			return want, nil
		},
	}

	got, err := d.NodeFilesystem(context.Background())
	if err != nil {
		t.Fatalf("NodeFilesystem: %v", err)
	}
	if sampled != "/home/test" {
		t.Errorf("statfs sampled %q, want the configured dataRoot /home/test", sampled)
	}
	if got != want {
		t.Errorf("NodeFilesystem = %+v, want %+v", got, want)
	}
}

func TestNodeFilesystemWrapsStatfsError(t *testing.T) {
	d := &Driver{
		dataRoot: "/nope",
		statfs: func(string) (runtime.FilesystemUsage, error) {
			return runtime.FilesystemUsage{}, errors.New("statfs failed")
		},
	}
	if _, err := d.NodeFilesystem(context.Background()); err == nil {
		t.Fatal("expected NodeFilesystem to surface the statfs error")
	}
}

// Driver must satisfy the optional DiskReporter capability.
var _ runtime.DiskReporter = (*Driver)(nil)
