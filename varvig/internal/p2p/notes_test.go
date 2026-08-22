package p2p

import (
	"strings"
	"testing"

	"github.com/dividebyzero/claude-experiments/varvig/internal/notes"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
	"github.com/dividebyzero/claude-experiments/varvig/internal/reserved"
)

// seedWithNotes returns a server carrying two notes on its tip change: one in a
// private namespace and one in a reserved governance namespace.
func seedWithNotes(t *testing.T) (*repo.Repo, string, string) {
	t.Helper()
	server, _, tip := seedServer(t)
	store := notes.New(server)
	if _, err := store.Add("myteam/scratch", tip, []byte("scratch"), "a", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Add(reserved.NoteScope, tip, []byte("scope"), "a", 1); err != nil {
		t.Fatal(err)
	}
	scratchRef := "refs/notes/myteam/scratch/" + tip.Hex()
	scopeRef := "refs/notes/" + reserved.NoteScope + "/" + tip.Hex()
	return server, scratchRef, scopeRef
}

func TestNotesReplicateOnFetchByDefault(t *testing.T) {
	server, scratchRef, scopeRef := seedWithNotes(t)
	client := dialServe(t, server)

	dst, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := ReplicateNotesFetch(client, dst, nil); err != nil {
		t.Fatalf("ReplicateNotesFetch: %v", err)
	}
	for _, ref := range []string{scratchRef, scopeRef} {
		if _, err := dst.Refs.Resolve(ref); err != nil {
			t.Errorf("note ref %s did not replicate: %v", ref, err)
		}
	}
}

// TestNotesLoudFailureOnDrop covers §7.7: between two notes-sync peers, a note
// that cannot be fully transferred is an error, not a silent partial.
func TestNotesLoudFailureOnDrop(t *testing.T) {
	server, scratchRef, _ := seedWithNotes(t)
	// Simulate a dropped note: the ref is still advertised, but its object is
	// gone from the server store, so its closure cannot be transferred.
	tip, err := server.Refs.Resolve(scratchRef)
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Objects.Delete(tip); err != nil {
		t.Fatal(err)
	}
	client := dialServe(t, server)
	dst, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	err = ReplicateNotesFetch(client, dst, nil)
	if err == nil {
		t.Fatal("a note that failed to transfer must be a loud error, not a silent skip")
	}
	if !strings.Contains(err.Error(), "notes") {
		t.Fatalf("error should be about notes, got: %v", err)
	}
}

// TestNotesOptOutRespectsReserved covers §4: a non-reserved namespace can be
// opted out, but a reserved governance namespace always replicates.
func TestNotesOptOutRespectsReserved(t *testing.T) {
	server, scratchRef, scopeRef := seedWithNotes(t)
	client := dialServe(t, server)

	dst, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Opt out of both namespaces; only the non-reserved one may actually skip.
	optOut := func(ns string) bool { return true }
	if err := ReplicateNotesFetch(client, dst, optOut); err != nil {
		t.Fatalf("ReplicateNotesFetch: %v", err)
	}
	if _, err := dst.Refs.Resolve(scratchRef); err == nil {
		t.Error("opted-out private namespace should not have replicated")
	}
	if _, err := dst.Refs.Resolve(scopeRef); err != nil {
		t.Errorf("reserved namespace must always replicate, even when opted out: %v", err)
	}
}

// TestNotesRequireCapability asserts the replication helpers refuse to pretend
// notes synced when the peer does not advertise the bit.
func TestNotesRequireCapability(t *testing.T) {
	server, _, _ := seedWithNotes(t)
	client := dialServe(t, server)
	client.caps = map[string]bool{} // partner without notes-sync
	dst, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := ReplicateNotesFetch(client, dst, nil); err == nil {
		t.Fatal("notes replication must refuse when the peer lacks notes-sync")
	}
}
