package agent

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/VisiOffice/VisiComms/internal/db"
)

// ClaimFor is how long a claim holds a thread. Claiming again renews it.
const ClaimFor = 15 * time.Minute

var (
	// ErrClaimed is a thread someone else is working on.
	ErrClaimed = errors.New("someone else is working on this thread")
	// ErrTakenOver is a thread a person has taken over.
	ErrTakenOver = errors.New("a person has taken over this thread; draft only if they ask")
)

// Claim is a hold on a thread.
type Claim struct {
	Status string     `json:"status"`
	Until  *time.Time `json:"until,omitempty"`
}

// ClaimThread holds a thread for the agent while it works on a reply, so a
// second agent does not answer too; drafting on a thread another agent holds
// is refused. The claim lapses after ClaimFor, and claiming again renews
// it. release lets go of it early.
func ClaimThread(ctx context.Context, pool *pgxpool.Pool, a Agent, threadID string, release bool) (Claim, error) {
	if !validID(threadID) {
		return Claim{}, ErrNotFound
	}
	var out Claim
	err := db.Tx(ctx, pool, func(tx pgx.Tx) error {
		var assignee, holder *string
		var until *time.Time
		var takenOver bool
		err := tx.QueryRow(ctx, `SELECT t.assignee_id, p.name, t.claimed_until, t.taken_over_by IS NOT NULL
			FROM threads t LEFT JOIN principals p ON p.id = t.assignee_id
			WHERE t.id = $1 AND t.workspace_id = $2 FOR UPDATE OF t`, threadID, a.WorkspaceID).
			Scan(&assignee, &holder, &until, &takenOver)
		if db.NotFound(err) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		mine := assignee != nil && *assignee == a.ID
		if release {
			out.Status = "released"
			if !mine || until == nil {
				return nil
			}
			if _, err := tx.Exec(ctx, `UPDATE threads SET assignee_id = NULL, claimed_until = NULL, updated_at = now() WHERE id = $1`, threadID); err != nil {
				return err
			}
			return event(ctx, tx, a, threadID, "thread.released", map[string]any{})
		}
		if takenOver {
			return ErrTakenOver
		}
		if assignee != nil && !mine && (until == nil || until.After(time.Now())) {
			name := "someone else"
			if holder != nil {
				name = *holder
			}
			return claimedBy(name)
		}
		if err := tx.QueryRow(ctx, `UPDATE threads SET assignee_id = $2, claimed_until = now() + make_interval(secs => $3)
			WHERE id = $1 RETURNING claimed_until`, threadID, a.ID, ClaimFor.Seconds()).Scan(&out.Until); err != nil {
			return err
		}
		out.Status = "claimed"
		if mine {
			out.Status = "renewed"
			return nil
		}
		return event(ctx, tx, a, threadID, "thread.claimed", map[string]any{"until": out.Until})
	})
	return out, err
}

type claimError struct{ name string }

func (e claimError) Error() string { return e.name + " is working on this thread" }
func (e claimError) Unwrap() error { return ErrClaimed }

func claimedBy(name string) error { return claimError{name} }
