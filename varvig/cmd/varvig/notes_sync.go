package main

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/dividebyzero/claude-experiments/varvig/internal/p2p"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
	"github.com/dividebyzero/claude-experiments/varvig/internal/wire"
)

// notesOptOutFile is a tracked, per-repo list of note namespaces to exclude
// from replication — one namespace per line, '#' comments allowed. It follows
// the trust-store convention (a line-oriented file under the versioned
// .varvig.d/ directory). Reserved governance namespaces are never opted out
// regardless of this file (federation §4).
const notesOptOutFile = ".varvig.d/notes-sync.optout"

// loadNotesOptOut returns a predicate reporting whether a namespace is opted out
// of replication. Absent file ⇒ everything replicates (the federation default).
func loadNotesOptOut(r *repo.Repo) func(ns string) bool {
	data, err := os.ReadFile(filepath.Join(r.Root(), notesOptOutFile))
	if err != nil {
		return func(string) bool { return false }
	}
	out := map[string]bool{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out[line] = true
	}
	return func(ns string) bool { return out[ns] }
}

// syncNotes replicates notes after a head sync, in the given direction, but only
// when the peer advertises notes-sync. When it does, a transfer failure is
// surfaced (federation §4: loud failure, never a silent partial). When it does
// not, notes are simply not replicated and that is not an error.
func syncNotes(client *p2p.Client, r *repo.Repo, push bool) error {
	if !client.Caps()[wire.CapNotesSync] {
		return nil
	}
	optOut := loadNotesOptOut(r)
	if push {
		if err := p2p.ReplicateNotesPush(client, r, optOut); err != nil {
			return err
		}
		fmt.Println("notes: replicated to peer")
		return nil
	}
	if err := p2p.ReplicateNotesFetch(client, r, optOut); err != nil {
		return err
	}
	fmt.Println("notes: replicated from peer")
	return nil
}
