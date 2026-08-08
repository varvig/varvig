package hook

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dividebyzero/claude-experiments/loom/internal/multihash"
	"github.com/dividebyzero/claude-experiments/loom/internal/repo"
)

// Ref-change trigger events (design §2: CI, deployment, and automation hang off
// ref changes, so these must be first-class). A ref update is the moment a
// branch advances — locally via update-ref or remotely via a push — and is
// exactly where a deploy/policy gate belongs.
const (
	// EventRefUpdate fires before a ref changes; a nonzero exit vetoes it.
	EventRefUpdate = "ref-update"
	// EventPostRefUpdate fires after a ref changed; informational only.
	EventPostRefUpdate = "post-ref-update"
)

// refUpdatePayload is the JSON handed to a ref-update hook on stdin.
type refUpdatePayload struct {
	Event string `json:"event"`
	Ref   string `json:"ref"`
	Old   string `json:"old"` // hex, empty for creation
	New   string `json:"new"` // hex, empty for deletion
}

// EvaluateRefUpdate runs pre ref-update hooks for a proposed change to name.
// It returns a non-nil error if any hook vetoes, so callers can refuse the
// update before performing the compare-and-swap. With no hooks bound it is a
// cheap no-op.
func EvaluateRefUpdate(ctx context.Context, r *repo.Repo, name string, old, new multihash.Multihash) error {
	payload, _ := json.Marshal(refUpdatePayload{
		Event: EventRefUpdate, Ref: name, Old: hexOrEmpty(old), New: hexOrEmpty(new),
	})
	results, err := Fire(ctx, r, EventRefUpdate, payload)
	if err != nil {
		return err
	}
	for _, res := range results {
		if !res.Allowed() {
			msg := strings.TrimSpace(string(res.Stderr))
			if msg == "" {
				msg = fmt.Sprintf("exit %d", res.ExitCode)
			}
			return fmt.Errorf("ref-update %s vetoed: %s", name, msg)
		}
	}
	return nil
}

// NotifyRefUpdate runs post ref-update hooks. Failures are returned but callers
// typically ignore them, since the ref has already moved.
func NotifyRefUpdate(ctx context.Context, r *repo.Repo, name string, old, new multihash.Multihash) error {
	payload, _ := json.Marshal(refUpdatePayload{
		Event: EventPostRefUpdate, Ref: name, Old: hexOrEmpty(old), New: hexOrEmpty(new),
	})
	_, err := Fire(ctx, r, EventPostRefUpdate, payload)
	return err
}

func hexOrEmpty(m multihash.Multihash) string {
	if m == nil {
		return ""
	}
	return m.Hex()
}
