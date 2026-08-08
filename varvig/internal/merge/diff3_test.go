package merge

import (
	"strings"
	"testing"
)

func lns(s string) []string {
	if s == "" {
		return nil
	}
	return strings.SplitAfter(s, "\n")
}

func TestMerge3OneSideOnly(t *testing.T) {
	base := lns("a\nb\nc\n")
	ours := lns("a\nb\nc\n")   // unchanged
	theirs := lns("a\nB\nc\n") // changed line 2
	got, conflict := merge3(base, ours, theirs)
	if conflict {
		t.Fatal("unexpected conflict")
	}
	if strings.Join(got, "") != "a\nB\nc\n" {
		t.Fatalf("got %q", strings.Join(got, ""))
	}
}

func TestMerge3NonOverlapping(t *testing.T) {
	base := lns("a\nb\nc\n")
	ours := lns("A\nb\nc\n")   // change first line
	theirs := lns("a\nb\nC\n") // change last line
	got, conflict := merge3(base, ours, theirs)
	if conflict {
		t.Fatal("unexpected conflict for non-overlapping edits")
	}
	if strings.Join(got, "") != "A\nb\nC\n" {
		t.Fatalf("got %q, want A\\nb\\nC\\n", strings.Join(got, ""))
	}
}

func TestMerge3Overlapping(t *testing.T) {
	base := lns("a\nb\nc\n")
	ours := lns("a\nX\nc\n")
	theirs := lns("a\nY\nc\n")
	got, conflict := merge3(base, ours, theirs)
	if !conflict {
		t.Fatal("expected conflict for overlapping edits")
	}
	joined := strings.Join(got, "")
	if !strings.Contains(joined, "X\n") || !strings.Contains(joined, "Y\n") {
		t.Fatalf("conflict output missing a side: %q", joined)
	}
	if !strings.Contains(joined, markerOurs) || !strings.Contains(joined, markerThrs) {
		t.Fatalf("missing conflict markers: %q", joined)
	}
}

func TestMerge3BothSameEdit(t *testing.T) {
	base := lns("a\nb\n")
	ours := lns("a\nZ\n")
	theirs := lns("a\nZ\n")
	got, conflict := merge3(base, ours, theirs)
	if conflict {
		t.Fatal("identical edits should not conflict")
	}
	if strings.Join(got, "") != "a\nZ\n" {
		t.Fatalf("got %q", strings.Join(got, ""))
	}
}

func TestMerge3Insertions(t *testing.T) {
	base := lns("a\nc\n")
	ours := lns("a\nb\nc\n")   // insert b
	theirs := lns("a\nc\nd\n") // append d
	got, conflict := merge3(base, ours, theirs)
	if conflict {
		t.Fatalf("unexpected conflict: %q", strings.Join(got, ""))
	}
	if strings.Join(got, "") != "a\nb\nc\nd\n" {
		t.Fatalf("got %q", strings.Join(got, ""))
	}
}
