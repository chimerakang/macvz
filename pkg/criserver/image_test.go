package criserver

import (
	"context"
	"encoding/base64"
	"errors"
	"sync"
	"testing"

	"github.com/chimerakang/macvz/pkg/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// fakeImageRuntime is an ImageRuntime (optionally an imageFsReporter) that records
// calls and allows per-method error injection, keeping the ImageService tests
// hermetic.
type fakeImageRuntime struct {
	mu      sync.Mutex
	images  map[string]runtime.ImageInfo // keyed by reference
	pulled  []string
	pullarg []*runtime.RegistryAuth
	removed []string

	pullErr, statusErr, listErr, removeErr error

	// fs reporting (optional capability)
	cache     runtime.ImageCacheUsage
	fsUsage   runtime.FilesystemUsage
	cacheErr  error
	nodeFsErr error
}

func newFakeImageRuntime() *fakeImageRuntime {
	return &fakeImageRuntime{images: map[string]runtime.ImageInfo{}}
}

func (f *fakeImageRuntime) Pull(_ context.Context, image string, auth *runtime.RegistryAuth) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pullErr != nil {
		return f.pullErr
	}
	f.pulled = append(f.pulled, image)
	f.pullarg = append(f.pullarg, auth)
	if _, ok := f.images[image]; !ok {
		f.images[image] = runtime.ImageInfo{ID: "sha256:" + image, RepoTags: []string{image}}
	}
	return nil
}

func (f *fakeImageRuntime) ImageStatus(_ context.Context, image string) (runtime.ImageInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.statusErr != nil {
		return runtime.ImageInfo{}, f.statusErr
	}
	for key, info := range f.images {
		if image == key || image == info.ID || containsString(info.RepoTags, image) || containsString(info.RepoDigests, image) {
			return info, nil
		}
	}
	return runtime.ImageInfo{}, runtime.ErrNotFound
}

func (f *fakeImageRuntime) ListImages(_ context.Context) ([]runtime.ImageInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]runtime.ImageInfo, 0, len(f.images))
	for _, v := range f.images {
		out = append(out, v)
	}
	return out, nil
}

func (f *fakeImageRuntime) RemoveImage(_ context.Context, image string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.removeErr != nil {
		return f.removeErr
	}
	f.removed = append(f.removed, image)
	for key, info := range f.images {
		if image == key || image == info.ID || containsString(info.RepoTags, image) || containsString(info.RepoDigests, image) {
			delete(f.images, key)
			return nil
		}
	}
	return nil
}

func containsString(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

// fakeImageFsRuntime adds the optional imageFsReporter capability.
type fakeImageFsRuntime struct {
	*fakeImageRuntime
}

func (f *fakeImageFsRuntime) NodeFilesystem(context.Context) (runtime.FilesystemUsage, error) {
	if f.nodeFsErr != nil {
		return runtime.FilesystemUsage{}, f.nodeFsErr
	}
	return f.fsUsage, nil
}

func (f *fakeImageFsRuntime) ImageCacheUsage(context.Context) (runtime.ImageCacheUsage, error) {
	if f.cacheErr != nil {
		return runtime.ImageCacheUsage{}, f.cacheErr
	}
	return f.cache, nil
}

func imageServer(img ImageRuntime) *Server {
	return New(Options{Images: img})
}

func TestPullImageReturnsResolvedRef(t *testing.T) {
	img := newFakeImageRuntime()
	img.images["alpine:3.20"] = runtime.ImageInfo{
		ID:          "alpine@sha256:deadbeef",
		RepoTags:    []string{"alpine:3.20"},
		RepoDigests: []string{"alpine@sha256:deadbeef", "sha256:deadbeef"},
	}
	s := imageServer(img)

	resp, err := s.PullImage(context.Background(), &runtimeapi.PullImageRequest{
		Image: &runtimeapi.ImageSpec{Image: "alpine:3.20"},
	})
	if err != nil {
		t.Fatalf("PullImage: %v", err)
	}
	if resp.GetImageRef() != "alpine@sha256:deadbeef" {
		t.Errorf("ImageRef = %q, want the runtime-usable image ID", resp.GetImageRef())
	}
	if len(img.pulled) != 1 || img.pulled[0] != "alpine:3.20" {
		t.Errorf("pulled = %v", img.pulled)
	}
	if img.pullarg[0] != nil {
		t.Errorf("expected anonymous pull, got auth %+v", img.pullarg[0])
	}
}

func TestPullImageWithUsernamePassword(t *testing.T) {
	img := newFakeImageRuntime()
	s := imageServer(img)
	_, err := s.PullImage(context.Background(), &runtimeapi.PullImageRequest{
		Image: &runtimeapi.ImageSpec{Image: "registry.example.com/team/app:v1"},
		Auth:  &runtimeapi.AuthConfig{Username: "alice", Password: "s3cret"},
	})
	if err != nil {
		t.Fatalf("PullImage: %v", err)
	}
	got := img.pullarg[0]
	if got == nil || got.Username != "alice" || got.Password != "s3cret" {
		t.Fatalf("auth = %+v, want alice/s3cret", got)
	}
	if got.Server != "registry.example.com" {
		t.Errorf("Server = %q, want host derived from the image ref", got.Server)
	}
}

func TestPullImageRejectsTokenAuth(t *testing.T) {
	s := imageServer(newFakeImageRuntime())
	_, err := s.PullImage(context.Background(), &runtimeapi.PullImageRequest{
		Image: &runtimeapi.ImageSpec{Image: "alpine"},
		Auth:  &runtimeapi.AuthConfig{IdentityToken: "tok"},
	})
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("err code = %v, want Unimplemented for token auth", status.Code(err))
	}
}

