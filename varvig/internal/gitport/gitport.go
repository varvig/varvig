// Package gitport translates between a Varvig repository and a plain Git
// repository (design §2, lossless bidirectional Git export).
//
// # Losslessness
//
// Git → Varvig → Git reproduces byte-identical git objects, and therefore
// identical git commit SHAs. Blobs and trees are deterministic functions of
// content, so they reconstruct exactly. Commits carry data Varvig's change model
// does not (committer identity, timezones, first-parent order, extra headers
// such as gpgsig); to avoid losing it, an imported commit retains its exact
// git object body as an interop field on the Varvig change (tag GitCommitBody).
// On export that body is re-emitted verbatim.
//
// Varvig-native changes have no original git object, so their git commit is
// synthesized deterministically (UTC, author == committer). Such a change's
// Varvig identity is not expected to survive a git bounce unchanged — only git
// identities are guaranteed stable across a round trip.
package gitport

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/dividebyzero/claude-experiments/varvig/internal/gitobj"
	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
)

// GitCommitBody is the interop field tag that stores an imported commit's exact
// git object body. It lives in the reserved interop range (>= 0xF000_0000),
// which is not part of the frozen semantic core but round-trips like any
// unknown field (design §4.4).
const GitCommitBody = 0xF0000001

// Export writes the Varvig change reachable from varvigHead into gitDir as loose
// git objects, sets refs/heads/<branch> to the resulting commit, and points
// HEAD at it. It returns the git commit id.
func Export(r *repo.Repo, gitDir, branch string, varvigHead multihash.Multihash) (gitobj.OID, error) {
	if err := initGitDir(gitDir); err != nil {
		return gitobj.OID{}, err
	}
	e := &exporter{objs: r.Objects, gs: gitobj.OpenStore(gitDir), seen: map[string]gitobj.OID{}}
	commit, err := e.object(varvigHead)
	if err != nil {
		return gitobj.OID{}, err
	}
	if err := writeGitRef(gitDir, branch, commit); err != nil {
		return gitobj.OID{}, err
	}
	return commit, nil
}

type exporter struct {
	objs objectStore
	gs   *gitobj.Store
	seen map[string]gitobj.OID
}

func (e *exporter) object(id multihash.Multihash) (gitobj.OID, error) {
	if g, ok := e.seen[id.Hex()]; ok {
		return g, nil
	}
	o, err := e.objs.Get(id)
	if err != nil {
		return gitobj.OID{}, err
	}
	var g gitobj.OID
	switch o.Type() {
	case object.TypeBlob:
		content, _ := o.BlobContent()
		g, err = e.gs.Write(gitobj.KindBlob, content)
	case object.TypeTree:
		g, err = e.tree(o)
	case object.TypeChange:
		g, err = e.change(o)
	default:
		return gitobj.OID{}, fmt.Errorf("gitport: cannot export object type %s", o.Type())
	}
	if err != nil {
		return gitobj.OID{}, err
	}
	e.seen[id.Hex()] = g
	return g, nil
}

func (e *exporter) tree(o *object.Object) (gitobj.OID, error) {
	entries, err := o.TreeEntries()
	if err != nil {
		return gitobj.OID{}, err
	}
	var ges []gitobj.TreeEntry
	for _, en := range entries {
		childGit, err := e.object(en.ID)
		if err != nil {
			return gitobj.OID{}, err
		}
		ges = append(ges, gitobj.TreeEntry{Mode: gitMode(en), Name: en.Name, OID: childGit})
	}
	return e.gs.Write(gitobj.KindTree, gitobj.EncodeTree(ges))
}

