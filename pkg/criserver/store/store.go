// Package store holds the experimental CRI Pod sandbox state for the MacVz CRI
// feasibility track (#74). It is deliberately a state-only model: a sandbox is a
// metadata record with a lifecycle, not a booted micro-VM or any host network
// resource. The store proves whether MacVz can honour the kubelet/crictl
// sandbox lifecycle and status contract before any data-plane work is attempted.
//
// Records are persisted as one JSON file per sandbox so the CRI adapter can be
// restarted without losing the kubelet's view of running sandboxes — a property
// the real CRI contract requires and the spike must demonstrate. Writes are
// atomic (temp file + rename) so a crash mid-write cannot corrupt a record.
package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// State is the lifecycle state of a sandbox, mirroring CRI's PodSandboxState.
type State string

const (
	// StateReady means the sandbox is running and can own containers.
	StateReady State = "Ready"
	// StateNotReady means the sandbox has been stopped; its resources (in this
	// state-only model, none) are reclaimed but the record survives until removal.
	StateNotReady State = "NotReady"
)

// Sandbox is the persisted record for one CRI Pod sandbox. The fields capture
// enough CRI metadata to map a sandbox ID back to its Kubernetes Pod identity
// (namespace/name/UID) and to answer PodSandboxStatus honestly.
type Sandbox struct {
	ID       string `json:"id"`
	Metadata struct {
		Name      string `json:"name"`
		UID       string `json:"uid"`
		Namespace string `json:"namespace"`
		Attempt   uint32 `json:"attempt"`
	} `json:"metadata"`
	State          State             `json:"state"`
	CreatedAt      int64             `json:"createdAt"` // unix nanoseconds
	Hostname       string            `json:"hostname,omitempty"`
	LogDirectory   string            `json:"logDirectory,omitempty"`
	RuntimeHandler string            `json:"runtimeHandler,omitempty"`
	Labels         map[string]string `json:"labels,omitempty"`
	Annotations    map[string]string `json:"annotations,omitempty"`
	DNS            struct {
		Servers  []string `json:"servers,omitempty"`
		Searches []string `json:"searches,omitempty"`
		Options  []string `json:"options,omitempty"`
	} `json:"dns,omitempty"`
}

const sandboxIDBytes = 32

// Store is an in-memory index of sandboxes backed by an on-disk directory. It is
// safe for concurrent use; the CRI server may receive overlapping calls.
type Store struct {
	mu  sync.RWMutex
	dir string
	m   map[string]*Sandbox
}

// New opens (creating if needed) a sandbox store rooted at dir and loads any
// records persisted by a previous run. A record that fails to parse is skipped
// rather than failing the whole load, so one corrupt file cannot wedge the
// adapter on restart; the error count is returned for visibility.
//
// An empty dir yields an in-memory-only store with no persistence, which is what
// the default skeleton and unit tests want — they need the sandbox lifecycle
// without touching the filesystem.
func New(dir string) (*Store, int, error) {
	if dir == "" {
		return &Store{m: make(map[string]*Sandbox)}, 0, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, 0, fmt.Errorf("store: create %s: %w", dir, err)
	}
	s := &Store{dir: dir, m: make(map[string]*Sandbox)}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, 0, fmt.Errorf("store: read %s: %w", dir, err)
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
		var sb Sandbox
		if err := json.Unmarshal(b, &sb); err != nil || !ValidID(sb.ID) || e.Name() != sb.ID+".json" {
			skipped++
			continue
		}
		cp := cloneSandbox(&sb)
		s.m[sb.ID] = &cp
	}
	return s, skipped, nil
}

// NewID returns a fresh, collision-resistant sandbox ID (64 hex chars), matching
// the opaque-string shape kubelet/crictl expect from a CRI runtime.
func NewID() (string, error) {
	var b [sandboxIDBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("store: generate id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// ValidID reports whether id is a store-owned CRI sandbox ID. Restricting IDs to
// the generated 64-hex form prevents a client-supplied ID from escaping the
// state directory when a record is persisted or deleted.
func ValidID(id string) bool {
	if len(id) != sandboxIDBytes*2 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		default:
			return false
		}
	}
	return true
}

// Put inserts or replaces a sandbox and persists it atomically.
func (s *Store) Put(sb *Sandbox) error {
	if sb == nil || !ValidID(sb.ID) {
		return fmt.Errorf("store: put: sandbox id must be 64 lowercase hex chars")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.persist(sb); err != nil {
		return err
	}
	cp := cloneSandbox(sb)
	s.m[sb.ID] = &cp
	return nil
}

// Get returns a copy of the sandbox with the given ID, or false if absent.
func (s *Store) Get(id string) (Sandbox, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sb, ok := s.m[id]
	if !ok {
		return Sandbox{}, false
	}
	return cloneSandbox(sb), true
}

// SetState transitions a sandbox to the given state and persists it. It returns
// false if the sandbox is absent; callers decide whether that is an error (it is
// not for the idempotent Stop path).
func (s *Store) SetState(id string, state State) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sb, ok := s.m[id]
	if !ok {
		return false, nil
	}
	if sb.State == state {
		return true, nil
	}
	sb.State = state
	if err := s.persist(sb); err != nil {
		return true, err
	}
	return true, nil
}

// Delete removes a sandbox from memory and disk. It is idempotent: removing an
// absent sandbox succeeds, matching CRI's RemovePodSandbox contract.
func (s *Store) Delete(id string) error {
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
		return fmt.Errorf("store: delete %s: %w", id, err)
	}
	return nil
}

// List returns copies of all sandboxes, sorted by creation time (newest first)
// for stable, human-readable crictl output.
func (s *Store) List() []Sandbox {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Sandbox, 0, len(s.m))
	for _, sb := range s.m {
		out = append(out, cloneSandbox(sb))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt != out[j].CreatedAt {
			return out[i].CreatedAt > out[j].CreatedAt
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func (s *Store) path(id string) string {
	return filepath.Join(s.dir, id+".json")
}

func cloneSandbox(sb *Sandbox) Sandbox {
	if sb == nil {
		return Sandbox{}
	}
	cp := *sb
	cp.Labels = cloneStringMap(sb.Labels)
	cp.Annotations = cloneStringMap(sb.Annotations)
	cp.DNS.Servers = cloneStringSlice(sb.DNS.Servers)
	cp.DNS.Searches = cloneStringSlice(sb.DNS.Searches)
	cp.DNS.Options = cloneStringSlice(sb.DNS.Options)
	return cp
}

func cloneStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneStringSlice(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

// persist atomically writes a sandbox record. Caller holds s.mu. It is a no-op
// for an in-memory store (empty dir).
func (s *Store) persist(sb *Sandbox) error {
	if s.dir == "" {
		return nil
	}
	b, err := json.MarshalIndent(sb, "", "  ")
	if err != nil {
		return fmt.Errorf("store: marshal %s: %w", sb.ID, err)
	}
	tmp, err := os.CreateTemp(s.dir, sb.ID+".*.tmp")
	if err != nil {
		return fmt.Errorf("store: temp for %s: %w", sb.ID, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("store: write %s: %w", sb.ID, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("store: close %s: %w", sb.ID, err)
	}
	if err := os.Rename(tmpName, s.path(sb.ID)); err != nil {
		return fmt.Errorf("store: rename %s: %w", sb.ID, err)
	}
	return nil
}
