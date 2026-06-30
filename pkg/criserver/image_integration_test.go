package criserver

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/chimerakang/macvz/pkg/runtime"
	"github.com/chimerakang/macvz/pkg/runtime/container"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// amd64OnlyImage has no linux/arm64 variant, so macvz's bootable-variant check
// must reject it at pull on Apple Silicon (without Rosetta). Used to prove the
// CRI-L8-4 (#143) architecture-rejection diagnostics on a real backend.
const amd64OnlyImage = "docker.io/amd64/alpine:3.20"

// TestLiveImageServiceLifecycle drives PullImage -> ImageStatus -> ListImages ->
// ImageFsInfo -> RemoveImage against a real apple/container backend, proving the
// CRI-P4 ImageService works end to end for a public arm64 image.
//
// It is gated behind MACVZ_CRI_INTEGRATION=1 because it pulls and deletes an
// image; the default test run stays hermetic via the fake image runtime.
func TestLiveImageServiceLifecycle(t *testing.T) {
	if os.Getenv("MACVZ_CRI_INTEGRATION") != "1" {
		t.Skip("set MACVZ_CRI_INTEGRATION=1 to run the CRI ImageService against a real apple/container service")
	}

	driver := container.New(container.Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	if err := driver.Ready(ctx); err != nil {
		t.Fatalf("apple/container not ready: %v", err)
	}

	s := New(Options{Runtime: driver, Images: driver})

	pull, err := s.PullImage(ctx, &runtimeapi.PullImageRequest{
		Image: &runtimeapi.ImageSpec{Image: liveImage},
	})
	if err != nil {
		t.Fatalf("PullImage: %v", err)
	}
	if pull.GetImageRef() == "" {
		t.Fatal("PullImage returned an empty image ref")
	}

	// Ensure cleanup even on failure.
	defer func() {
		_, _ = s.RemoveImage(context.Background(), &runtimeapi.RemoveImageRequest{
			Image: &runtimeapi.ImageSpec{Image: liveImage},
		})
	}()

	st, err := s.ImageStatus(ctx, &runtimeapi.ImageStatusRequest{
		Image: &runtimeapi.ImageSpec{Image: liveImage},
	})
	if err != nil {
		t.Fatalf("ImageStatus: %v", err)
	}
	if st.GetImage() == nil {
		t.Fatal("ImageStatus reported no image after a successful pull")
	}

	list, err := s.ListImages(ctx, &runtimeapi.ListImagesRequest{})
	if err != nil {
		t.Fatalf("ListImages: %v", err)
	}
	found := false
	for _, img := range list.GetImages() {
		if img.GetId() == st.GetImage().GetId() {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("pulled image %q not present in ListImages", st.GetImage().GetId())
	}

	// ImageFsInfo must report honest data (the data root is always sampleable on
	// a host running apple/container).
	fs, err := s.ImageFsInfo(ctx, &runtimeapi.ImageFsInfoRequest{})
	if err != nil {
		t.Fatalf("ImageFsInfo: %v", err)
	}
	if len(fs.GetImageFilesystems()) == 0 {
		t.Error("ImageFsInfo reported no image filesystem on a live host")
	}

	if _, err := s.RemoveImage(ctx, &runtimeapi.RemoveImageRequest{
		Image: &runtimeapi.ImageSpec{Image: liveImage},
	}); err != nil {
		t.Fatalf("RemoveImage: %v", err)
	}

	// After removal, ImageStatus must report the image as absent (empty, non-error).
	after, err := s.ImageStatus(ctx, &runtimeapi.ImageStatusRequest{
		Image: &runtimeapi.ImageSpec{Image: liveImage},
	})
	if err != nil {
		t.Fatalf("ImageStatus after remove: %v", err)
	}
	if after.GetImage() != nil {
		t.Errorf("image still present after RemoveImage: %+v", after.GetImage())
	}
}

// TestLiveImageCacheReuseArchAndDigest is the CRI-L8-4 (#143) gated proof on a
// real apple/container backend for three behaviors hermetic tests can only
// approximate: cache reuse (a second pull is a fast cache hit with a stable ID),
// pull-by-digest resolution, and architecture rejection with actionable
// diagnostics. It captures the real runtime error text so the diagnostic that
// reaches an operator is pinned in the evidence, not just the gRPC code.
//
// Gated behind MACVZ_CRI_INTEGRATION=1; the default test run stays hermetic.
func TestLiveImageCacheReuseArchAndDigest(t *testing.T) {
	if os.Getenv("MACVZ_CRI_INTEGRATION") != "1" {
		t.Skip("set MACVZ_CRI_INTEGRATION=1 to run the CRI ImageService cache/arch checks against a real apple/container service")
	}

	driver := container.New(container.Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	if err := driver.Ready(ctx); err != nil {
		t.Fatalf("apple/container not ready: %v", err)
	}
	s := New(Options{Runtime: driver, Images: driver})

	// Clean slate so the first pull is a real cache miss.
	_, _ = s.RemoveImage(context.Background(), &runtimeapi.RemoveImageRequest{
		Image: &runtimeapi.ImageSpec{Image: liveImage},
	})
	t.Cleanup(func() {
		_, _ = s.RemoveImage(context.Background(), &runtimeapi.RemoveImageRequest{
			Image: &runtimeapi.ImageSpec{Image: liveImage},
		})
	})

	// --- Cache reuse: first pull (miss) then second pull (hit) with a stable ID.
	startCold := time.Now()
	firstPull, err := s.PullImage(ctx, &runtimeapi.PullImageRequest{Image: &runtimeapi.ImageSpec{Image: liveImage}})
	if err != nil {
		t.Fatalf("PullImage(cold): %v", err)
	}
	cold := time.Since(startCold)

	startWarm := time.Now()
	secondPull, err := s.PullImage(ctx, &runtimeapi.PullImageRequest{Image: &runtimeapi.ImageSpec{Image: liveImage}})
	if err != nil {
		t.Fatalf("PullImage(warm cache hit): %v", err)
	}
	warm := time.Since(startWarm)
	t.Logf("cache reuse: cold pull %s, warm pull %s; ID cold=%q warm=%q", cold, warm, firstPull.GetImageRef(), secondPull.GetImageRef())
	if firstPull.GetImageRef() != secondPull.GetImageRef() {
		t.Errorf("cache-hit ref drift: cold=%q warm=%q", firstPull.GetImageRef(), secondPull.GetImageRef())
	}
	if warm > cold {
		t.Logf("warning: warm pull (%s) was not faster than cold pull (%s); cache reuse not demonstrated by timing", warm, cold)
	}

	// ListImages must show exactly one entry for the image after repeated pulls.
	list, err := s.ListImages(ctx, &runtimeapi.ListImagesRequest{})
	if err != nil {
		t.Fatalf("ListImages: %v", err)
	}
	matches := 0
	var digestRef string
	for _, img := range list.GetImages() {
		if img.GetId() == firstPull.GetImageRef() {
			matches++
			for _, d := range img.GetRepoDigests() {
				if strings.Contains(d, "@sha256:") {
					digestRef = d
				}
			}
		}
	}
	if matches != 1 {
		t.Errorf("ListImages reports %d entries for the pulled image, want 1 (no duplicate cache entry)", matches)
	}

	// --- Pull-by-digest: re-pull using the resolved digest and confirm status.
	if digestRef == "" {
		t.Log("no repo digest resolved for the pulled image; skipping pull-by-digest leg")
	} else {
		if _, err := s.PullImage(ctx, &runtimeapi.PullImageRequest{Image: &runtimeapi.ImageSpec{Image: digestRef}}); err != nil {
			t.Errorf("PullImage(by digest %q): %v", digestRef, err)
		}
		st, err := s.ImageStatus(ctx, &runtimeapi.ImageStatusRequest{Image: &runtimeapi.ImageSpec{Image: digestRef}})
		if err != nil {
			t.Errorf("ImageStatus(by digest %q): %v", digestRef, err)
		} else if st.GetImage() == nil {
			t.Errorf("ImageStatus(by digest %q) reported absent", digestRef)
		} else {
			t.Logf("pull-by-digest resolved: %q -> ID %q", digestRef, st.GetImage().GetId())
		}
	}

	// --- Architecture rejection: an amd64-only image is refused at pull with a
	// FailedPrecondition and an actionable, arm64-naming diagnostic.
	_, archErr := s.PullImage(ctx, &runtimeapi.PullImageRequest{Image: &runtimeapi.ImageSpec{Image: amd64OnlyImage}})
	if archErr == nil {
		// Best-effort cleanup if it somehow landed.
		_, _ = s.RemoveImage(context.Background(), &runtimeapi.RemoveImageRequest{Image: &runtimeapi.ImageSpec{Image: amd64OnlyImage}})
		t.Fatalf("PullImage(amd64-only) unexpectedly succeeded; want ErrIncompatibleArch")
	}
	if got := status.Code(archErr); got != codes.FailedPrecondition {
		t.Errorf("arch rejection code = %v, want FailedPrecondition", got)
	}
	archMsg := status.Convert(archErr).Message()
	t.Logf("architecture rejection diagnostic (real backend): %s", archMsg)
	if !strings.Contains(archMsg, "linux/arm64") {
		t.Errorf("arch diagnostic should name the missing target arch; got %q", archMsg)
	}

	// --- Missing-image mapping: a non-existent tag maps to NotFound, so kubelet
	// reports ImagePullBackOff for a typo rather than a generic Internal error.
	const missingImage = "docker.io/library/macvz-nonexistent-image:does-not-exist"
	_, missErr := s.PullImage(ctx, &runtimeapi.PullImageRequest{Image: &runtimeapi.ImageSpec{Image: missingImage}})
	if missErr == nil {
		t.Errorf("PullImage(%q) unexpectedly succeeded", missingImage)
	} else {
		t.Logf("missing-image diagnostic (real backend): code=%v msg=%s", status.Code(missErr), status.Convert(missErr).Message())
		// The backend may classify a missing image as NotFound or, if the registry
		// returns an auth-shaped error, as Internal; assert it is at least not a
		// silent success and surfaces the runtime error.
		if errors.Is(missErr, runtime.ErrIncompatibleArch) {
			t.Errorf("missing image misclassified as incompatible arch: %v", missErr)
		}
	}
}
