package metrics

import (
	"context"
	"testing"
	"time"

	"github.com/chimerakang/macvz/pkg/runtime"
	dto "github.com/prometheus/client_model/go"
)

// diskFn returns a DiskFunc yielding the given sample, for collector tests.
func diskFn(sample DiskSample) DiskFunc {
	return func(context.Context) DiskSample { return sample }
}

func fullDisk() DiskSample {
	return DiskSample{
		NodeFS: runtime.FilesystemUsage{
			Path:           "/data",
			Timestamp:      time.Unix(1700000000, 0),
			TotalBytes:     100 << 30,
			UsedBytes:      40 << 30,
			AvailableBytes: 55 << 30, // < total-used: a reserve is held back
			TotalInodes:    1000,
			UsedInodes:     250,
		},
		NodeFSOK: true,
		Images:   runtime.ImageCacheUsage{TotalBytes: 8 << 30, Count: 12},
		ImagesOK: true,
	}
}

func TestSummaryReportsFilesystemAndImageFs(t *testing.T) {
	c := NewCollector("mac-1", fakeMem{total: 8 << 30, used: 1 << 30})
	s := c.Summary(context.Background(), onePod(), statsMap(nil), diskFn(fullDisk()))

	fs := s.Node.Fs
	if fs == nil {
		t.Fatal("node Fs stats missing")
	}
	if *fs.CapacityBytes != 100<<30 || *fs.UsedBytes != 40<<30 || *fs.AvailableBytes != 55<<30 {
		t.Errorf("node fs = cap %d used %d avail %d", *fs.CapacityBytes, *fs.UsedBytes, *fs.AvailableBytes)
	}
	if fs.Inodes == nil || *fs.Inodes != 1000 || *fs.InodesUsed != 250 || *fs.InodesFree != 750 {
		t.Errorf("node fs inodes = %v/%v/%v, want 1000/250/750", fs.Inodes, fs.InodesUsed, fs.InodesFree)
	}

	// The image cache is reported as imageFs used bytes on the same filesystem.
	if s.Node.Runtime == nil || s.Node.Runtime.ImageFs == nil {
		t.Fatal("node runtime imageFs missing")
	}
	imgfs := s.Node.Runtime.ImageFs
	if *imgfs.UsedBytes != 8<<30 {
		t.Errorf("imageFs used = %d, want %d", *imgfs.UsedBytes, uint64(8<<30))
	}
	if *imgfs.CapacityBytes != 100<<30 {
		t.Errorf("imageFs capacity = %d, want node fs total", *imgfs.CapacityBytes)
	}
}

func TestSummaryOmitsDiskWhenNoDiskFunc(t *testing.T) {
	c := NewCollector("mac-1", fakeMem{total: 8 << 30, used: 1 << 30})
	s := c.Summary(context.Background(), onePod(), statsMap(nil), nil)
	if s.Node.Fs != nil || s.Node.Runtime != nil {
		t.Errorf("disk stats should be omitted without a DiskFunc: fs=%v runtime=%v", s.Node.Fs, s.Node.Runtime)
	}
}

func TestSummaryReportsFilesystemWithoutImagesWhenImagesUnavailable(t *testing.T) {
	c := NewCollector("mac-1", fakeMem{total: 8 << 30, used: 1 << 30})
	sample := fullDisk()
	sample.ImagesOK = false
	s := c.Summary(context.Background(), onePod(), statsMap(nil), diskFn(sample))

	if s.Node.Fs == nil {
		t.Fatal("node Fs should still be reported when only images are unavailable")
	}
	if s.Node.Runtime != nil {
		t.Errorf("imageFs should be omitted when image usage is unavailable, got %v", s.Node.Runtime)
	}
}

func TestSummaryReportsImagesWithoutFilesystemWhenFilesystemUnavailable(t *testing.T) {
	c := NewCollector("mac-1", fakeMem{total: 8 << 30, used: 1 << 30})
	sample := DiskSample{
		Images:   runtime.ImageCacheUsage{Timestamp: time.Unix(1700000100, 0), TotalBytes: 3 << 30, Count: 4},
		ImagesOK: true,
	}
	s := c.Summary(context.Background(), onePod(), statsMap(nil), diskFn(sample))

	if s.Node.Fs != nil {
		t.Errorf("node Fs should be omitted when filesystem usage is unavailable, got %v", s.Node.Fs)
	}
	if s.Node.Runtime == nil || s.Node.Runtime.ImageFs == nil {
		t.Fatal("imageFs should still be reported when image usage is available")
	}
	imgfs := s.Node.Runtime.ImageFs
	if imgfs.CapacityBytes != nil || imgfs.AvailableBytes != nil {
		t.Errorf("image-only sample should not invent filesystem capacity/availability: %+v", imgfs)
	}
	if imgfs.UsedBytes == nil || *imgfs.UsedBytes != 3<<30 {
		t.Fatalf("imageFs used = %v, want %d", imgfs.UsedBytes, uint64(3<<30))
	}
}

func TestResourceMetricsEmitsDiskAndImageGauges(t *testing.T) {
	c := NewCollector("mac-1", fakeMem{total: 8 << 30, used: 2 << 30})
	families := c.ResourceMetrics(context.Background(), onePod(), statsMap(nil), diskFn(fullDisk()), true)

	byName := map[string]*dto.MetricFamily{}
	for _, f := range families {
		byName[f.GetName()] = f
	}

	checks := map[string]float64{
		"macvz_node_filesystem_capacity_bytes":  float64(100 << 30),
		"macvz_node_filesystem_used_bytes":      float64(40 << 30),
		"macvz_node_filesystem_available_bytes": float64(55 << 30),
		"macvz_image_cache_bytes":               float64(8 << 30),
		"macvz_image_cache_images":              12,
	}
	for name, want := range checks {
		f := byName[name]
		if f == nil {
			t.Errorf("missing metric family %q", name)
			continue
		}
		if got := f.Metric[0].Gauge.GetValue(); got != want {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
}

func TestResourceMetricsOmitsDiskWhenUnavailable(t *testing.T) {
	c := NewCollector("mac-1", fakeMem{total: 8 << 30, used: 2 << 30})
	families := c.ResourceMetrics(context.Background(), onePod(), statsMap(nil), nil, true)
	for _, f := range families {
		switch f.GetName() {
		case "macvz_node_filesystem_capacity_bytes", "macvz_image_cache_bytes":
			t.Errorf("disk family %q should be absent without a DiskFunc", f.GetName())
		}
	}
}
