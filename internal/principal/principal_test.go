package principal

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestObserveUpsertsAndMergesGroups(t *testing.T) {
	dir := t.TempDir()
	r := New(dir)

	if err := r.Observe("alice@example.com", "Alice", []string{"eng"}, SourceLogin); err != nil {
		t.Fatal(err)
	}
	// A second sighting merges new groups, updates the display name, and keeps a
	// single entry (upsert, not append).
	if err := r.Observe("alice@example.com", "Alice A", []string{"eng", "ops"}, SourceCFHeader); err != nil {
		t.Fatal(err)
	}
	if err := r.Observe("bob@example.com", "", nil, SourceGrantTarget); err != nil {
		t.Fatal(err)
	}

	list := r.List()
	if len(list) != 2 {
		t.Fatalf("List len = %d, want 2 (alice upserted, bob added)", len(list))
	}
	alice := list[0]
	if alice.Email != "alice@example.com" {
		t.Fatalf("sorted[0] = %q, want alice", alice.Email)
	}
	if alice.DisplayName != "Alice A" {
		t.Errorf("display name = %q, want the updated Alice A", alice.DisplayName)
	}
	if len(alice.GroupsSeen) != 2 || alice.GroupsSeen[0] != "eng" || alice.GroupsSeen[1] != "ops" {
		t.Errorf("groups = %v, want [eng ops] merged+deduped", alice.GroupsSeen)
	}
	if alice.FirstSeen.IsZero() || alice.LastSeen.Before(alice.FirstSeen) {
		t.Errorf("timestamps wrong: first=%v last=%v", alice.FirstSeen, alice.LastSeen)
	}
}

func TestObserveEmptyEmailIgnored(t *testing.T) {
	r := New(t.TempDir())
	if err := r.Observe("", "Nobody", []string{"x"}, SourceLogin); err != nil {
		t.Fatal(err)
	}
	if got := r.List(); len(got) != 0 {
		t.Errorf("List after empty-email Observe = %v, want empty", got)
	}
}

func TestRegistryPersistsAcrossInstances(t *testing.T) {
	dir := t.TempDir()
	if err := New(dir).Observe("alice@example.com", "Alice", []string{"eng"}, SourceLogin); err != nil {
		t.Fatal(err)
	}
	// A fresh Registry over the same directory reads the persisted state (as a
	// hub restart would).
	list := New(dir).List()
	if len(list) != 1 || list[0].Email != "alice@example.com" {
		t.Fatalf("reloaded list = %+v, want the persisted alice", list)
	}
}

func TestCorruptFileToleratedAsEmpty(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, fileName), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := New(dir)
	if got := r.List(); len(got) != 0 {
		t.Errorf("List over a corrupt file = %v, want empty", got)
	}
	// Observe must recover by overwriting the corrupt file, not error out.
	if err := r.Observe("alice@example.com", "Alice", nil, SourceLogin); err != nil {
		t.Fatalf("Observe over a corrupt file error = %v, want recovery", err)
	}
	if got := r.List(); len(got) != 1 {
		t.Errorf("List after recovery = %v, want one entry", got)
	}
}

func TestObserveConcurrent(t *testing.T) {
	// Under -race this proves the mutex-guarded whole-file read/write introduces
	// no data race and never loses an upsert to a torn file.
	r := New(t.TempDir())
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = r.Observe("alice@example.com", "Alice", []string{"eng"}, SourceLogin)
		}()
	}
	wg.Wait()
	if got := r.List(); len(got) != 1 {
		t.Errorf("concurrent Observe of one email produced %d entries, want 1", len(got))
	}
}
