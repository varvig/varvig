package readapi

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestHTTPRefsJSON(t *testing.T) {
	f := newFixture(t)
	srv := httptest.NewServer(Handler(f.q))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/refs")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var refs []RefInfo
	if err := json.NewDecoder(resp.Body).Decode(&refs); err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0].Name != "refs/heads/main" {
		t.Fatalf("refs=%+v", refs)
	}
}

func TestHTTPBranchRedirectsToHash(t *testing.T) {
	f := newFixture(t)
	srv := httptest.NewServer(Handler(f.q))
	defer srv.Close()

	// Do not follow redirects: we want to observe the 302 to the permalink.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Get(srv.URL + "/change/main")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("expected 302, got %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/change/"+f.childID.Hex() {
		t.Fatalf("redirect location=%q", loc)
	}
}

func TestHTTPChangeJSONIntentFirst(t *testing.T) {
	f := newFixture(t)
	srv := httptest.NewServer(Handler(f.q))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/change/" + f.childID.Hex())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var view ChangeView
	if err := json.NewDecoder(resp.Body).Decode(&view); err != nil {
		t.Fatal(err)
	}
	if view.Intent != "add new.txt" {
		t.Fatalf("intent=%q", view.Intent)
	}
	// Immutable, hash-addressed responses are cacheable forever.
	if cc := resp.Header.Get("Cache-Control"); cc == "" {
		t.Fatal("expected Cache-Control on a hash-addressed response")
	}
}

func TestHTTPContentNegotiationHTML(t *testing.T) {
	f := newFixture(t)
	srv := httptest.NewServer(Handler(f.q))
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/change/"+f.childID.Hex(), nil)
	req.Header.Set("Accept", "text/html")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct[:9] != "text/html" {
		t.Fatalf("content-type=%q", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	// Intent must appear before the diff in the HTML (auth design §7.3).
	if idxIntent, idxDiff := indexOf(body, "intent"), indexOf(body, "diff"); idxIntent < 0 || idxIntent > idxDiff {
		t.Fatalf("intent must precede diff (intent=%d diff=%d)", idxIntent, idxDiff)
	}
}

func TestHTTPBlobRaw(t *testing.T) {
	f := newFixture(t)
	srv := httptest.NewServer(Handler(f.q))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/blob/" + f.blobID.Hex())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "hello\n" {
		t.Fatalf("blob body=%q", body)
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("blob should be served with nosniff")
	}
}

func TestHTTPNotFound(t *testing.T) {
	f := newFixture(t)
	srv := httptest.NewServer(Handler(f.q))
	defer srv.Close()

	// A well-formed but absent hash is 404.
	resp, err := http.Get(srv.URL + "/o/1e20" + "00000000000000000000000000000000000000000000000000000000000000")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestHTTPOriginGuard(t *testing.T) {
	f := newFixture(t)
	srv := httptest.NewServer(Handler(f.q))
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/refs", nil)
	req.Header.Set("Origin", "http://evil.example.com")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin request should be 403, got %d", resp.StatusCode)
	}
}

// TestServeUnixSocket exercises the real Unix-socket transport end to end.
func TestServeUnixSocket(t *testing.T) {
	f := newFixture(t)
	sock := filepath.Join(t.TempDir(), "read.sock")
	ln, err := ListenUnix(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go Serve(f.q, ln)

	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
	}}
	resp, err := client.Get("http://unix/refs")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var refs []RefInfo
	if err := json.NewDecoder(resp.Body).Decode(&refs); err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 {
		t.Fatalf("refs over unix socket: %+v", refs)
	}
}

func indexOf(b []byte, sub string) int {
	s, t := string(b), sub
	for i := 0; i+len(t) <= len(s); i++ {
		if s[i:i+len(t)] == t {
			return i
		}
	}
	return -1
}
