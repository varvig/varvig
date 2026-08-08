// Package spec is Varvig's speculation store (design §1.5). Branching becomes
// search: an agent produces many ephemeral attempt-states — content-addressed
// changes — grouped under a task, a scorer ranks them against an objective, and
// the winner is promoted onto a real ref. Everything that is not promoted is
// governed by a retention policy, because at speculation volume retention is a
// first-order problem, not a background cleanup (§1.5, §5).
//
// A candidate is one file under .varvig/spec/<task>/<change-hex>, so concurrent
// agents add attempts with independent atomic writes and no whole-pool
// contention. The pool is a garbage-collection root: an active candidate is
// protected until retention prunes it, after which GC may reclaim it (unless it
// is reachable from a ref or the reflog).
package spec

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
)

// Entry is one speculation candidate.
type Entry struct {
	Change  multihash.Multihash `json:"-"`
	Score   float64             `json:"score"`
	Scored  bool                `json:"scored"`
	Created int64               `json:"created"`
}

// Pool is a speculation store rooted at .varvig/spec.
type Pool struct{ dir string }

// Open returns the speculation pool for a repository metadata directory.
func Open(gitDir string) *Pool { return &Pool{dir: filepath.Join(gitDir, "spec")} }

func validTask(task string) error {
	if task == "" || strings.ContainsAny(task, "/\\ \t") {
		return fmt.Errorf("spec: invalid task name %q", task)
	}
	return nil
}

func (p *Pool) taskDir(task string) string { return filepath.Join(p.dir, task) }
func (p *Pool) entryPath(task string, change multihash.Multihash) string {
	return filepath.Join(p.taskDir(task), change.Hex())
}

// Add records a candidate change under a task. It is idempotent and safe for
// concurrent callers.
func (p *Pool) Add(task string, change multihash.Multihash, now int64) error {
	if err := validTask(task); err != nil {
		return err
	}
	if err := os.MkdirAll(p.taskDir(task), 0o755); err != nil {
		return err
	}
	path := p.entryPath(task, change)
	if _, err := os.Stat(path); err == nil {
		return nil // already present
	}
	return writeEntry(path, Entry{Created: now})
}

// List returns all candidates for a task.
func (p *Pool) List(task string) ([]Entry, error) {
	if err := validTask(task); err != nil {
		return nil, err
	}
	files, err := os.ReadDir(p.taskDir(task))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Entry
	for _, f := range files {
		change, err := multihash.ParseHex(f.Name())
		if err != nil {
			continue
		}
		e, err := readEntry(filepath.Join(p.taskDir(task), f.Name()))
		if err != nil {
			return nil, err
		}
		e.Change = change
		out = append(out, e)
	}
	return out, nil
}

// SetScore records a candidate's score.
func (p *Pool) SetScore(task string, change multihash.Multihash, score float64) error {
	if err := validTask(task); err != nil {
		return err
	}
	path := p.entryPath(task, change)
	e, err := readEntry(path)
	if err != nil {
		return err
	}
	e.Score = score
	e.Scored = true
	return writeEntry(path, e)
}

// Best returns the highest-scored candidate for a task.
func (p *Pool) Best(task string) (Entry, bool, error) {
	entries, err := p.List(task)
	if err != nil {
		return Entry{}, false, err
	}
	best := Entry{}
	found := false
	for _, e := range entries {
		if !e.Scored {
			continue
		}
		if !found || e.Score > best.Score {
			best, found = e, true
		}
	}
	return best, found, nil
}

// Prune enforces the retention policy: keep the top keepTopK candidates by
// score (scored candidates rank above unscored) and remove the rest from the
// pool. Removing a candidate from the pool does not delete its objects — it
// merely stops protecting them from garbage collection. Returns the removed
// change ids.
func (p *Pool) Prune(task string, keepTopK int) ([]multihash.Multihash, error) {
	entries, err := p.List(task)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].Scored != entries[j].Scored {
			return entries[i].Scored // scored first
		}
		return entries[i].Score > entries[j].Score
	})
	var removed []multihash.Multihash
	for i, e := range entries {
		if i < keepTopK {
			continue
		}
		if err := os.Remove(p.entryPath(task, e.Change)); err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		removed = append(removed, e.Change)
	}
	return removed, nil
}

// Tasks lists the tasks that have candidates.
func (p *Pool) Tasks() ([]string, error) {
	dirs, err := os.ReadDir(p.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, d := range dirs {
		if d.IsDir() {
			out = append(out, d.Name())
		}
	}
	return out, nil
}

// AllChanges returns every candidate change id across all tasks. These are
// garbage-collection roots: active speculations are protected.
func (p *Pool) AllChanges() ([]multihash.Multihash, error) {
	tasks, err := p.Tasks()
	if err != nil {
		return nil, err
	}
	var out []multihash.Multihash
	for _, t := range tasks {
		entries, err := p.List(t)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			out = append(out, e.Change)
		}
	}
	return out, nil
}

// Scorer ranks a candidate change against an objective. It is intentionally an
// interface (design §3.3): a real objective — a test suite, a wasm scorer — is
// injected, never embedded in the store.
type Scorer interface {
	Score(ctx context.Context, r *repo.Repo, change multihash.Multihash) (float64, error)
}

// ScorerFunc adapts a function to a Scorer.
type ScorerFunc func(ctx context.Context, r *repo.Repo, change multihash.Multihash) (float64, error)

// Score implements Scorer.
func (f ScorerFunc) Score(ctx context.Context, r *repo.Repo, change multihash.Multihash) (float64, error) {
	return f(ctx, r, change)
}

// ScoreAll scores every candidate for a task and records the results.
func (p *Pool) ScoreAll(ctx context.Context, task string, r *repo.Repo, s Scorer) error {
	entries, err := p.List(task)
	if err != nil {
		return err
	}
	for _, e := range entries {
		score, err := s.Score(ctx, r, e.Change)
		if err != nil {
			return err
		}
		if err := p.SetScore(task, e.Change, score); err != nil {
			return err
		}
	}
	return nil
}

// Promote advances ref to the task's best-scored candidate via compare-and-swap
// (design §1.5: promote the winner). The promoted change becomes reachable from
// the ref and is thereafter permanent; losers remain in the pool until pruned.
func Promote(p *Pool, r *repo.Repo, task, ref, actor string) (multihash.Multihash, error) {
	best, ok, err := p.Best(task)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("spec: no scored candidate to promote for task %q", task)
	}
	cur, err := r.Refs.Resolve(ref)
	if err != nil {
		cur = nil
	}
	if err := r.Refs.CompareAndSwap(ref, cur, best.Change, actor, "promote "+task); err != nil {
		return nil, err
	}
	return best.Change, nil
}

func writeEntry(path string, e Entry) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-spec-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func readEntry(path string) (Entry, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Entry{}, err
	}
	var e Entry
	if err := json.Unmarshal(b, &e); err != nil {
		return Entry{}, err
	}
	return e, nil
}
