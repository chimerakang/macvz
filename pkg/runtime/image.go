package runtime

import "context"

// ImageInfo describes a locally cached OCI image as reported by the host
// runtime (apple/container). It carries only the fields the experimental CRI
// ImageService needs (CRI-P4); honest partial data is preferred over fabricated
// fields when the runtime does not report something.
type ImageInfo struct {
	// ID is the stable, runtime-usable image reference returned to CRI clients as
	// ImageRef. Prefer a repo digest (name@sha256:...) when available so kubelet
	// can feed it back into CreateContainer/RemoveImage; fall back to the canonical
	// tag/reference when the runtime reports no digest.
	ID string
	// RepoTags are the "name:tag" references the image is known by locally.
	RepoTags []string
	// RepoDigests are digest references, present only when the runtime reports a
	// digest. The first entry is the runtime-usable repo digest when known; the raw
	// digest may also be included for filtering/debugging.
	RepoDigests []string
	// Size is the image's on-disk size in bytes; 0 when the runtime reports none.
	Size uint64
}

// ImageManager is an optional Runtime capability for inspecting and managing the
// local OCI image store, backing the experimental CRI ImageService (CRI-P4). A
// Runtime that cannot manage images simply does not implement it; the CRI
// adapter then serves the image surface honestly as not-configured.
type ImageManager interface {
	// ImageStatus returns the locally cached image matching ref, or a wrapped
	// ErrNotFound when no such image is present. ref may be a tag or a digest.
	ImageStatus(ctx context.Context, ref string) (ImageInfo, error)
	// ListImages enumerates every locally cached image. An empty store returns an
	// empty slice, not an error.
	ListImages(ctx context.Context) ([]ImageInfo, error)
	// RemoveImage deletes a locally cached image. Removing an absent image is a
	// no-op success, matching the idempotent CRI RemoveImage contract.
	RemoveImage(ctx context.Context, ref string) error
}
