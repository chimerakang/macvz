package store

// This file adds the CRI-P3 container state store (#75). A container record maps
// a CRI container ID to the apple/container workload that backs it, the sandbox
// it belongs to, the Kubernetes Pod identity inherited from that sandbox, and the
// image/command/env it was created with. Like the sandbox store it is restart
// tolerant: each record is one atomically-written JSON file, so a restarted CRI
// adapter rediscovers its containers and can answer status/list without the
// kubelet's view drifting.
//
// Container records live in their own directory, separate from sandbox records,
// so the two stores never read each other's files.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// ContainerState is the lifecycle state of a container, mirroring CRI's
// ContainerState enum (CONTAINER_CREATED/RUNNING/EXITED).
type ContainerState string

const (
	// ContainerCreated means the workload has been created but not started.
	ContainerCreated ContainerState = "Created"
	// ContainerRunning means the workload's micro-VM is running.
	ContainerRunning ContainerState = "Running"
	// ContainerExited means the workload has stopped; its exit information is
	// captured on the record and survives until removal.
	ContainerExited ContainerState = "Exited"
)

// Container is the persisted record for one CRI container. The fields capture
// enough to map the CRI container ID back to its apple/container workload and to
// answer ContainerStatus honestly after an adapter restart.
type Container struct {
	ID        string `json:"id"`
	SandboxID string `json:"sandboxID"`
	// WorkloadID is the apple/container workload name backing this container. It
	// is derived deterministically from the container ID (see DeriveWorkloadID) so
	// a restarted adapter can address the same workload without extra state.
	WorkloadID string `json:"workloadID"`
	Metadata   struct {
		Name    string `json:"name"`
		Attempt uint32 `json:"attempt"`
	} `json:"metadata"`
	// Pod is the Kubernetes Pod identity inherited from the owning sandbox.
	Pod struct {
		Name      string `json:"name"`
		UID       string `json:"uid"`
		Namespace string `json:"namespace"`
	} `json:"pod"`
	Image       string            `json:"image"`
	Command     []string          `json:"command,omitempty"`
	Args        []string          `json:"args,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
	LogPath     string            `json:"logPath,omitempty"`
	// Mounts are the kubelet-provided filesystem mounts realized for this
	// container (CRI-P7, #79). They are persisted so ContainerStatus can report
	// them faithfully and so a restarted adapter recovers the same view without
	// re-deriving it from the runtime.
	Mounts []Mount `json:"mounts,omitempty"`
	State       ContainerState    `json:"state"`
	CreatedAt   int64             `json:"createdAt"`            // unix nanoseconds
	StartedAt   int64             `json:"startedAt,omitempty"`  // unix nanoseconds
	FinishedAt  int64             `json:"finishedAt,omitempty"` // unix nanoseconds
	ExitCode    int32             `json:"exitCode,omitempty"`
	Reason      string            `json:"reason,omitempty"`
	Message     string            `json:"message,omitempty"`
}

// Mount is a persisted filesystem mount realized for a container (CRI-P7, #79).
// It mirrors the subset of the CRI Mount the adapter acts on: a host source bound
// into the guest at a target path, or a guest-local tmpfs when Tmpfs is set. The
// kubelet has already materialized projected ConfigMap/Secret/Downward/SA-token
// and emptyDir content on the host before passing these, so the record captures
// what was mounted, not how it was projected.
type Mount struct {
	HostPath      string `json:"hostPath,omitempty"`
	ContainerPath string `json:"containerPath"`
	ReadOnly      bool   `json:"readOnly,omitempty"`
	Tmpfs         bool   `json:"tmpfs,omitempty"`
}

// DeriveWorkloadID maps a CRI container ID to a deterministic apple/container
// workload name. A raw 64-hex container ID can exceed the runtime's name length
// limits, so this takes a stable prefix under a "macvz-cri-" namespace. It is a
// pure function of the container ID: a restarted adapter recomputes the same name
// for the same container without persisting it.
func DeriveWorkloadID(containerID string) string {
	const prefixHex = 24 // 96 bits of the id is ample to avoid collisions
	short := containerID
	if len(short) > prefixHex {
		short = short[:prefixHex]
	}
	return "macvz-cri-" + short
}

// ContainerStore is an in-memory index of containers backed by an on-disk
// directory, mirroring Store. It is safe for concurrent use.
type ContainerStore struct {
	mu  sync.RWMutex
	dir string
	m   map[string]*Container
}

// NewContainerStore opens (creating if needed) a container store rooted at dir
// and loads any records from a previous run. A record that fails to parse is
// skipped (one corrupt file cannot wedge the adapter); the skip count is
// returned. An empty dir yields an in-memory-only store, which is what the
// default skeleton and unit tests want.
func NewContainerStore(dir string) (*ContainerStore, int, error) {
	if dir == "" {
		return &ContainerStore{m: make(map[string]*Container)}, 0, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, 0, fmt.Errorf("container store: create %s: %w", dir, err)
	}
	s := &ContainerStore{dir: dir, m: make(map[string]*Container)}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, 0, fmt.Errorf("container store: read %s: %w", dir, err)
	}
	skipped := 0
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			skipped++
			continue
		}
		var c Container
		if err := json.Unmarshal(b, &c); err != nil || !ValidID(c.ID) || e.Name() != c.ID+".json" {
			skipped++
			continue
		}
		cp := cloneContainer(&c)
		s.m[c.ID] = &cp
	}
	return s, skipped, nil
}

// Put inserts or replaces a container and persists it atomically.
func (s *ContainerStore) Put(c *Container) error {
	if c == nil || !ValidID(c.ID) {
		return fmt.Errorf("container store: put: container id must be 64 lowercase hex chars")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.persist(c); err != nil {
		return err
	}
	cp := cloneContainer(c)
	s.m[c.ID] = &cp
	return nil
}

// Get returns a copy of the container with the given ID, or false if absent.
func (s *ContainerStore) Get(id string) (Container, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.m[id]
	if !ok {
		return Container{}, false
	}
	return cloneContainer(c), true
}

// Delete removes a container from memory and disk. It is idempotent.
func (s *ContainerStore) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[id]; !ok {
		return nil
	}
	delete(s.m, id)
	if s.dir == "" {
		return nil
	}
	if err := os.Remove(s.path(id)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("container store: delete %s: %w", id, err)
	}
	return nil
}

// List returns copies of all containers, newest first.
func (s *ContainerStore) List() []Container {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Container, 0, len(s.m))
	for _, c := range s.m {
		out = append(out, cloneContainer(c))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt != out[j].CreatedAt {
			return out[i].CreatedAt > out[j].CreatedAt
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// ListBySandbox returns copies of all containers belonging to the given sandbox.
// CRI-P3 supports one container per sandbox, so this is how CreateContainer
// detects an already-occupied sandbox.
func (s *ContainerStore) ListBySandbox(sandboxID string) []Container {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Container
	for _, c := range s.m {
		if c.SandboxID == sandboxID {
			out = append(out, cloneContainer(c))
		}
	}
	return out
}

func (s *ContainerStore) path(id string) string {
	return filepath.Join(s.dir, id+".json")
}

// persist atomically writes a container record. Caller holds s.mu. It is a no-op
// for an in-memory store (empty dir).
func (s *ContainerStore) persist(c *Container) error {
	if s.dir == "" {
		return nil
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("container store: marshal %s: %w", c.ID, err)
	}
	tmp, err := os.CreateTemp(s.dir, c.ID+".*.tmp")
	if err != nil {
		return fmt.Errorf("container store: temp for %s: %w", c.ID, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("container store: write %s: %w", c.ID, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("container store: close %s: %w", c.ID, err)
	}
	if err := os.Rename(tmpName, s.path(c.ID)); err != nil {
		return fmt.Errorf("container store: rename %s: %w", c.ID, err)
	}
	return nil
}

func cloneContainer(c *Container) Container {
	if c == nil {
		return Container{}
	}
	cp := *c
	cp.Command = cloneStringSlice(c.Command)
	cp.Args = cloneStringSlice(c.Args)
	cp.Env = cloneStringMap(c.Env)
	cp.Labels = cloneStringMap(c.Labels)
	cp.Annotations = cloneStringMap(c.Annotations)
	if c.Mounts != nil {
		cp.Mounts = append([]Mount(nil), c.Mounts...)
	}
	return cp
}
