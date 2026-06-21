package container

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/chimerakang/macvz/pkg/runtime"
)

// NodeFilesystem reports usage of the filesystem backing micro-VM and image
// storage. It samples the driver's data root (Config.DataRoot, defaulting to
// the user's home directory — the volume apple/container stores its data on),
// so the figure tracks the disk that actually fills up as Pods and images
// accumulate.
func (d *Driver) NodeFilesystem(ctx context.Context) (runtime.FilesystemUsage, error) {
	_ = ctx // statfs is a local syscall; ctx is accepted for interface symmetry.
	fn := d.statfs
	if fn == nil {
		fn = statfsUsage
	}
	fu, err := fn(d.dataRootPath())
	if err != nil {
		return runtime.FilesystemUsage{}, fmt.Errorf("node filesystem: %w", err)
	}
	return fu, nil
}

// dataRootPath resolves the path whose filesystem is sampled for node disk
// accounting: the configured DataRoot, or the user's home directory, or "/" as
// a last resort. Any path on the target volume yields the same statfs figures.
func (d *Driver) dataRootPath() string {
	if d.dataRoot != "" {
		return d.dataRoot
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return home
	}
	return "/"
}

// ImageCacheUsage reports the size and count of locally cached images by
// summing `container image ls --format json`. A missing/unparsable size field
// contributes 0 bytes (count still increments), so the count stays accurate
// even if a runtime build omits sizes.
func (d *Driver) ImageCacheUsage(ctx context.Context) (runtime.ImageCacheUsage, error) {
	out, err := d.run.output(ctx, "image", "ls", "--format", "json")
	if err != nil {
		return runtime.ImageCacheUsage{}, fmt.Errorf("image cache usage: %w", mapErr(err))
	}
	return parseImageCache(out, time.Now())
}

// imageListEntry is the subset of `container image ls/inspect --format json` the
// driver consumes. apple/container reports each cached image's total size in
// bytes; some builds nest the size under a platform variant, so both are summed.
// The digest fields are optional (best-effort across CLI builds) and only used
// to derive a stable image ID for the CRI ImageService (#76); their absence
// degrades the ID to the reference rather than failing.
type imageListEntry struct {
	Reference  string `json:"reference"`
	Name       string `json:"name"`
	Digest     string `json:"digest"`
	Descriptor struct {
		Digest string `json:"digest"`
	} `json:"descriptor"`
	Size     byteSize   `json:"size"`
	Variants []struct { // present on multi-platform builds
		Size       byteSize `json:"size"`
		Descriptor struct {
			Digest string `json:"digest"`
		} `json:"descriptor"`
	} `json:"variants"`
}

func parseImageCache(out []byte, now time.Time) (runtime.ImageCacheUsage, error) {
	trimmed := strings.TrimSpace(string(out))
	// An empty store may print nothing or an empty array; both are valid and
	// mean zero cached images, not an error.
	if trimmed == "" {
		return runtime.ImageCacheUsage{Timestamp: now}, nil
	}
	entries, err := parseImageEntries([]byte(trimmed))
	if err != nil {
		return runtime.ImageCacheUsage{}, fmt.Errorf("image cache usage: parse image list: %w", err)
	}

	usage := runtime.ImageCacheUsage{Timestamp: now, Count: len(entries)}
	for _, e := range entries {
		size := uint64(e.Size)
		if size == 0 {
			for _, v := range e.Variants {
				size += uint64(v.Size)
			}
		}
		usage.TotalBytes += size
	}
	return usage, nil
}

// parseImageEntries accepts the shapes seen across CLI-style JSON formatters:
// a JSON array, a single JSON object, or newline-delimited JSON objects.
func parseImageEntries(out []byte) ([]imageListEntry, error) {
	var entries []imageListEntry
	if err := json.Unmarshal(out, &entries); err == nil {
		return entries, nil
	}

	var single imageListEntry
	if err := json.Unmarshal(out, &single); err == nil {
		return []imageListEntry{single}, nil
	}

	var lines []imageListEntry
	for _, raw := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		var e imageListEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			return nil, err
		}
		lines = append(lines, e)
	}
	return lines, nil
}

// byteSize decodes an image size that the runtime may emit as a JSON number or
// as a quoted decimal string, tolerating either without forcing the caller to
// know which a given runtime build uses. Non-numeric or absent values decode to
// 0 so one odd entry never fails the whole scrape.
type byteSize uint64

func (b *byteSize) UnmarshalJSON(p []byte) error {
	s := strings.TrimSpace(string(p))
	s = strings.Trim(s, `"`)
	if s == "" || s == "null" {
		*b = 0
		return nil
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		// A human-formatted or unexpected size is treated as unknown (0), not a
		// hard parse failure, keeping the image count usable.
		*b = 0
		return nil
	}
	*b = byteSize(n)
	return nil
}
