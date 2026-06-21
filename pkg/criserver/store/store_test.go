package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testID(prefix byte) string {
	return strings.Repeat(string(prefix), 64)
}

func newSandbox(id, ns, name string, st State) *Sandbox {
	sb := &Sandbox{ID: id, State: st, CreatedAt: 1}
	sb.Metadata.Namespace = ns
	sb.Metadata.Name = name
	sb.Metadata.UID = "uid-" + id
	return sb
}

func TestNewIDUnique(t *testing.T) {
	a, err := NewID()
	if err != nil {
		t.Fatalf("NewID: %v", err)
	}
	b, _ := NewID()
	if a == b {
		t.Fatal("NewID returned duplicate IDs")
	}
	if len(a) != 64 {
		t.Errorf("ID length = %d, want 64 hex chars", len(a))
	}
	if !ValidID(a) {
		t.Errorf("NewID generated invalid ID %q", a)
	}
}

func TestPutGetDelete(t *testing.T) {
	s, _, err := New("")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := s.Get("missing"); ok {
		t.Fatal("Get on empty store returned ok")
	}
	id := testID('a')
	if err := s.Put(newSandbox(id, "ns", "pod", StateReady)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, ok := s.Get(id)
	if !ok || got.Metadata.Name != "pod" {
		t.Fatalf("Get = %+v, ok=%v", got, ok)
	}
	// Get returns a copy: mutating it must not change the stored record.
	got.Metadata.Name = "mutated"
	if again, _ := s.Get(id); again.Metadata.Name != "pod" {
		t.Error("Get did not return an isolated copy")
	}
	if err := s.Delete(id); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := s.Get(id); ok {
		t.Error("sandbox present after Delete")
	}
	// Delete is idempotent.
	if err := s.Delete(id); err != nil {
		t.Errorf("second Delete: %v", err)
	}
}

func TestValidID(t *testing.T) {
	valid := testID('a')
	cases := map[string]bool{
		valid:                           true,
		"":                              false,
		strings.Repeat("a", 63):         false,
		strings.Repeat("a", 65):         false,
		strings.Repeat("A", 64):         false,
		strings.Repeat("g", 64):         false,
		"../" + strings.Repeat("a", 61): false,
	}
	for id, want := range cases {
		if got := ValidID(id); got != want {
			t.Errorf("ValidID(%q) = %v, want %v", id, got, want)
		}
	}
}

func TestPutRejectsInvalidID(t *testing.T) {
	s, _, _ := New("")
	if err := s.Put(&Sandbox{}); err == nil {
		t.Error("Put with empty ID should error")
	}
	if err := s.Put(nil); err == nil {
		t.Error("Put(nil) should error")
	}
	if err := s.Put(newSandbox("a", "ns", "pod", StateReady)); err == nil {
		t.Error("Put with short ID should error")
	}
}

func TestSetState(t *testing.T) {
	s, _, _ := New("")
	id := testID('a')
	_ = s.Put(newSandbox(id, "ns", "pod", StateReady))

	ok, err := s.SetState(id, StateNotReady)
	if err != nil || !ok {
		t.Fatalf("SetState = (%v, %v)", ok, err)
	}
	if got, _ := s.Get(id); got.State != StateNotReady {
		t.Errorf("state = %v, want NotReady", got.State)
	}
	// SetState on a missing sandbox reports not-found without error (idempotent
	// Stop path relies on this).
	ok, err = s.SetState("missing", StateNotReady)
	if err != nil || ok {
		t.Errorf("SetState(missing) = (%v, %v), want (false, nil)", ok, err)
	}
}

func TestListSortedNewestFirst(t *testing.T) {
	s, _, _ := New("")
	older := newSandbox(testID('a'), "ns", "old", StateReady)
	older.CreatedAt = 100
	newer := newSandbox(testID('b'), "ns", "new", StateReady)
	newer.CreatedAt = 200
	_ = s.Put(older)
	_ = s.Put(newer)

	list := s.List()
	if len(list) != 2 || list[0].ID != newer.ID || list[1].ID != older.ID {
		t.Fatalf("List order = %+v, want newest first", list)
	}
}

func TestPersistenceAcrossReopen(t *testing.T) {
	id := testID('a')
	dir := t.TempDir()
	s, skipped, err := New(dir)
	if err != nil || skipped != 0 {
		t.Fatalf("New = (%v, %v)", skipped, err)
	}
	_ = s.Put(newSandbox(id, "ns", "pod", StateReady))
	_, _ = s.SetState(id, StateNotReady)

	// A file should exist on disk for the record.
	if _, err := os.Stat(filepath.Join(dir, id+".json")); err != nil {
		t.Fatalf("expected persisted file: %v", err)
	}

	// Reopen: the record (and its NotReady state) must survive.
	s2, skipped, err := New(dir)
	if err != nil || skipped != 0 {
		t.Fatalf("reopen New = (%v, %v)", skipped, err)
	}
	got, ok := s2.Get(id)
	if !ok {
		t.Fatal("record not loaded after reopen")
	}
	if got.State != StateNotReady {
		t.Errorf("loaded state = %v, want NotReady", got.State)
	}

	// Delete removes the file too.
	if err := s2.Delete(id); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, id+".json")); !os.IsNotExist(err) {
		t.Errorf("file still present after Delete: %v", err)
	}
}

func TestLoadSkipsCorruptRecords(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bad.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "empty.json"), []byte(`{"id":""}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mismatch.json"), []byte(`{"id":"`+testID('a')+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// A non-json file is ignored entirely.
	if err := os.WriteFile(filepath.Join(dir, "note.txt"), []byte("ignore me"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, skipped, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if skipped != 3 {
		t.Errorf("skipped = %d, want 3", skipped)
	}
	if len(s.List()) != 0 {
		t.Errorf("expected no valid records, got %d", len(s.List()))
	}
}

func TestDeleteAbsentDoesNotRemovePathOutsideStore(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "victim.json")
	if err := os.WriteFile(outside, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, _, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	escapeID := "../" + strings.TrimSuffix(filepath.Base(outside), ".json")
	if err := s.Delete(escapeID); err != nil {
		t.Fatalf("Delete(absent traversal id): %v", err)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("outside file was removed or changed: %v", err)
	}
}

func TestStoreDeepCopiesMapsAndSlices(t *testing.T) {
	s, _, _ := New("")
	id := testID('a')
	sb := newSandbox(id, "ns", "pod", StateReady)
	sb.Labels = map[string]string{"app": "pod"}
	sb.Annotations = map[string]string{"note": "original"}
	sb.DNS.Servers = []string{"10.0.0.10"}
	if err := s.Put(sb); err != nil {
		t.Fatal(err)
	}

	sb.Labels["app"] = "mutated-before-get"
	got, _ := s.Get(id)
	if got.Labels["app"] != "pod" {
		t.Fatalf("Put did not deep-copy labels: %v", got.Labels)
	}

	got.Labels["app"] = "mutated-after-get"
	got.Annotations["note"] = "mutated"
	got.DNS.Servers[0] = "127.0.0.1"
	again, _ := s.Get(id)
	if again.Labels["app"] != "pod" || again.Annotations["note"] != "original" || again.DNS.Servers[0] != "10.0.0.10" {
		t.Fatalf("Get did not deep-copy nested data: %+v", again)
	}

	list := s.List()
	list[0].Labels["app"] = "mutated-after-list"
	again, _ = s.Get(id)
	if again.Labels["app"] != "pod" {
		t.Fatalf("List did not deep-copy labels: %v", again.Labels)
	}
}
