package criserver

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/chimerakang/macvz/pkg/runtime/container"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

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
