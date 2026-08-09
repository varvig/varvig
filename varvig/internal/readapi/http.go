package readapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
)

// Handler builds the read-only HTTP surface over the query layer. The same
// routes serve JSON and HTML by content negotiation (auth design §7.3); every
// route is hash-addressed, and a branch name is redirected to its current hash
// so every served page is an immutable permalink.
//
// Routes:
//
//	GET /o/{hash}                 object metadata
//	GET /tree/{hash}[/{path...}]  directory listing
//	GET /blob/{hash}              file content
//	GET /change/{hash}            intent, then evidence, then diff
//	GET /log/{ref}?limit=N        change list
//	GET /refs                     ref -> hash
//	GET /proposals?scope=…        unpromoted speculative states
func Handler(q *Query) http.Handler {
	h := &handler{q: q}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /o/{hash}", h.object)
	mux.HandleFunc("GET /tree/{hash}", h.tree)
	mux.HandleFunc("GET /tree/{hash}/{path...}", h.tree)
	mux.HandleFunc("GET /blob/{hash}", h.blob)
	mux.HandleFunc("GET /change/{hash}", h.change)
	mux.HandleFunc("GET /log/{ref}", h.log)
	mux.HandleFunc("GET /refs", h.refs)
	mux.HandleFunc("GET /proposals", h.proposals)
	mux.HandleFunc("GET /", h.index)
	return originGuard(mux)
}

type handler struct{ q *Query }

func (h *handler) index(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		h.fail(w, r, http.StatusNotFound, errors.New("not found"))
		return
	}
	refs, err := h.q.Refs()
	if err != nil {
		h.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	if wantsHTML(r) {
		var b strings.Builder
		b.WriteString("<h1>varvig</h1><h2>refs</h2><ul>")
		for _, ref := range refs {
			fmt.Fprintf(&b, `<li><a href="/change/%s">%s</a> → %s</li>`, ref.Hash, htmlEscape(ref.Name), ref.Hash)
		}
		b.WriteString(`</ul><p><a href="/proposals">proposals</a></p>`)
		h.html(w, b.String())
		return
	}
	h.json(w, map[string]any{"service": "varvig read-only", "refs": refs})
}

func (h *handler) object(w http.ResponseWriter, r *http.Request) {
	id, ok := h.resolve(w, r, r.PathValue("hash"), func(hash string) string { return "/o/" + hash })
	if !ok {
		return
	}
	info, err := h.q.Object(id)
	if err != nil {
		h.failLookup(w, r, err)
		return
	}
	h.json(w, info) // metadata is inherently structured; JSON regardless of Accept
}

func (h *handler) tree(w http.ResponseWriter, r *http.Request) {
	path := r.PathValue("path")
	id, ok := h.resolve(w, r, r.PathValue("hash"), func(hash string) string {
		if path == "" {
			return "/tree/" + hash
		}
		return "/tree/" + hash + "/" + path
	})
	if !ok {
		return
	}
	listing, err := h.q.Tree(id, path)
	if err != nil {
		h.failLookup(w, r, err)
		return
	}
	if wantsHTML(r) {
		var b strings.Builder
		fmt.Fprintf(&b, "<h1>tree %s</h1><p>/%s</p><ul>", listing.Hash, htmlEscape(listing.Path))
		for _, e := range listing.Entries {
			href := "/blob/" + e.Hash
			if e.Kind == "tree" {
				href = "/tree/" + e.Hash
			}
			fmt.Fprintf(&b, `<li><a href="%s">%s</a> (%s)</li>`, href, htmlEscape(e.Name), e.Kind)
		}
		b.WriteString("</ul>")
		h.html(w, b.String())
		return
	}
	h.json(w, listing)
}

func (h *handler) blob(w http.ResponseWriter, r *http.Request) {
	id, ok := h.resolve(w, r, r.PathValue("hash"), func(hash string) string { return "/blob/" + hash })
	if !ok {
		return
	}
	content, err := h.q.Blob(id)
	if err != nil {
		h.failLookup(w, r, err)
		return
	}
	// Content is bytes; serve as octet-stream so a browser never executes it.
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	cacheImmutable(w)
	_, _ = w.Write(content)
}

