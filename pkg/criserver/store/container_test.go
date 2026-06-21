package store

import (
	"os"
	"path/filepath"
	"testing"
)

func mustID(t *testing.T) string {
	t.Helper()
	id, err := NewID()
	if err != nil {
		t.Fatalf("NewID: %v", err)
	}
	return id
}

func newContainer(id, sandboxID string) *Container {
	c := &Container{
		ID:         id,
		SandboxID:  sandboxID,
		WorkloadID: DeriveWorkloadID(id),
		Image:      "docker.io/library/alpine:3.20",
		State:      ContainerCreated,
		CreatedAt:  1,
		Env:        map[string]string{"FOO": "bar"},
		Command:    []string{"/bin/sh"},
	}
	c.Metadata.Name = "app"
	c.Pod.Namespace = "default"
	c.Pod.Name = "pod"
	c.Pod.UID = "uid-1"
	return c
}

func TestDeriveWorkloadIDDeterministicAndBounded(t *testing.T) {
	id := mustID(t)
	a, b := DeriveWorkloadID(id), DeriveWorkloadID(id)
	if a != b {
		t.Errorf("DeriveWorkloadID not deterministic: %q != %q", a, b)
	}
	if len(a) > 40 {
		t.Errorf("workload id too long for runtime name limits: %q (%d)", a, len(a))
	}
	if DeriveWorkloadID(id) == DeriveWorkloadID(mustID(t)) {
		t.Error("distinct container ids produced the same workload id")
	}
}

func TestContainerStorePutGetDelete(t *testing.T) {
	s, _, err := NewContainerStore("")
	if err != nil {
		t.Fatalf("NewContainerStore: %v", err)
	}
	id := mustID(t)
	if err := s.Put(newContainer(id, "sb-1")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, ok := s.Get(id)
	if !ok {
		t.Fatal("Get: container not found after Put")
	}
	if got.WorkloadID != DeriveWorkloadID(id) || got.Image == "" {
		t.Errorf("Get returned unexpected record: %+v", got)
	}
	// Mutating the returned copy must not affect the store.
	got.Env["FOO"] = "mutated"
	again, _ := s.Get(id)
	if again.Env["FOO"] != "bar" {
		t.Error("Get did not return an isolated copy")
	}
	if err := s.Delete(id); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := s.Get(id); ok {
		t.Error("container still present after Delete")
	}
	// Delete is idempotent.
	if err := s.Delete(id); err != nil {
		t.Errorf("second Delete: %v", err)
	}
}

func TestContainerStoreRejectsBadID(t *testing.T) {
	s, _, _ := NewContainerStore("")
	if err := s.Put(&Container{ID: "short"}); err == nil {
		t.Error("Put accepted an invalid container id")
	}
}

func TestContainerStoreListBySandbox(t *testing.T) {
	s, _, _ := NewContainerStore("")
	id1, id2 := mustID(t), mustID(t)
	_ = s.Put(newContainer(id1, "sb-1"))
	_ = s.Put(newContainer(id2, "sb-2"))
	if got := s.ListBySandbox("sb-1"); len(got) != 1 || got[0].ID != id1 {
		t.Errorf("ListBySandbox(sb-1) = %+v, want one with id %s", got, id1)
	}
	if got := s.ListBySandbox("none"); len(got) != 0 {
		t.Errorf("ListBySandbox(none) = %+v, want empty", got)
	}
}

func TestContainerStoreReloadFromDisk(t *testing.T) {
	dir := t.TempDir()
	s, skipped, err := NewContainerStore(dir)
	if err != nil || skipped != 0 {
		t.Fatalf("NewContainerStore: err=%v skipped=%d", err, skipped)
	}
	id := mustID(t)
	c := newContainer(id, "sb-1")
	c.State = ContainerRunning
	c.StartedAt = 42
	if err := s.Put(c); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// A fresh store over the same dir must rediscover the record (restart tolerance).
	reloaded, skipped, err := NewContainerStore(dir)
	if err != nil || skipped != 0 {
		t.Fatalf("reload: err=%v skipped=%d", err, skipped)
	}
	got, ok := reloaded.Get(id)
	if !ok {
		t.Fatal("reloaded store missing the container")
	}
	if got.State != ContainerRunning || got.StartedAt != 42 || got.WorkloadID != c.WorkloadID {
		t.Errorf("reloaded record drifted: %+v", got)
	}
}

func TestContainerStoreSkipsCorruptAndForeignFiles(t *testing.T) {
	dir := t.TempDir()
	id := mustID(t)
	// Corrupt JSON.
	if err := os.WriteFile(filepath.Join(dir, id+".json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Valid JSON whose filename does not match its id (e.g. a misplaced record).
	other := mustID(t)
	if err := os.WriteFile(filepath.Join(dir, other+".json"), []byte(`{"id":"`+mustID(t)+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s, skipped, err := NewContainerStore(dir)
	if err != nil {
		t.Fatalf("NewContainerStore: %v", err)
	}
	if skipped != 2 {
		t.Errorf("skipped = %d, want 2", skipped)
	}
	if len(s.List()) != 0 {
		t.Errorf("List = %+v, want empty after skipping corrupt files", s.List())
	}
}
