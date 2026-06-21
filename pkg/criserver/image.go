package criserver

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"

	"github.com/chimerakang/macvz/pkg/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	"k8s.io/klog/v2"
)

// This file implements the CRI-P4 ImageService (#76): PullImage, ImageStatus,
// ListImages, RemoveImage, and ImageFsInfo, driven over the apple/container image
// store. It moves image lifecycle out of CreateContainer (CRI-P3) and onto the
// ImageService, where kubelet and crictl expect it.
//
// As with the container surface, every method is honest about a missing backend:
// with no image runtime wired, the mutating methods return FailedPrecondition
// rather than faking success, and the read-only surfaces (ListImages,
// ImageFsInfo) report empty rather than fabricated data.

// ImageRuntime is the subset of the apple/container driver the CRI ImageService
// drives. *container.Driver satisfies it (Pull from runtime.Runtime, the rest
// from runtime.ImageManager); tests inject a fake. It is defined here so the CRI
// adapter depends only on the operations it calls.
type ImageRuntime interface {
	Pull(ctx context.Context, image string, auth *runtime.RegistryAuth) error
	ImageStatus(ctx context.Context, image string) (runtime.ImageInfo, error)
	ListImages(ctx context.Context) ([]runtime.ImageInfo, error)
	RemoveImage(ctx context.Context, image string) error
}

// imageFsReporter is the optional capability the ImageService uses to report
// honest image-filesystem usage. *container.Driver satisfies it via
// runtime.DiskReporter; a runtime without it makes ImageFsInfo degrade to empty.
type imageFsReporter interface {
	NodeFilesystem(ctx context.Context) (runtime.FilesystemUsage, error)
	ImageCacheUsage(ctx context.Context) (runtime.ImageCacheUsage, error)
}

// PullImage pulls an image into the local store, optionally authenticating with
// the CRI-supplied registry credentials. It returns a runtime-usable image ref,
// which kubelet records and later feeds back into CreateContainer/RemoveImage.
func (s *Server) PullImage(ctx context.Context, req *runtimeapi.PullImageRequest) (*runtimeapi.PullImageResponse, error) {
	if s.imageRuntime == nil {
		return nil, errImageServiceNotConfigured("PullImage")
	}
	image := req.GetImage().GetImage()
	if image == "" {
		return nil, status.Error(codes.InvalidArgument, "PullImage: image reference is required")
	}
	auth, err := toRegistryAuth(req.GetAuth(), image)
	if err != nil {
		return nil, err
	}
	if err := s.imageRuntime.Pull(ctx, image, auth); err != nil {
		return nil, runtimeError("PullImage", "pull image", err)
	}
	// Resolve the runtime-usable image ref for the response. A failure here does
	// not fail the pull (the image is already local); fall back to the requested
	// reference.
	ref := image
	if info, serr := s.imageRuntime.ImageStatus(ctx, image); serr == nil && info.ID != "" {
		ref = info.ID
	}
	klog.V(4).InfoS("CRI PullImage", "image", image, "imageRef", ref, "authenticated", auth != nil)
	return &runtimeapi.PullImageResponse{ImageRef: ref}, nil
}

// ImageStatus returns the status of a locally cached image. Per the CRI
// contract, an absent image is reported as an empty (non-error) response so
// kubelet/crictl can distinguish "not present" from a runtime failure.
func (s *Server) ImageStatus(ctx context.Context, req *runtimeapi.ImageStatusRequest) (*runtimeapi.ImageStatusResponse, error) {
	if s.imageRuntime == nil {
		return nil, errImageServiceNotConfigured("ImageStatus")
	}
	image := req.GetImage().GetImage()
	if image == "" {
		return nil, status.Error(codes.InvalidArgument, "ImageStatus: image reference is required")
	}
	info, err := s.imageRuntime.ImageStatus(ctx, image)
	if err != nil {
		if errors.Is(err, runtime.ErrNotFound) {
			return &runtimeapi.ImageStatusResponse{}, nil
		}
		return nil, runtimeError("ImageStatus", "inspect image", err)
	}
	resp := &runtimeapi.ImageStatusResponse{Image: toCRIImage(info)}
	if req.GetVerbose() {
		resp.Info = map[string]string{
			"track":      "CRI-P4 ImageService (docs/CRI_FEASIBILITY.md)",
			"imageStore": "apple/container",
		}
	}
	return resp, nil
}