func (h *handler) change(w http.ResponseWriter, r *http.Request) {
	id, ok := h.resolve(w, r, r.PathValue("hash"), func(hash string) string { return "/change/" + hash })
	if !ok {
		return
	}
	view, err := h.q.Change(id)
	if err != nil {
		h.failLookup(w, r, err)
		return
	}
	if wantsHTML(r) {
		var b strings.Builder
		// Intent first — deliberately, not the diff (auth design §7.3).
		fmt.Fprintf(&b, "<h1>change %s</h1><h2>intent</h2><p>%s</p>", view.Hash, htmlEscape(view.Intent))
		if view.Evidence != nil {
			fmt.Fprintf(&b, "<h2>evidence</h2><pre>%s</pre>", htmlEscape(fmt.Sprintf("%+v", *view.Evidence)))
		}
		b.WriteString("<h2>diff</h2><ul>")
		for _, p := range view.ChangedAdd {
			fmt.Fprintf(&b, "<li>+ %s</li>", htmlEscape(p))
		}
		for _, p := range view.ChangedMod {
			fmt.Fprintf(&b, "<li>~ %s</li>", htmlEscape(p))
		}
		for _, p := range view.ChangedDel {
			fmt.Fprintf(&b, "<li>- %s</li>", htmlEscape(p))
		}
		b.WriteString("</ul>")
		h.html(w, b.String())
		return
	}
	h.json(w, view)
}

func (h *handler) log(w http.ResponseWriter, r *http.Request) {
	// A ref or a hash; a branch name resolves to a hash but the list itself is
	// not a single immutable state, so we do not redirect — we start the walk.
	id, err := h.q.Resolve(r.PathValue("ref"))
	if err != nil {
		h.fail(w, r, http.StatusNotFound, err)
		return
	}
	limit := 0
	if s := r.URL.Query().Get("limit"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n >= 0 {
			limit = n
		}
	}
	entries, err := h.q.Log(id, limit)
	if err != nil {
		h.failLookup(w, r, err)
		return
	}
	if wantsHTML(r) {
		var b strings.Builder
		b.WriteString("<h1>log</h1><ul>")
		for _, e := range entries {
			fmt.Fprintf(&b, `<li><a href="/change/%s">%s</a> — %s</li>`, e.Hash, e.Hash[:16], htmlEscape(e.Intent))
		}
		b.WriteString("</ul>")
		h.html(w, b.String())
		return
	}
	h.json(w, entries)
}

func (h *handler) refs(w http.ResponseWriter, r *http.Request) {
	refs, err := h.q.Refs()
	if err != nil {
		h.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	h.json(w, refs)
}

func (h *handler) proposals(w http.ResponseWriter, r *http.Request) {
	props, err := h.q.Proposals(r.URL.Query().Get("scope"))
	if err != nil {
		h.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	h.json(w, props)
}

// resolve returns the identity named by seg. If seg is a hash it is used
// directly; if it is a branch name it is resolved and the request is redirected
// to the hash-addressed URL (so every served page is an immutable permalink).
// It returns ok=false when it has already written a response (redirect or 404).
func (h *handler) resolve(w http.ResponseWriter, r *http.Request, seg string, redirectTo func(hash string) string) (multihash.Multihash, bool) {
	if id, err := multihash.ParseHex(seg); err == nil {
		return id, true
	}
	id, err := h.q.Resolve(seg)
	if err != nil {
		h.fail(w, r, http.StatusNotFound, err)
		return nil, false
	}
	http.Redirect(w, r, redirectTo(id.Hex()), http.StatusFound)
	return nil, false
}

func (h *handler) failLookup(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, ErrNotFound) {
		h.fail(w, r, http.StatusNotFound, err)
		return
	}
	h.fail(w, r, http.StatusInternalServerError, err)
}

func (h *handler) json(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	cacheImmutable(w)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func (h *handler) html(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte("<!doctype html><meta charset=utf-8><title>varvig</title>" + body))
}

func (h *handler) fail(w http.ResponseWriter, r *http.Request, code int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

// cacheImmutable marks hash-addressed responses cacheable forever: a view names
// an immutable state, so it can never change (auth design §7.3).
func cacheImmutable(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
}

// wantsHTML reports whether the client prefers HTML (a browser). The default is
// JSON: the machine API is the primary consumer.
func wantsHTML(r *http.Request) bool {
	accept := r.Header.Get("Accept")
	return strings.Contains(accept, "text/html")
}

// originGuard is the browser-hop defense (auth design §7.5): any website the
// user visits can issue requests to a localhost server, so a cross-origin
// browser request (one carrying an Origin whose host is not our Host) is
// refused. Non-browser clients send no Origin and are unaffected. Full
// DNS-rebinding defense (a startup token exchanged for an HttpOnly cookie) is
// deferred until the client actually serves HTML to a browser (§7.5, §11).
func originGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" {
			if host := originHost(origin); host != "" && host != r.Host {
				http.Error(w, "cross-origin request refused", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func originHost(origin string) string {
	if i := strings.Index(origin, "://"); i >= 0 {
		return origin[i+3:]
	}
	return origin
}

func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;")
	return r.Replace(s)
}
