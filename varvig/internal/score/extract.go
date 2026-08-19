package score

import (
	"sort"

	"github.com/dividebyzero/claude-experiments/varvig/internal/bridge"
	"github.com/dividebyzero/claude-experiments/varvig/internal/deps"
	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
)

// ExtractFeatures computes a ticket's features from repository state (tickets
// §3.3). Nothing here is hand-labelled: blast radius comes from the declared
// write set, contention from the derived dependency graph, and age from the
// ticket's own timestamp. all is the set the contention count is measured
// against (typically deps.ScopedTickets); now is the reference time for age.
func ExtractFeatures(r *repo.Repo, ticket deps.Ticket, all []deps.Ticket, now int64) Features {
	f := Features{
		BlastRadius: float64(len(ticket.Scope.Writes)),
		Unblocks:    float64(len(deps.Blockers(ticket, all))),
	}
	if obj, err := r.Objects.Get(ticket.ID); err == nil && obj.Type() == object.TypeChange {
		if c, err := obj.AsChange(); err == nil && c.Timestamp > 0 {
			if age := now - c.Timestamp; age > 0 {
				f.AgeSeconds = float64(age)
			}
		}
	}
	// A linked tracker may project a priority nudge onto the ticket (§5.2). It is
	// the one non-derived feature; the scorer decides how far to trust it.
	if link, ok, err := bridge.GetLink(r, ticket.ID); err == nil && ok {
		f.PriorityNudge = link.PriorityNudge
	}
	return f
}

// Ranked is a ticket paired with its computed score, for a ranked listing.
type Ranked struct {
	ID       multihash.Multihash
	Score    float64
	Features Features
}

// RankTickets scores every ticket with w and returns them highest score first.
// Ties break by hash, so the order is total and deterministic — the same
// tickets, weights, and reference time always produce the same ranking (§7.4).
func RankTickets(r *repo.Repo, w Weights, tickets []deps.Ticket, now int64) []Ranked {
	out := make([]Ranked, 0, len(tickets))
	for _, t := range tickets {
		f := ExtractFeatures(r, t, tickets, now)
		out = append(out, Ranked{ID: t.ID, Score: w.Score(f), Features: f})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].ID.Hex() < out[j].ID.Hex()
	})
	return out
}
