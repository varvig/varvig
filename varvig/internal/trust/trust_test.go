package trust

import (
	"bytes"
	"testing"
)

const sample = `# fingerprint                     name    scope        rights
SHA256:aXk9Lm4Qr                  jan     /            promote
SHA256:bR7tYu2Pl                  mira    /            promote
SHA256:cW3nEf8Zx                  ci-01   /            propose

# sam only owns the web subtree
SHA256:dK1oIu5Vb                  sam     src/web/     promote future-column extra
`

func TestParseRoundTrip(t *testing.T) {
	f := Parse([]byte(sample))
	if got := f.Bytes(); !bytes.Equal(got, []byte(sample)) {
		t.Fatalf("round-trip changed bytes:\n--- got ---\n%s\n--- want ---\n%s", got, sample)
	}
}

func TestRoundTripNoFinalNewline(t *testing.T) {
	in := "SHA256:x jan / promote"
	f := Parse([]byte(in))
	if got := string(f.Bytes()); got != in {
		t.Fatalf("no-final-newline round-trip: got %q want %q", got, in)
	}
}

func TestRecordsAndExtra(t *testing.T) {
	f := Parse([]byte(sample))
	recs := f.Records()
	if len(recs) != 4 {
		t.Fatalf("want 4 records, got %d", len(recs))
	}
	sam := recs[3]
	if sam.Name != "sam" || sam.Scope != "src/web/" || sam.Right != RightPromote {
		t.Fatalf("sam parsed wrong: %+v", sam)
	}
	if len(sam.Extra) != 2 || sam.Extra[0] != "future-column" || sam.Extra[1] != "extra" {
		t.Fatalf("unknown columns not preserved: %v", sam.Extra)
	}
}

func TestRightsOrdering(t *testing.T) {
	if !RightPromote.Allows(RightPropose) || !RightPromote.Allows(RightRead) {
		t.Fatal("promote should imply propose and read")
	}
	if RightPropose.Allows(RightPromote) {
		t.Fatal("propose must not imply promote")
	}
	if RightUnknown.Allows(RightRead) || RightRead.Allows(RightUnknown) {
		t.Fatal("unknown right must neither grant nor be satisfiable")
	}
}

func TestScopeCovers(t *testing.T) {
	cases := []struct {
		scope, target string
		want          bool
	}{
		{"/", "anything/at/all", true},
		{"/", "/", true},
		{"src/web/", "src/web/", true},
		{"src/web/", "src/web/app.js", true},
		{"src/web/", "src/webapp/", false}, // must not match across a boundary
		{"src/web/", "src/", false},
		{"src/web/", "/", false}, // whole repo is not inside a narrower scope
	}
	for _, c := range cases {
		if got := NormalizeScope(c.scope).Covers(c.target); got != c.want {
			t.Errorf("Scope(%q).Covers(%q) = %v, want %v", c.scope, c.target, got, c.want)
		}
	}
}

func TestAuthorized(t *testing.T) {
	f := Parse([]byte(sample))
	// jan holds promote at root: authorized everywhere for anything.
	if !f.Authorized("SHA256:aXk9Lm4Qr", RightPromote, "refs/heads/main") {
		t.Fatal("jan should be able to promote at root")
	}
	// ci-01 holds only propose: not authorized to promote.
	if f.Authorized("SHA256:cW3nEf8Zx", RightPromote, "refs/heads/main") {
		t.Fatal("ci-01 must not be able to promote")
	}
	if !f.Authorized("SHA256:cW3nEf8Zx", RightPropose, "refs/heads/main") {
		t.Fatal("ci-01 should be able to propose")
	}
	// sam holds promote only within src/web/.
	if !f.Authorized("SHA256:dK1oIu5Vb", RightPromote, "src/web/index.html") {
		t.Fatal("sam should promote within scope")
	}
	if f.Authorized("SHA256:dK1oIu5Vb", RightPromote, "src/api/handler.go") {
		t.Fatal("sam must not promote outside scope")
	}
	// An unknown fingerprint is never authorized.
	if f.Authorized("SHA256:nobody", RightRead, "/") {
		t.Fatal("unknown principal must not be authorized")
	}
}

func TestUnknownRightsLinePreserved(t *testing.T) {
	in := "SHA256:z newbie / superuser\n"
	f := Parse([]byte(in))
	if len(f.Records()) != 0 {
		t.Fatal("line with unknown rights token must not parse as a record")
	}
	if got := string(f.Bytes()); got != in {
		t.Fatalf("unknown-rights line not preserved: got %q", got)
	}
}

func TestAddAppends(t *testing.T) {
	f := Parse([]byte(sample))
	f.Add(Record{Fingerprint: "SHA256:new", Name: "kai", Scope: NormalizeScope("/"), Right: RightPropose})
	recs := f.Records()
	if len(recs) != 5 {
		t.Fatalf("want 5 records after add, got %d", len(recs))
	}
	// The original block must be untouched, the new line appended canonically.
	out := string(f.Bytes())
	if !bytes.HasPrefix([]byte(out), []byte(sample)) {
		t.Fatalf("Add disturbed existing content:\n%s", out)
	}
	reparsed := Parse([]byte(out))
	if !reparsed.Authorized("SHA256:new", RightPropose, "/") {
		t.Fatal("added principal not authorized after reparse")
	}
}

func TestRemove(t *testing.T) {
	f := Parse([]byte(sample))
	if n := f.Remove("SHA256:cW3nEf8Zx"); n != 1 {
		t.Fatalf("removed %d, want 1", n)
	}
	if len(f.Lookup("SHA256:cW3nEf8Zx")) != 0 {
		t.Fatal("ci-01 should be gone")
	}
	// Comments and other records survive.
	if len(f.Records()) != 3 {
		t.Fatalf("want 3 records after remove, got %d", len(f.Records()))
	}
}