// ListImages returns locally cached images matching the optional filter. With no
// image runtime wired it returns an empty list — honest, since nothing is
// tracked — matching the pre-CRI-P4 skeleton behavior.
func (s *Server) ListImages(ctx context.Context, req *runtimeapi.ListImagesRequest) (*runtimeapi.ListImagesResponse, error) {
	if s.imageRuntime == nil {
		return &runtimeapi.ListImagesResponse{}, nil
	}
	infos, err := s.imageRuntime.ListImages(ctx)
	if err != nil {
		return nil, runtimeError("ListImages", "list images", err)
	}
	var items []*runtimeapi.Image
	for _, info := range infos {
		img := toCRIImage(info)
		if matchesImageFilter(img, req.GetFilter()) {
			items = append(items, img)
		}
	}
	return &runtimeapi.ListImagesResponse{Images: items}, nil
}

// RemoveImage removes a locally cached image. It is idempotent at the driver
// level: removing an absent image succeeds.
func (s *Server) RemoveImage(ctx context.Context, req *runtimeapi.RemoveImageRequest) (*runtimeapi.RemoveImageResponse, error) {
	if s.imageRuntime == nil {
		return nil, errImageServiceNotConfigured("RemoveImage")
	}
	image := req.GetImage().GetImage()
	if image == "" {
		return nil, status.Error(codes.InvalidArgument, "RemoveImage: image reference is required")
	}
	if err := s.imageRuntime.RemoveImage(ctx, image); err != nil {
		return nil, runtimeError("RemoveImage", "remove image", err)
	}
	klog.V(4).InfoS("CRI RemoveImage", "image", image)
	return &runtimeapi.RemoveImageResponse{}, nil
}

// ImageFsInfo reports usage of the filesystem backing the image store. It uses
// the runtime's honest data — the image-cache size for used bytes and the data
// root's filesystem for the mountpoint and inode usage — when the runtime can
// report it. When it cannot (no reporter, or a sampling error) it returns an
// empty response rather than fabricated usage, so kubelet's image GC sees no
// fake numbers.
func (s *Server) ImageFsInfo(ctx context.Context, _ *runtimeapi.ImageFsInfoRequest) (*runtimeapi.ImageFsInfoResponse, error) {
	reporter, ok := s.imageRuntime.(imageFsReporter)
	if !ok {
		return &runtimeapi.ImageFsInfoResponse{}, nil
	}
	cache, err := reporter.ImageCacheUsage(ctx)
	if err != nil {
		klog.V(2).InfoS("ImageFsInfo: image-cache usage unavailable; reporting no image filesystem", "err", err)
		return &runtimeapi.ImageFsInfoResponse{}, nil
	}
	usage := &runtimeapi.FilesystemUsage{
		Timestamp: cache.Timestamp.UnixNano(),
		UsedBytes: &runtimeapi.UInt64Value{Value: cache.TotalBytes},
	}
	// The mountpoint and inode usage come from the data-root filesystem. They are
	// best-effort: a statfs failure still yields honest image-cache used bytes.
	if fu, ferr := reporter.NodeFilesystem(ctx); ferr == nil {
		usage.FsId = &runtimeapi.FilesystemIdentifier{Mountpoint: fu.Path}
		usage.InodesUsed = &runtimeapi.UInt64Value{Value: fu.UsedInodes}
	}
	return &runtimeapi.ImageFsInfoResponse{ImageFilesystems: []*runtimeapi.FilesystemUsage{usage}}, nil
}

