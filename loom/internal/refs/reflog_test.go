package refs

import (
	"testing"
	"time"
)

func TestReflogRecordsEveryMove(t *testing.T) {
	s := newStore(t)
	// Deterministic, monotonic clock for stable timestamps.
	var tick int64
	s.SetClock(func() time.Time {
		tick++
		return time.Unix(0, tick)
	})

	a, b := id(t, "a"), id(t, "b")
	if err := s.Create("refs/heads/main", a, "agent-1", "create"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.CompareAndSwap("refs/heads/main", a, b, "agent-2", "advance"); err != nil {
		t.Fatalf("CAS: %v", err)
	}
	if err := s.Delete("refs/heads/main", b, "agent-3", "cleanup"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	entries, err := s.ReadLog("refs/heads/main")
	if err != nil {
		t.Fatalf("ReadLog: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3", len(entries))
	}

	// Creation: no old value, new = a.
	if entries[0].Old != nil || !entries[0].New.Equal(a) || entries[0].Actor != "agent-1" {
		t.Fatalf("entry 0 = %+v", entries[0])
	}
	// Advance: old = a, new = b.
	if !entries[1].Old.Equal(a) || !entries[1].New.Equal(b) || entries[1].Message != "advance" {
		t.Fatalf("entry 1 = %+v", entries[1])
	}
	// Deletion: old = b, no new value — the state is still recoverable from here.
	if !entries[2].Old.Equal(b) || entries[2].New != nil || entries[2].Actor != "agent-3" {
		t.Fatalf("entry 2 = %+v", entries[2])
	}
}

func TestReflogSurvivesMessagesWithSpaces(t *testing.T) {
	s := newStore(t)
	a := id(t, "a")
	msg := "regenerated against new base after conflict"
	if err := s.Create("refs/heads/x", a, "agent name with spaces", msg); err != nil {
		t.Fatalf("Create: %v", err)
	}
	entries, err := s.ReadLog("refs/heads/x")
	if err != nil {
		t.Fatalf("ReadLog: %v", err)
	}
	if len(entries) != 1 || entries[0].Message != msg {
		t.Fatalf("message not preserved: %+v", entries)
	}
	if entries[0].Actor != "agent name with spaces" {
		t.Fatalf("actor not preserved: %q", entries[0].Actor)
	}
}

func TestReadLogMissingRef(t *testing.T) {
	s := newStore(t)
	entries, err := s.ReadLog("refs/heads/nope")
	if err != nil {
		t.Fatalf("ReadLog: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("entries = %v, want empty", entries)
	}
}