func (e *exporter) change(o *object.Object) (gitobj.OID, error) {
	ch, err := o.AsChange()
	if err != nil {
		return gitobj.OID{}, err
	}
	treeGit, err := e.object(ch.Tree)
	if err != nil {
		return gitobj.OID{}, err
	}
	// Export parents first so they exist in the git store regardless of path.
	for _, p := range ch.Parents {
		if _, err := e.object(p); err != nil {
			return gitobj.OID{}, err
		}
	}

	// Imported commits carry their exact git body: re-emit it verbatim so the
	// git identity is reproduced bit for bit.
	if body, ok := o.Field(GitCommitBody); ok {
		parsed, err := gitobj.DecodeCommit(body)
		if err != nil {
			return gitobj.OID{}, err
		}
		if parsed.Tree != treeGit {
			return gitobj.OID{}, fmt.Errorf("gitport: reconstructed tree %s != recorded %s (lossy)", treeGit.Hex(), parsed.Tree.Hex())
		}
		return e.gs.Write(gitobj.KindCommit, body)
	}

	// Varvig-native change: synthesize a deterministic commit. Parent order
	// follows the change's canonical (sorted) parent order.
	var parents []gitobj.OID
	for _, p := range ch.Parents {
		g, err := e.object(p)
		if err != nil {
			return gitobj.OID{}, err
		}
		parents = append(parents, g)
	}
	ident := normalizeIdent(ch.Author)
	// The ticket→commit link is carried across the Git boundary as a commit
	// trailer, so a native change's Fulfills survives export → import (C3). An
	// imported commit takes the verbatim-body path above and keeps whatever
	// trailer it already had.
	body := gitobj.EncodeCommit(treeGit, parents, ident, ch.Timestamp, withFulfillsTrailer(ch.Message, ch.Fulfills))
	return e.gs.Write(gitobj.KindCommit, body)
}

// fulfillsTrailerKey is the Git commit trailer that carries Change.Fulfills.
const fulfillsTrailerKey = "Varvig-Fulfills"

// withFulfillsTrailer appends the ticket→commit link as a trailer, or returns the
// message unchanged when the change fulfills nothing.
func withFulfillsTrailer(msg string, f multihash.Multihash) string {
	if f == nil {
		return msg
	}
	return strings.TrimRight(msg, "\n") + "\n\n" + fulfillsTrailerKey + ": " + f.Hex()
}

// splitFulfillsTrailer pulls a trailing Varvig-Fulfills trailer off an imported
// commit message, returning the cleaned message and the intent revision it named
// (nil if there is none or it does not parse). Only the last non-blank line is
// considered, matching how withFulfillsTrailer writes it.
func splitFulfillsTrailer(msg string) (string, multihash.Multihash) {
	lines := strings.Split(msg, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) == "" {
			continue
		}
		prefix := fulfillsTrailerKey + ": "
		line := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(line, prefix) {
			break // the last non-blank line is not our trailer
		}
		h, err := multihash.ParseHex(strings.TrimSpace(strings.TrimPrefix(line, prefix)))
		if err != nil {
			return msg, nil // malformed; leave the message intact
		}
		return strings.TrimRight(strings.Join(lines[:i], "\n"), "\n"), h
	}
	return msg, nil
}

// Import reads the git commit at refs/heads/<branch> in gitDir into r, creating
// Varvig objects, and sets the Varvig ref refs/heads/<branch>. Returns the Varvig
// change id.
func Import(r *repo.Repo, gitDir, branch string) (multihash.Multihash, error) {
	head, err := readGitRef(gitDir, branch)
	if err != nil {
		return nil, err
	}
	im := &importer{objs: r.Objects, gs: gitobj.OpenStore(gitDir), seen: map[string]multihash.Multihash{}}
	varvigID, err := im.object(head)
	if err != nil {
		return nil, err
	}
	name := "refs/heads/" + branch
	cur, err := r.Refs.Resolve(name)
	if err != nil {
		cur = nil
	}
	if err := r.Refs.CompareAndSwap(name, cur, varvigID, "git-import", "import from git"); err != nil {
		return nil, err
	}
	return varvigID, nil
}

type importer struct {
	objs objectStore
	gs   *gitobj.Store
	seen map[string]multihash.Multihash
}

func (im *importer) object(g gitobj.OID) (multihash.Multihash, error) {
	if id, ok := im.seen[g.Hex()]; ok {
		return id, nil
	}
	kind, body, err := im.gs.Read(g)
	if err != nil {
		return nil, err
	}
	var id multihash.Multihash
	switch kind {
	case gitobj.KindBlob:
		id, err = im.objs.Put(object.NewBlob(body))
	case gitobj.KindTree:
		id, err = im.tree(body)
	case gitobj.KindCommit:
		id, err = im.commit(body)
	default:
		return nil, fmt.Errorf("gitport: unknown git object kind %q", kind)
	}
	if err != nil {
		return nil, err
	}
	im.seen[g.Hex()] = id
	return id, nil
}

func (im *importer) tree(body []byte) (multihash.Multihash, error) {
	ges, err := gitobj.DecodeTree(body)
	if err != nil {
		return nil, err
	}
	var entries []object.Entry
	for _, ge := range ges {
		childID, err := im.object(ge.OID)
		if err != nil {
			return nil, err
		}
		kind := object.TypeBlob
		if ge.Mode == gitobj.ModeTree {
			kind = object.TypeTree
		}
		entries = append(entries, object.Entry{
			Name: ge.Name,
			Mode: parseOctal(ge.Mode),
			Kind: kind,
			ID:   childID,
		})
	}
	return im.objs.Put(object.NewTree(entries))
}

