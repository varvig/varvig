package ticket

import (
	"testing"
)

// TestCommentsRoundTripAndOrder covers §5.2: comments accrete on a ticket, are
// returned oldest-first, and carry the origin tags a connector needs for echo
// suppression — all without touching the ticket's intent head.
func TestCommentsRoundTripAndOrder(t *testing.T) {
	r := newRepo(t)
	k := key(t)
	id, err := New(r, "spec", k, "director", 1)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := AddComment(r, id, Comment{Author: "ext:alice", Body: "second", Origin: "ext", OriginID: "222"}, 20); err != nil {
		t.Fatalf("AddComment: %v", err)
	}
	if err := AddComment(r, id, Comment{Author: "agent", Body: "first"}, 10); err != nil {
		t.Fatalf("AddComment: %v", err)
	}

	cs, err := Comments(r, id)
	if err != nil {
		t.Fatalf("Comments: %v", err)
	}
	if len(cs) != 2 {
		t.Fatalf("comments = %d, want 2", len(cs))
	}
	// Oldest first, by timestamp, regardless of insertion order.
	if cs[0].Body != "first" || cs[1].Body != "second" {
		t.Fatalf("order = [%q %q], want [first second]", cs[0].Body, cs[1].Body)
	}
	if cs[1].Origin != "ext" || cs[1].OriginID != "222" {
		t.Fatalf("origin tags lost: %+v", cs[1])
	}
	// Comments do not disturb the ticket head.
	if head, _ := Head(r, id); !head.Equal(id) {
		t.Fatalf("commenting moved the head: %s", head.Hex())
	}
}

// TestAddCommentDefaultsTimestamp: a zero timestamp defaults to now.
func TestAddCommentDefaultsTimestamp(t *testing.T) {
	r := newRepo(t)
	k := key(t)
	id, _ := New(r, "spec", k, "director", 1)
	if err := AddComment(r, id, Comment{Author: "a", Body: "b"}, 42); err != nil {
		t.Fatalf("AddComment: %v", err)
	}
	cs, _ := Comments(r, id)
	if len(cs) != 1 || cs[0].Timestamp != 42 {
		t.Fatalf("timestamp = %v, want 42", cs)
	}
}
