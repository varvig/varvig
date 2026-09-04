package textdiff

import "testing"

func TestUnifiedModify(t *testing.T) {
	a := []byte("line one\nline two\nline three\n")
	b := []byte("line one\nline TWO\nline three\n")
	out, empty := Unified(a, b, "a/x", "b/x", DefaultContext)
	if empty {
		t.Fatal("changed inputs reported empty")
	}
	for _, want := range []string{"--- a/x", "+++ b/x", "@@ -1,3 +1,3 @@", " line one", "-line two", "+line TWO", " line three"} {
		if !contains(out, want) {
			t.Errorf("unified diff missing %q:\n%s", want, out)
		}
	}
}

func TestUnifiedEmptyWhenEqual(t *testing.T) {
	if _, empty := Unified([]byte("same\n"), []byte("same\n"), "a", "b", 3); !empty {
		t.Fatal("identical inputs should be empty")
	}
}

func TestUnifiedNoTrailingNewline(t *testing.T) {
	out, _ := Unified([]byte("x"), []byte("x\n"), "a", "b", 3)
	if !contains(out, "\\ No newline at end of file") {
		t.Fatalf("missing no-newline marker:\n%s", out)
	}
}

func TestUnifiedAddAndDelete(t *testing.T) {
	add, _ := Unified(nil, []byte("new\n"), "/dev/null", "b/x", 3)
	if !contains(add, "@@ -0,0 +1,1 @@") || !contains(add, "+new") {
		t.Fatalf("add diff wrong:\n%s", add)
	}
	del, _ := Unified([]byte("gone\n"), nil, "a/x", "/dev/null", 3)
	if !contains(del, "@@ -1,1 +0,0 @@") || !contains(del, "-gone") {
		t.Fatalf("delete diff wrong:\n%s", del)
	}
}

func TestStat(t *testing.T) {
	added, removed := Stat([]byte("a\nb\nc\n"), []byte("a\nB\nc\nd\n"))
	if added != 2 || removed != 1 {
		t.Fatalf("Stat = +%d -%d, want +2 -1", added, removed)
	}
}

func TestIsBinary(t *testing.T) {
	if !IsBinary([]byte("abc\x00def")) {
		t.Error("NUL byte should be binary")
	}
	if IsBinary([]byte("plain text\n")) {
		t.Error("text misreported as binary")
	}
}

func contains(hay, needle string) bool {
	return len(needle) == 0 || indexOf(hay, needle) >= 0
}

func indexOf(hay, needle string) int {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