func (im *importer) commit(body []byte) (multihash.Multihash, error) {
	c, err := gitobj.DecodeCommit(body)
	if err != nil {
		return nil, err
	}
	treeID, err := im.object(c.Tree)
	if err != nil {
		return nil, err
	}
	var parents []multihash.Multihash
	for _, p := range c.Parents {
		pid, err := im.object(p)
		if err != nil {
			return nil, err
		}
		parents = append(parents, pid)
	}
	msg, fulfills := splitFulfillsTrailer(c.Message)
	obj := object.NewChange(object.Change{
		Tree:      treeID,
		Parents:   parents,
		Message:   msg,
		Timestamp: c.AuthorTS,
		Author:    c.Author,
		Fulfills:  fulfills,
	})
	// Retain the exact git body so a re-export is byte-identical.
	obj.SetField(GitCommitBody, c.RawBody)
	return im.objs.Put(obj)
}

// objectStore is the subset of the object store gitport needs.
type objectStore interface {
	Get(multihash.Multihash) (*object.Object, error)
	Put(*object.Object) (multihash.Multihash, error)
}

func gitMode(e object.Entry) string {
	if e.Kind == object.TypeTree {
		return gitobj.ModeTree
	}
	switch e.Mode {
	case 0o100755:
		return gitobj.ModeExec
	case 0o120000:
		return gitobj.ModeSymlink
	default:
		return gitobj.ModeFile
	}
}

func parseOctal(mode string) uint32 {
	var v uint32
	for _, c := range mode {
		if c < '0' || c > '7' {
			return v
		}
		v = v*8 + uint32(c-'0')
	}
	return v
}

// normalizeIdent turns a Varvig author string into a git identity "Name <email>".
// A bare name gets an empty-address form so the git line stays well-formed.
func normalizeIdent(author string) string {
	if author == "" {
		author = "varvig"
	}
	if strings.Contains(author, "<") && strings.HasSuffix(strings.TrimSpace(author), ">") {
		return author
	}
	return fmt.Sprintf("%s <>", author)
}

// --- minimal git repository plumbing ---

func initGitDir(gitDir string) error {
	for _, d := range []string{
		filepath.Join(gitDir, "objects"),
		filepath.Join(gitDir, "refs", "heads"),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	config := "[core]\n\trepositoryformatversion = 0\n\tfilemode = true\n\tbare = false\n"
	if err := os.WriteFile(filepath.Join(gitDir, "config"), []byte(config), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644)
}

func writeGitRef(gitDir, branch string, oid gitobj.OID) error {
	p := filepath.Join(gitDir, "refs", "heads", branch)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(p, []byte(oid.Hex()+"\n"), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte("ref: refs/heads/"+branch+"\n"), 0o644)
}

func readGitRef(gitDir, branch string) (gitobj.OID, error) {
	// Loose ref first; a cloned repo may instead keep it in packed-refs.
	if b, err := os.ReadFile(filepath.Join(gitDir, "refs", "heads", branch)); err == nil {
		return gitobj.ParseOID(strings.TrimSpace(string(b)))
	} else if !os.IsNotExist(err) {
		return gitobj.OID{}, err
	}
	if oid, ok, err := readPackedRef(gitDir, "refs/heads/"+branch); err != nil {
		return gitobj.OID{}, err
	} else if ok {
		return oid, nil
	}
	return gitobj.OID{}, fmt.Errorf("gitport: no ref refs/heads/%s", branch)
}

// readPackedRef looks up a fully-qualified ref name in .git/packed-refs.
func readPackedRef(gitDir, fullName string) (gitobj.OID, bool, error) {
	b, err := os.ReadFile(filepath.Join(gitDir, "packed-refs"))
	if err != nil {
		if os.IsNotExist(err) {
			return gitobj.OID{}, false, nil
		}
		return gitobj.OID{}, false, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "^") {
			continue // comment, or a peeled-tag line
		}
		sha, name, ok := strings.Cut(line, " ")
		if !ok || name != fullName {
			continue
		}
		oid, err := gitobj.ParseOID(sha)
		return oid, err == nil, err
	}
	return gitobj.OID{}, false, nil
}
