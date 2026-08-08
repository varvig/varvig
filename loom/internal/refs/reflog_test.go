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

func TestExpireLogByCount(t *testing.T) {
	s := newStore(t)
	var tick int64
	s.SetClock(func() time.Time { tick++; return time.Unix(0, tick) })

	name := "refs/heads/main"
	prev := id(t, "0")
	if err := s.Create(name, prev, "a", "create"); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < 6; i++ {
		next := id(t, string(rune('a'+i)))
		if err := s.CompareAndSwap(name, prev, next, "a", "move"); err != nil {
			t.Fatal(err)
		}
		prev = next
	}
	// 6 entries total; keep the last 2 by count, with the age window disabled
	// (cutoff above all timestamps).
	removed, err := s.ExpireLog(name, 2, 1<<62)
	if err != nil {
		t.Fatalf("ExpireLog: %v", err)
	}
	if removed != 4 {
		t.Fatalf("removed = %d, want 4", removed)
	}
	entries, _ := s.ReadLog(name)
	if len(entries) != 2 {
		t.Fatalf("remaining = %d, want 2", len(entries))
	}
	// The retained entries are the most recent ones (highest timestamps).
	if entries[len(entries)-1].New == nil || !entries[len(entries)-1].New.Equal(prev) {
		t.Fatal("most recent entry not retained")
	}
}

func TestExpireLogByAge(t *testing.T) {
	s := newStore(t)
	var tick int64
	s.SetClock(func() time.Time { tick++; return time.Unix(0, tick*1000) })

	name := "refs/heads/x"
	prev := id(t, "0")
	_ = s.Create(name, prev, "a", "c")
	for i := 1; i < 5; i++ {
		next := id(t, string(rune('a'+i)))
		_ = s.CompareAndSwap(name, prev, next, "a", "m")
		prev = next
	}
	// Entries have timestamps 1000..5000ns. Keep only those at/after 4000,
	// with no count floor.
	removed, err := s.ExpireLog(name, 0, 4000)
	if err != nil {
		t.Fatalf("ExpireLog: %v", err)
	}
	if removed != 3 {
		t.Fatalf("removed = %d, want 3 (kept 4000,5000)", removed)
	}
	entries, _ := s.ReadLog(name)
	for _, e := range entries {
		if e.UnixNS < 4000 {
			t.Fatalf("entry older than cutoff survived: %d", e.UnixNS)
		}
	}
}

func TestExpireAllEmptiesLog(t *testing.T) {
	s := newStore(t)
	var tick int64
	s.SetClock(func() time.Time { tick++; return time.Unix(0, tick) })
	_ = s.Create("refs/heads/gone", id(t, "a"), "u", "c")
	// keepMax 0 and a cutoff in the future removes everything.
	removed, err := s.ExpireAll(0, 1<<62)
	if err != nil {
		t.Fatalf("ExpireAll: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	entries, _ := s.ReadLog("refs/heads/gone")
	if len(entries) != 0 {
		t.Fatalf("log not emptied: %v", entries)
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