func TestPullImageNotConfigured(t *testing.T) {
	s := New(Options{}) // no image runtime
	_, err := s.PullImage(context.Background(), &runtimeapi.PullImageRequest{
		Image: &runtimeapi.ImageSpec{Image: "alpine"},
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("err code = %v, want FailedPrecondition", status.Code(err))
	}
}

func TestImageStatusPresentAndAbsent(t *testing.T) {
	img := newFakeImageRuntime()
	img.images["alpine:3.20"] = runtime.ImageInfo{ID: "alpine@sha256:a", RepoTags: []string{"alpine:3.20"}, RepoDigests: []string{"alpine@sha256:a", "sha256:a"}, Size: 42}
	s := imageServer(img)

	resp, err := s.ImageStatus(context.Background(), &runtimeapi.ImageStatusRequest{
		Image: &runtimeapi.ImageSpec{Image: "alpine:3.20"},
	})
	if err != nil {
		t.Fatalf("ImageStatus: %v", err)
	}
	if resp.GetImage().GetId() != "alpine@sha256:a" || resp.GetImage().GetSize() != 42 {
		t.Errorf("image = %+v", resp.GetImage())
	}

	byID, err := s.ImageStatus(context.Background(), &runtimeapi.ImageStatusRequest{
		Image: &runtimeapi.ImageSpec{Image: "alpine@sha256:a"},
	})
	if err != nil {
		t.Fatalf("ImageStatus(by ID): %v", err)
	}
	if byID.GetImage().GetId() != "alpine@sha256:a" {
		t.Errorf("ImageStatus(by ID) = %+v", byID.GetImage())
	}

	// Absent image: empty (non-error) response per the CRI contract.
	absent, err := s.ImageStatus(context.Background(), &runtimeapi.ImageStatusRequest{
		Image: &runtimeapi.ImageSpec{Image: "ghost:latest"},
	})
	if err != nil {
		t.Fatalf("ImageStatus(absent): %v", err)
	}
	if absent.GetImage() != nil {
		t.Errorf("absent image = %+v, want nil", absent.GetImage())
	}
}

func TestImageStatusRuntimeErrorIsNotEmpty(t *testing.T) {
	img := newFakeImageRuntime()
	img.statusErr = errors.New("backend down")
	s := imageServer(img)
	_, err := s.ImageStatus(context.Background(), &runtimeapi.ImageStatusRequest{
		Image: &runtimeapi.ImageSpec{Image: "alpine"},
	})
	if status.Code(err) != codes.Internal {
		t.Fatalf("err code = %v, want Internal for a backend failure", status.Code(err))
	}
}

func TestListImagesWithFilter(t *testing.T) {
	img := newFakeImageRuntime()
	img.images["alpine:3.20"] = runtime.ImageInfo{ID: "alpine@sha256:a", RepoTags: []string{"alpine:3.20"}, RepoDigests: []string{"alpine@sha256:a", "sha256:a"}}
	img.images["busybox:1.36"] = runtime.ImageInfo{ID: "busybox@sha256:b", RepoTags: []string{"busybox:1.36"}, RepoDigests: []string{"busybox@sha256:b", "sha256:b"}}
	s := imageServer(img)

	all, err := s.ListImages(context.Background(), &runtimeapi.ListImagesRequest{})
	if err != nil {
		t.Fatalf("ListImages: %v", err)
	}
	if len(all.GetImages()) != 2 {
		t.Fatalf("got %d, want 2", len(all.GetImages()))
	}

	filtered, err := s.ListImages(context.Background(), &runtimeapi.ListImagesRequest{
		Filter: &runtimeapi.ImageFilter{Image: &runtimeapi.ImageSpec{Image: "busybox:1.36"}},
	})
	if err != nil {
		t.Fatalf("ListImages(filter): %v", err)
	}
	if len(filtered.GetImages()) != 1 || filtered.GetImages()[0].GetId() != "busybox@sha256:b" {
		t.Errorf("filtered = %+v", filtered.GetImages())
	}

	byRawDigest, err := s.ListImages(context.Background(), &runtimeapi.ListImagesRequest{
		Filter: &runtimeapi.ImageFilter{Image: &runtimeapi.ImageSpec{Image: "sha256:b"}},
	})
	if err != nil {
		t.Fatalf("ListImages(raw digest filter): %v", err)
	}
	if len(byRawDigest.GetImages()) != 1 || byRawDigest.GetImages()[0].GetId() != "busybox@sha256:b" {
		t.Errorf("raw digest filtered = %+v", byRawDigest.GetImages())
	}
}

func TestListImagesNotConfiguredIsEmpty(t *testing.T) {
	s := New(Options{})
	resp, err := s.ListImages(context.Background(), &runtimeapi.ListImagesRequest{})
	if err != nil {
		t.Fatalf("ListImages: %v", err)
	}
	if len(resp.GetImages()) != 0 {
		t.Errorf("want empty list with no image runtime, got %+v", resp.GetImages())
	}
}

func TestRemoveImage(t *testing.T) {
	img := newFakeImageRuntime()
	img.images["alpine:3.20"] = runtime.ImageInfo{ID: "alpine@sha256:a", RepoTags: []string{"alpine:3.20"}, RepoDigests: []string{"alpine@sha256:a", "sha256:a"}}
	s := imageServer(img)
	if _, err := s.RemoveImage(context.Background(), &runtimeapi.RemoveImageRequest{
		Image: &runtimeapi.ImageSpec{Image: "alpine:3.20"},
	}); err != nil {
		t.Fatalf("RemoveImage: %v", err)
	}
	if len(img.removed) != 1 || img.removed[0] != "alpine:3.20" {
		t.Errorf("removed = %v", img.removed)
	}
}

func TestRemoveImageByImageRef(t *testing.T) {
	img := newFakeImageRuntime()
	img.images["alpine:3.20"] = runtime.ImageInfo{ID: "alpine@sha256:a", RepoTags: []string{"alpine:3.20"}, RepoDigests: []string{"alpine@sha256:a", "sha256:a"}}
	s := imageServer(img)
	if _, err := s.RemoveImage(context.Background(), &runtimeapi.RemoveImageRequest{
		Image: &runtimeapi.ImageSpec{Image: "alpine@sha256:a"},
	}); err != nil {
		t.Fatalf("RemoveImage(image ref): %v", err)
	}
	if len(img.removed) != 1 || img.removed[0] != "alpine@sha256:a" {
		t.Errorf("removed = %v", img.removed)
	}
	if _, err := img.ImageStatus(context.Background(), "alpine:3.20"); !errors.Is(err, runtime.ErrNotFound) {
		t.Errorf("image still present after remove by image ref: %v", err)
	}
}

func TestImageFsInfoReportsHonestData(t *testing.T) {
	base := newFakeImageRuntime()
	base.cache = runtime.ImageCacheUsage{TotalBytes: 9000}
	base.fsUsage = runtime.FilesystemUsage{Path: "/var/lib/macvz", UsedInodes: 12}
	s := imageServer(&fakeImageFsRuntime{base})

	resp, err := s.ImageFsInfo(context.Background(), &runtimeapi.ImageFsInfoRequest{})
	if err != nil {
		t.Fatalf("ImageFsInfo: %v", err)
	}
	fss := resp.GetImageFilesystems()
	if len(fss) != 1 {
		t.Fatalf("got %d filesystems, want 1", len(fss))
	}
	if fss[0].GetUsedBytes().GetValue() != 9000 {
		t.Errorf("UsedBytes = %d, want image-cache size 9000", fss[0].GetUsedBytes().GetValue())
	}
	if fss[0].GetFsId().GetMountpoint() != "/var/lib/macvz" {
		t.Errorf("Mountpoint = %q", fss[0].GetFsId().GetMountpoint())
	}
	if fss[0].GetInodesUsed().GetValue() != 12 {
		t.Errorf("InodesUsed = %d, want 12", fss[0].GetInodesUsed().GetValue())
	}
}

func TestImageFsInfoDegradesWithoutReporter(t *testing.T) {
	// The plain fake does not implement imageFsReporter.
	s := imageServer(newFakeImageRuntime())
	resp, err := s.ImageFsInfo(context.Background(), &runtimeapi.ImageFsInfoRequest{})
	if err != nil {
		t.Fatalf("ImageFsInfo: %v", err)
	}
	if len(resp.GetImageFilesystems()) != 0 {
		t.Errorf("want no filesystems (no fake values) without a reporter, got %+v", resp.GetImageFilesystems())
	}
}

func TestImageFsInfoDegradesOnCacheError(t *testing.T) {
	base := newFakeImageRuntime()
	base.cacheErr = runtime.ErrDiskUsageUnavailable
	s := imageServer(&fakeImageFsRuntime{base})
	resp, err := s.ImageFsInfo(context.Background(), &runtimeapi.ImageFsInfoRequest{})
	if err != nil {
		t.Fatalf("ImageFsInfo: %v", err)
	}
	if len(resp.GetImageFilesystems()) != 0 {
		t.Errorf("want empty response when the image cache cannot be sampled")
	}
}

func TestToRegistryAuthBase64(t *testing.T) {
	enc := base64.StdEncoding.EncodeToString([]byte("bob:hunter2"))
	auth, err := toRegistryAuth(&runtimeapi.AuthConfig{Auth: enc}, "docker.io/library/nginx")
	if err != nil {
		t.Fatalf("toRegistryAuth: %v", err)
	}
	if auth == nil || auth.Username != "bob" || auth.Password != "hunter2" {
		t.Fatalf("auth = %+v, want bob/hunter2", auth)
	}
	if auth.Server != "docker.io" {
		t.Errorf("Server = %q, want docker.io for a Hub short name", auth.Server)
	}
}

func TestToRegistryAuthAnonymous(t *testing.T) {
	if a, err := toRegistryAuth(nil, "alpine"); err != nil || a != nil {
		t.Fatalf("nil auth -> %+v, %v; want nil,nil", a, err)
	}
	if a, err := toRegistryAuth(&runtimeapi.AuthConfig{}, "alpine"); err != nil || a != nil {
		t.Fatalf("empty auth -> %+v, %v; want nil,nil", a, err)
	}
}

// newServerWithImageRuntime builds a server with both a container runtime and an
// image runtime, plus a ready sandbox, and returns the server, sandbox id, and
// the fakes.
func newServerWithImageRuntime(t *testing.T) (*Server, string, *fakeRuntime, *fakeImageRuntime) {
	t.Helper()
	rt := newFakeRuntime()
	img := newFakeImageRuntime()
	s := New(Options{Runtime: rt, Images: img})
	resp, err := s.RunPodSandbox(context.Background(), &runtimeapi.RunPodSandboxRequest{
		Config: &runtimeapi.PodSandboxConfig{
			Metadata: &runtimeapi.PodSandboxMetadata{Name: "pod", Namespace: "default", Uid: "uid-1"},
		},
	})
	if err != nil {
		t.Fatalf("RunPodSandbox: %v", err)
	}
	return s, resp.GetPodSandboxId(), rt, img
}

// TestCreateContainerDoesNotPullWhenImageServiceWired proves CRI-P4 moved the
// pull off CreateContainer: with an image runtime present, CreateContainer
// verifies the image is local (it does not call Pull) when the image exists.
func TestCreateContainerDoesNotPullWhenImageServiceWired(t *testing.T) {
	s, sandboxID, rt, img := newServerWithImageRuntime(t)
	img.images["docker.io/library/alpine:3.20"] = runtime.ImageInfo{ID: "docker.io/library/alpine@sha256:a", RepoTags: []string{"docker.io/library/alpine:3.20"}, RepoDigests: []string{"docker.io/library/alpine@sha256:a", "sha256:a"}}

	if _, err := s.CreateContainer(context.Background(), createReq(sandboxID, "app")); err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	if len(rt.pulled) != 0 {
		t.Errorf("CreateContainer must not pull when the ImageService is wired; pulls=%v", rt.pulled)
	}
	if len(img.pulled) != 0 {
		t.Errorf("CreateContainer must not call the image runtime's Pull; pulls=%v", img.pulled)
	}
	if len(rt.created) != 1 {
		t.Errorf("expected one workload create, got %d", len(rt.created))
	}
}

func TestCreateContainerAcceptsPullImageRef(t *testing.T) {
	s, sandboxID, rt, img := newServerWithImageRuntime(t)
	img.images["docker.io/library/alpine:3.20"] = runtime.ImageInfo{
		ID:          "docker.io/library/alpine@sha256:a",
		RepoTags:    []string{"docker.io/library/alpine:3.20"},
		RepoDigests: []string{"docker.io/library/alpine@sha256:a", "sha256:a"},
	}
	pull, err := s.PullImage(context.Background(), &runtimeapi.PullImageRequest{
		Image: &runtimeapi.ImageSpec{Image: "docker.io/library/alpine:3.20"},
	})
	if err != nil {
		t.Fatalf("PullImage: %v", err)
	}
	req := createReq(sandboxID, "app")
	req.Config.Image = &runtimeapi.ImageSpec{
		Image:              pull.GetImageRef(),
		UserSpecifiedImage: "docker.io/library/alpine:3.20",
	}
	if _, err := s.CreateContainer(context.Background(), req); err != nil {
		t.Fatalf("CreateContainer with PullImage image_ref %q: %v", pull.GetImageRef(), err)
	}
	if len(rt.created) != 1 || rt.created[0].Image != pull.GetImageRef() {
		t.Fatalf("created = %+v, want image %q", rt.created, pull.GetImageRef())
	}
}

// TestCreateContainerFailsWhenImageAbsent proves CreateContainer fails clearly,
// without pulling, when the image was not pulled via the ImageService first.
func TestCreateContainerFailsWhenImageAbsent(t *testing.T) {
	s, sandboxID, rt, _ := newServerWithImageRuntime(t)
	_, err := s.CreateContainer(context.Background(), createReq(sandboxID, "app"))
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition for an unpulled image", status.Code(err))
	}
	if len(rt.created) != 0 {
		t.Errorf("no workload should be created when the image is absent; creates=%d", len(rt.created))
	}
}

func TestRegistryHost(t *testing.T) {
	cases := map[string]string{
		"alpine":                        "docker.io",
		"library/alpine":                "docker.io",
		"docker.io/library/alpine:3.20": "docker.io",
		"localhost:5000/img":            "localhost:5000",
		"registry.example.com/team/app": "registry.example.com",
		"ghcr.io/owner/repo:tag":        "ghcr.io",
	}
	for in, want := range cases {
		if got := registryHost(in); got != want {
			t.Errorf("registryHost(%q) = %q, want %q", in, got, want)
		}
	}
}
