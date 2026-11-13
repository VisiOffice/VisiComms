package sending

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
)

// SendArgs is the job that dispatches one action.
type SendArgs struct {
	ActionID string `json:"action_id"`
}

func (SendArgs) Kind() string { return "send_action" }

// One attempt only: Dispatch itself decides what a failure means, and a
// blind retry is exactly what could send twice.
func (SendArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: 1, UniqueOpts: river.UniqueOpts{ByArgs: true}}
}

type sendWorker struct {
	river.WorkerDefaults[SendArgs]
	pool       *pgxpool.Pool
	provider   Sender
	signalwire *SignalWireProjects
	lob        *LobAccounts
}

func (w *sendWorker) Work(ctx context.Context, job *river.Job[SendArgs]) error {
	var kind, action, workspaceID, project, connector string
	if err := w.pool.QueryRow(ctx, `SELECT c.kind,a.action,a.workspace_id,coalesce(c.settings->>'project_id',''),c.id::text
   FROM actions a JOIN connectors c ON c.id=a.connector_id WHERE a.id=$1`, job.Args.ActionID).Scan(&kind, &action, &workspaceID, &project, &connector); err != nil {
		return err
	}
	if kind == "lob" {
		lob, owner, err := w.lob.Client(ctx, connector)
		if err != nil {
			return fmt.Errorf("Lob account for action %s: %w", job.Args.ActionID, err)
		}
		if owner != workspaceID {
			return fmt.Errorf("Lob connector %s belongs to another workspace", connector)
		}
		_, err = DispatchLetter(ctx, w.pool, lob, job.Args.ActionID)
		return err
	}
	if kind == "signalwire" {
		// The number's project, not the workspace's current one: an action
		// approved before a project change still goes out as approved, or not
		// at all (Dispatch checks the number against the same project).
		sw, owner, err := w.signalwire.ForProject(ctx, project)
		if err != nil {
			return fmt.Errorf("SignalWire project for action %s: %w", job.Args.ActionID, err)
		}
		if owner != "" && owner != workspaceID {
			return fmt.Errorf("SignalWire project %s belongs to another workspace", project)
		}
		if action == "fax.send" {
			_, err = DispatchFax(ctx, w.pool, sw, job.Args.ActionID)
		} else {
			_, err = DispatchSMS(ctx, w.pool, sw, job.Args.ActionID)
		}
		return err
	}
	if w.provider == nil {
		return errors.New("email sending is not configured")
	}
	_, err := Dispatch(ctx, w.pool, w.provider, job.Args.ActionID)
	return err
}

// SweepArgs is the periodic safety net.
type SweepArgs struct{}

func (SweepArgs) Kind() string { return "send_sweep" }

type sweepWorker struct {
	river.WorkerDefaults[SweepArgs]
	pool          *pgxpool.Pool
	providerKind  string
	providerKinds []string
}

// stuckAfter is how long an action may sit dispatching before it is reported
// as unknown: far longer than a send takes, so only a crash gets here.
const stuckAfter = 10 * time.Minute

func (w *sweepWorker) Work(ctx context.Context, _ *river.Job[SweepArgs]) error {
	if err := ReconcileEvents(ctx, w.pool); err != nil {
		return err
	}
	// Mid-send crashes: the provider may or may not have the message.
	if _, err := w.pool.Exec(ctx, `WITH stuck AS (
			UPDATE actions SET state = 'unknown', updated_at = now()
			WHERE state = 'dispatching' AND updated_at < now() - $1::interval
			RETURNING id, workspace_id, thread_id, requested_by)
		INSERT INTO events (workspace_id, thread_id, contact_id, type, actor_id, channel, payload)
		SELECT s.workspace_id, s.thread_id, t.contact_id, t.channel||'.send_unknown', s.requested_by, t.channel,
			jsonb_build_object('action_id', s.id, 'note', 'Sending was interrupted. Check delivery before sending again.')
		FROM stuck s JOIN threads t ON t.id = s.thread_id`, stuckAfter.String()); err != nil {
		return err
	}
	// Queued sends whose job never ran (a restart between commit and work).
	kinds := w.providerKinds
	if len(kinds) == 0 {
		kinds = []string{w.providerKind}
	}
	rows, err := w.pool.Query(ctx, `SELECT a.id FROM actions a JOIN connectors c ON c.id = a.connector_id
		WHERE a.state = 'queued' AND c.kind = ANY($1) AND a.updated_at < now() - interval '1 minute'`, kinds)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || len(ids) == 0 {
		return err
	}
	client := river.ClientFromContext[pgx.Tx](ctx)
	params := make([]river.InsertManyParams, len(ids))
	for i, id := range ids {
		params[i] = river.InsertManyParams{Args: SendArgs{ActionID: id}}
	}
	_, err = client.InsertMany(ctx, params)
	return err
}

// Register adds the sending workers and the sweep to a River setup. With no
// provider configured, sends stay queued and nothing is registered.
func Register(workers *river.Workers, pool *pgxpool.Pool, p Sender, signalwire *SignalWireProjects, lob *LobAccounts) []*river.PeriodicJob {
	if p == nil && signalwire == nil && lob == nil {
		return nil
	}
	kinds := []string{}
	if p != nil {
		kinds = append(kinds, ProviderKind(p))
	}
	if signalwire != nil {
		kinds = append(kinds, "signalwire")
	}
	if lob != nil {
		kinds = append(kinds, "lob")
	}
	river.AddWorker(workers, &sendWorker{pool: pool, provider: p, signalwire: signalwire, lob: lob})
	river.AddWorker(workers, &sweepWorker{pool: pool, providerKinds: kinds})
	return []*river.PeriodicJob{
		river.NewPeriodicJob(river.PeriodicInterval(time.Minute), func() (river.JobArgs, *river.InsertOpts) {
			return SweepArgs{}, &river.InsertOpts{UniqueOpts: river.UniqueOpts{ByPeriod: time.Minute}}
		}, nil),
	}
}

// Enqueue inserts the send job in the caller's transaction, so a rolled-back
// approval never sends.
func Enqueue(client *river.Client[pgx.Tx]) func(ctx context.Context, tx pgx.Tx, actionID string) error {
	return func(ctx context.Context, tx pgx.Tx, actionID string) error {
		_, err := client.InsertTx(ctx, tx, SendArgs{ActionID: actionID}, nil)
		return err
	}
}