// toCRIImage maps a runtime.ImageInfo onto the CRI Image message.
func toCRIImage(info runtime.ImageInfo) *runtimeapi.Image {
	return &runtimeapi.Image{
		Id:          info.ID,
		RepoTags:    info.RepoTags,
		RepoDigests: info.RepoDigests,
		Size:        info.Size,
	}
}

// matchesImageFilter applies a CRI ImageFilter. A nil/empty filter matches all;
// a reference filter matches an image's ID, any RepoTag, or any RepoDigest.
func matchesImageFilter(img *runtimeapi.Image, f *runtimeapi.ImageFilter) bool {
	ref := f.GetImage().GetImage()
	if ref == "" {
		return true
	}
	if img.GetId() == ref {
		return true
	}
	for _, t := range img.GetRepoTags() {
		if t == ref {
			return true
		}
	}
	for _, d := range img.GetRepoDigests() {
		if d == ref {
			return true
		}
	}
	return false
}

// toRegistryAuth maps a CRI AuthConfig onto runtime.RegistryAuth, the credential
// the apple/container driver uses for an authenticated pull (#49). A nil auth or
// an empty credential means an anonymous pull (nil result).
//
// Username/password (or a base64 "user:password" in Auth) are supported. Token
// credentials (IdentityToken/RegistryToken) are not — apple/container's
// `registry login` takes only username/password — so they are rejected with a
// clear Unimplemented rather than silently dropped. When ServerAddress is empty
// the registry host is derived from the image reference.
func toRegistryAuth(auth *runtimeapi.AuthConfig, image string) (*runtime.RegistryAuth, error) {
	if auth == nil {
		return nil, nil
	}
	if auth.GetIdentityToken() != "" || auth.GetRegistryToken() != "" {
		return nil, status.Error(codes.Unimplemented,
			"PullImage: token-based registry auth (identity/registry token) is not supported by the apple/container backend; supply username/password credentials")
	}
	username, password := auth.GetUsername(), auth.GetPassword()
	if username == "" && password == "" && auth.GetAuth() != "" {
		u, p, err := decodeBasicAuth(auth.GetAuth())
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "PullImage: decode auth: %v", err)
		}
		username, password = u, p
	}
	if username == "" && password == "" {
		return nil, nil
	}
	server := auth.GetServerAddress()
	if server == "" {
		server = registryHost(image)
	}
	return &runtime.RegistryAuth{Server: server, Username: username, Password: password}, nil
}

// decodeBasicAuth decodes a base64 "username:password" credential as carried in
// AuthConfig.Auth (the docker config "auth" field).
func decodeBasicAuth(encoded string) (string, string, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return "", "", err
	}
	user, pass, ok := strings.Cut(string(raw), ":")
	if !ok {
		return "", "", errors.New("auth is not in user:password form")
	}
	return user, pass, nil
}

// registryHost extracts the registry host from an image reference. A reference
// whose first path component contains "." or ":" or is "localhost" names an
// explicit registry; otherwise the image is a Docker Hub short name and the host
// defaults to "docker.io" (what apple/container's `registry login` expects).
func registryHost(image string) string {
	slash := strings.IndexByte(image, '/')
	if slash < 0 {
		return "docker.io"
	}
	first := image[:slash]
	if first == "localhost" || strings.ContainsAny(first, ".:") {
		return first
	}
	return "docker.io"
}

// errImageServiceNotConfigured is returned by image methods when no image runtime
// is wired (e.g. the default skeleton). The methods are implemented but cannot
// act without a backend, so FailedPrecondition is the honest code.
func errImageServiceNotConfigured(method string) error {
	return status.Errorf(codes.FailedPrecondition,
		"%s: no image runtime is configured (experimental adapter started without apple/container backend)", method)
}
