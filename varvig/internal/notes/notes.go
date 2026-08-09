// Package notes attaches metadata to an object without changing that object's
// bytes or identity (design §2). A note is an immutable, content-addressed
// object; the (namespace, target) pair maps to the head of a note chain via a
// ref under refs/notes/, so attaching a note never touches the target and the
// full accretion history is preserved and syncs like any other object.
//
// One ref per (namespace, target) is simple and correct. A fan-out note tree
// (as Git uses) is purely an on-disk scaling optimization (design §4.3) and can
// be layered on later without changing note objects.
package notes

import (
	"fmt"
	"strings"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
)

// Store manages notes for a repository.
type Store struct{ r *repo.Repo }

// New returns a note store over r.
func New(r *repo.Repo) *Store { return &Store{r: r} }

// Entry is a note plus its own object id.
type Entry struct {
	ID   multihash.Multihash
	Note object.Note
}

func refName(namespace string, target multihash.Multihash) (string, error) {
	if err := validNamespace(namespace); err != nil {
		return "", err
	}
	return "refs/notes/" + namespace + "/" + target.Hex(), nil
}

// validNamespace accepts a slash-separated namespace of non-empty segments, so
// hierarchical namespaces such as the reserved governance spaces
// "varvig/attest", "varvig/external", and "varvig/score" (tickets §1.3) are
// expressible. It rejects leading/trailing slashes, path traversal, and
// whitespace or control characters that would not map safely to a ref path.
func validNamespace(namespace string) error {
	if namespace == "" {
		return fmt.Errorf("notes: invalid namespace %q: empty", namespace)
	}
	if strings.HasPrefix(namespace, "/") || strings.HasSuffix(namespace, "/") {
		return fmt.Errorf("notes: invalid namespace %q: leading/trailing slash", namespace)
	}
	for _, seg := range strings.Split(namespace, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("notes: invalid namespace %q: bad segment %q", namespace, seg)
		}
		if strings.ContainsAny(seg, "\\ \t\x00") {
			return fmt.Errorf("notes: invalid namespace %q: illegal character", namespace)
		}
	}
	return nil
}

// Add attaches a note to target under namespace, chaining onto any existing
// note for that pair. It returns the new note's id. The compare-and-swap makes
// concurrent additions safe: a racing writer retries against the new head.
func (s *Store) Add(namespace string, target multihash.Multihash, payload []byte, author string, now int64) (multihash.Multihash, error) {
	name, err := refName(namespace, target)
	if err != nil {
		return nil, err
	}
	var parent multihash.Multihash
	if cur, err := s.r.Refs.Resolve(name); err == nil {
		parent = cur
	}
	note := object.NewNote(object.Note{
		Target:    target,
		Namespace: namespace,
		Payload:   payload,
		Parent:    parent,
		Timestamp: now,
		Author:    author,
	})
	id, err := s.r.Objects.Put(note)
	if err != nil {
		return nil, err
	}
	if err := s.r.Refs.CompareAndSwap(name, parent, id, author, "note:"+namespace); err != nil {
		return nil, err
	}
	return id, nil
}

// List returns the note chain for (namespace, target), newest first.
func (s *Store) List(namespace string, target multihash.Multihash) ([]Entry, error) {
	name, err := refName(namespace, target)
	if err != nil {
		return nil, err
	}
	head, err := s.r.Refs.Resolve(name)
	if err != nil {
		return nil, nil // no notes
	}
	var out []Entry
	id := head
	for id != nil {
		obj, err := s.r.Objects.Get(id)
		if err != nil {
			return nil, err
		}
		n, err := obj.AsNote()
		if err != nil {
			return nil, err
		}
		out = append(out, Entry{ID: id, Note: n})
		id = n.Parent
	}
	return out, nil
}

// Namespaces returns the note namespaces that have at least one note for target.
func (s *Store) Namespaces(target multihash.Multihash) ([]string, error) {
	all, err := s.r.Refs.List()
	if err != nil {
		return nil, err
	}
	suffix := "/" + target.Hex()
	prefix := "refs/notes/"
	var out []string
	for _, n := range all {
		if strings.HasPrefix(n, prefix) && strings.HasSuffix(n, suffix) {
			ns := strings.TrimSuffix(strings.TrimPrefix(n, prefix), suffix)
			out = append(out, ns)
		}
	}
	return out, nil
}
