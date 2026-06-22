package container

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/chimerakang/macvz/pkg/runtime"
)

// This file implements the runtime.ImageManager capability over apple/container's
// `image inspect`, `image ls`, and `image delete` subcommands, backing the
// experimental CRI ImageService (CRI-P4, #76). Image pull lives in driver.go
// (Pull), since it already verifies the bootable arch variant.
//
// apple/container's image metadata does not map perfectly onto CRI's image
// fields. The driver prefers honest partial data: when a digest is available,
// ImageInfo.ID is a runtime-usable repo digest rather than a naked digest; when
// no digest is available, it degrades to the reference instead of fabricating one.

var _ runtime.ImageManager = (*Driver)(nil)

// ImageStatus inspects a single locally cached image. A missing image surfaces
// as a wrapped runtime.ErrNotFound (the CRI layer maps that to an empty, non-
// error ImageStatus response).
func (d *Driver) ImageStatus(ctx context.Context, ref string) (runtime.ImageInfo, error) {
	if ref == "" {
		return runtime.ImageInfo{}, fmt.Errorf("image status: image reference is empty")
	}
	out, err := d.run.output(ctx, "image", "inspect", ref)
	if err != nil {
		mapped := mapErr(err)
		if errors.Is(mapped, runtime.ErrNotFound) {
			if info, ok, ferr := d.findImageByKnownRef(ctx, ref); ferr != nil {
				return runtime.ImageInfo{}, fmt.Errorf("image status %q: fallback list images: %w", ref, ferr)
			} else if ok {
				return info, nil
			}
		}
		return runtime.ImageInfo{}, fmt.Errorf("image status %q: %w", ref, mapped)
	}
	trimmed := bytes.TrimSpace(out)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return runtime.ImageInfo{}, fmt.Errorf("image status %q: %w", ref, runtime.ErrNotFound)
	}
	entries, err := parseImageEntries(trimmed)
	if err != nil {
		return runtime.ImageInfo{}, fmt.Errorf("image status %q: parse inspect output: %w", ref, err)
	}
	if len(entries) == 0 {
		return runtime.ImageInfo{}, fmt.Errorf("image status %q: %w", ref, runtime.ErrNotFound)
	}
	return imageInfoFromEntry(entries[0]), nil
}

func (d *Driver) findImageByKnownRef(ctx context.Context, ref string) (runtime.ImageInfo, bool, error) {
	infos, err := d.ListImages(ctx)
	if err != nil {
		return runtime.ImageInfo{}, false, err
	}
	for _, info := range infos {
		if imageInfoMatchesRef(info, ref) {
			return info, true, nil
		}
	}
	return runtime.ImageInfo{}, false, nil
}

func imageInfoMatchesRef(info runtime.ImageInfo, ref string) bool {
	if info.ID == ref {
		return true
	}
	for _, tag := range info.RepoTags {
		if tag == ref {
			return true
		}
	}
	for _, digest := range info.RepoDigests {
		if digest == ref {
			return true
		}
	}
	return false
}

// ListImages enumerates every locally cached image by parsing
// `image ls --format json`. An empty store returns an empty slice, not an error.
func (d *Driver) ListImages(ctx context.Context) ([]runtime.ImageInfo, error) {
	out, err := d.run.output(ctx, "image", "ls", "--format", "json")
	if err != nil {
		return nil, fmt.Errorf("list images: %w", mapErr(err))
	}
	trimmed := bytes.TrimSpace(out)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil, nil
	}
	entries, err := parseImageEntries(trimmed)
	if err != nil {
		return nil, fmt.Errorf("list images: parse image list: %w", err)
	}
	infos := make([]runtime.ImageInfo, 0, len(entries))
	for _, e := range entries {
		infos = append(infos, imageInfoFromEntry(e))
	}
	return infos, nil
}

// RemoveImage deletes a locally cached image. Removing an absent image is a
// no-op success, matching the idempotent CRI RemoveImage contract.
func (d *Driver) RemoveImage(ctx context.Context, ref string) error {
	if ref == "" {
		return fmt.Errorf("remove image: image reference is empty")
	}
	if _, err := d.run.output(ctx, "image", "delete", ref); err != nil {
		if errors.Is(mapErr(err), runtime.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("remove image %q: %w", ref, mapErr(err))
	}
	return nil
}

// imageInfoFromEntry maps an apple/container image record onto runtime.ImageInfo.
// The ID prefers a runtime-usable repo digest (name@sha256:...) and falls back to
// the reference when no digest is available — honest partial data rather than a
// fabricated ID. Naked digests are kept in RepoDigests for filter matching, but
// not used as ID because kubelet feeds ImageRef back into CreateContainer and
// RemoveImage.
func imageInfoFromEntry(e imageListEntry) runtime.ImageInfo {
	ref := entryReference(e)
	info := runtime.ImageInfo{Size: uint64(e.Size)}
	if ref != "" {
		info.RepoTags = []string{ref}
	}

	digest := entryDigest(e)
	if digest != "" {
		if name := repoName(ref); name != "" {
			repoDigest := name + "@" + digest
			info.ID = repoDigest
			info.RepoDigests = []string{repoDigest, digest}
		} else {
			info.ID = ref
			info.RepoDigests = []string{digest}
		}
	} else {
		info.ID = ref
	}
	if info.ID == "" && e.ID != "" {
		info.ID = normalizeDigest(e.ID)
	}

	// Some builds report size only under the platform variants; sum them when the
	// top-level size is absent so the figure stays honest for multi-arch images.
	if info.Size == 0 {
		for _, v := range e.Variants {
			info.Size += uint64(v.Size)
		}
	}
	return info
}

// entryReference returns the tag/reference field across apple/container JSON
// schema versions. container 1.0 nests it under configuration.name.
func entryReference(e imageListEntry) string {
	if e.Reference != "" {
		return e.Reference
	}
	if e.Name != "" {
		return e.Name
	}
	return e.Configuration.Name
}

// entryDigest returns the first digest the runtime reported for an image, in
// preference order, or "" when none is present.
func entryDigest(e imageListEntry) string {
	if e.Digest != "" {
		return e.Digest
	}
	if e.Configuration.Descriptor.Digest != "" {
		return e.Configuration.Descriptor.Digest
	}
	if e.Descriptor.Digest != "" {
		return e.Descriptor.Digest
	}
	for _, v := range e.Variants {
		if v.Descriptor.Digest != "" {
			return v.Descriptor.Digest
		}
	}
	return normalizeDigest(e.ID)
}

func normalizeDigest(d string) string {
	if d == "" {
		return ""
	}
	if bytes.Contains([]byte(d), []byte(":")) {
		return d
	}
	return "sha256:" + d
}

// repoName strips the tag from a reference so a digest can be appended as a
// "name@digest" RepoDigest. A digest-only or untagged reference is returned
// unchanged. The registry-host colon (e.g. "host:5000/img") is preserved.
func repoName(ref string) string {
	if ref == "" {
		return ""
	}
	// A reference with a digest already cannot host a RepoDigest reformat.
	if i := bytes.IndexByte([]byte(ref), '@'); i >= 0 {
		return ref[:i]
	}
	// Find a tag colon: the last colon that is not part of a registry host:port,
	// i.e. one that appears after the final '/'.
	slash := lastIndexByte(ref, '/')
	colon := lastIndexByte(ref, ':')
	if colon > slash {
		return ref[:colon]
	}
	return ref
}

func lastIndexByte(s string, b byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == b {
			return i
		}
	}
	return -1
}
