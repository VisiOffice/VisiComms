package recordings

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/VisiOffice/VisiComms/internal/sending"
)

// Retention is how long a recording is kept after the call.
const Retention = 365 * 24 * time.Hour

// maxBytes bounds one recording held in memory while it is copied: hours of
// MP3, far past any real call.
const maxBytes = 200 << 20

// ErrExpired is a recording deleted at the end of its year.
var ErrExpired = errors.New("the recording was deleted a year after the call")

// Recording is one call's recording and where it is kept.
type Recording struct {
	CallID, WorkspaceID, ProviderURL string
	RecordedAt                       time.Time
	ObjectKey, ObjectVersion         string
	Archived, ProviderDeleted        bool
	Expired                          bool
}

const columns = `call_id::text, workspace_id::text, provider_url, recorded_at, coalesce(object_key, ''), coalesce(object_version, ''),
	archived_at IS NOT NULL, provider_deleted_at IS NOT NULL, expired_at IS NOT NULL`

func scan(row pgx.Row) (Recording, error) {
	var r Recording
	err := row.Scan(&r.CallID, &r.WorkspaceID, &r.ProviderURL, &r.RecordedAt, &r.ObjectKey, &r.ObjectVersion, &r.Archived, &r.ProviderDeleted, &r.Expired)
	return r, err
}

// Find is a call's recording in a workspace. A recording past its year is
// expired even if the sweep has not deleted it yet.
func Find(ctx context.Context, pool *pgxpool.Pool, workspaceID, callID string, now time.Time) (Recording, error) {
	r, err := scan(pool.QueryRow(ctx, `SELECT `+columns+` FROM recordings WHERE workspace_id = $1 AND call_id = $2::uuid`, workspaceID, callID))
	if err == nil && (r.Expired || now.Sub(r.RecordedAt) >= Retention) {
		return r, ErrExpired
	}
	return r, err
}

// Archiver copies recordings into the bucket, then deletes SignalWire's
// copy, and deletes both a year after the call. Without a Store it only
// enforces the year on SignalWire's copies.
type Archiver struct {
	DB         *pgxpool.Pool
	SignalWire *sending.SignalWireProjects
	Store      Store
	Logger     *slog.Logger
}

// Sweep runs one pass: finds new recordings, expires old ones, and moves
// what is waiting. A failure on one recording is retried later with backoff
// and does not stop the rest.
func (a *Archiver) Sweep(ctx context.Context, now time.Time) error {
	if _, err := a.DB.Exec(ctx, `INSERT INTO recordings (call_id, workspace_id, recorded_at, provider_url, next_attempt_at)
		SELECT DISTINCT ON (c.id) c.id, c.workspace_id, e.occurred_at, e.payload->>'recording_url', e.occurred_at
		FROM events e JOIN calls c ON c.id::text = e.payload->>'call_id' AND c.workspace_id = e.workspace_id
		WHERE e.type IN ('call.recorded', 'voice.voicemail') AND coalesce(e.payload->>'recording_url', '') <> ''
		ORDER BY c.id, e.occurred_at DESC
		ON CONFLICT (call_id) DO NOTHING`); err != nil {
		return err
	}
	due, err := a.list(ctx, `expired_at IS NULL AND recorded_at <= $1`, now.Add(-Retention))
	if err != nil {
		return err
	}
	for _, r := range due {
		a.try(ctx, r, now, a.expire)
	}
	waiting := `expired_at IS NULL AND next_attempt_at <= $1 AND archived_at IS NOT NULL AND provider_deleted_at IS NULL`
	if a.Store != nil {
		waiting = `expired_at IS NULL AND next_attempt_at <= $1 AND (archived_at IS NULL OR provider_deleted_at IS NULL)`
	}
	pending, err := a.list(ctx, waiting, now)
	if err != nil {
		return err
	}
	for _, r := range pending {
		a.try(ctx, r, now, a.move)
	}
	return nil
}

func (a *Archiver) list(ctx context.Context, where string, at time.Time) ([]Recording, error) {
	rows, err := a.DB.Query(ctx, `SELECT `+columns+` FROM recordings WHERE `+where+` ORDER BY recorded_at LIMIT 25`, at)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Recording, error) { return scan(row) })
}

func (a *Archiver) try(ctx context.Context, r Recording, now time.Time, step func(context.Context, Recording, time.Time) error) {
	err := step(ctx, r, now)
	if err == nil {
		return
	}
	if a.Logger != nil {
		a.Logger.Error("archiving a recording", "call", r.CallID, "err", err)
	}
	// 2, 4, 8 … minutes, at most six hours apart.
	if _, dbErr := a.DB.Exec(ctx, `UPDATE recordings SET attempts = attempts + 1, last_error = $2,
		next_attempt_at = $3::timestamptz + least(interval '1 minute' * power(2, attempts + 1), interval '6 hours')
		WHERE call_id = $1::uuid`, r.CallID, err.Error(), now); dbErr != nil && a.Logger != nil {
		a.Logger.Error("recording a failed archive attempt", "call", r.CallID, "err", dbErr)
	}
}

// move copies a recording into the bucket, then deletes SignalWire's copy.
func (a *Archiver) move(ctx context.Context, r Recording, now time.Time) error {
	sw, err := a.SignalWire.ForWorkspace(ctx, r.WorkspaceID)
	if err != nil {
		return err
	}
	if !r.Archived {
		res, err := sw.OpenRecording(ctx, r.ProviderURL, "")
		if err != nil {
			return err
		}
		data, err := io.ReadAll(io.LimitReader(res.Body, maxBytes+1))
		res.Body.Close()
		if err != nil {
			return err
		}
		if len(data) == 0 || len(data) > maxBytes {
			return fmt.Errorf("SignalWire returned a recording of %d bytes", len(data))
		}
		kind, ext := "audio/mpeg", ".mp3"
		if t := strings.ToLower(res.Header.Get("Content-Type")); strings.Contains(t, "wav") {
			kind, ext = "audio/wav", ".wav"
		}
		key := "recordings/" + r.WorkspaceID + "/" + r.CallID + ext
		version, err := a.Store.Put(ctx, key, kind, data)
		if err != nil {
			return fmt.Errorf("storing the recording: %w", err)
		}
		sum := sha256.Sum256(data)
		if _, err := a.DB.Exec(ctx, `UPDATE recordings SET object_key = $2, object_version = nullif($3, ''), content_type = $4,
			bytes = $5, sha256 = $6, archived_at = $7, attempts = 0, last_error = NULL WHERE call_id = $1::uuid`,
			r.CallID, key, version, kind, len(data), sum[:], now); err != nil {
			return err
		}
	}
	if err := sw.DeleteRecording(ctx, r.ProviderURL); err != nil {
		return err
	}
	_, err = a.DB.Exec(ctx, `UPDATE recordings SET provider_deleted_at = $2, attempts = 0, last_error = NULL WHERE call_id = $1::uuid`, r.CallID, now)
	return err
}

// expire deletes both copies of a recording a year after the call.
func (a *Archiver) expire(ctx context.Context, r Recording, now time.Time) error {
	if r.ObjectKey != "" {
		if a.Store == nil {
			return errors.New("recordings storage is not configured, so an expired recording cannot be deleted from it")
		}
		if err := a.Store.Delete(ctx, r.ObjectKey, r.ObjectVersion); err != nil {
			return err
		}
	}
	if !r.ProviderDeleted {
		sw, err := a.SignalWire.ForWorkspace(ctx, r.WorkspaceID)
		if err != nil {
			return err
		}
		if err := sw.DeleteRecording(ctx, r.ProviderURL); err != nil {
			return err
		}
	}
	_, err := a.DB.Exec(ctx, `UPDATE recordings SET expired_at = $2, provider_deleted_at = coalesce(provider_deleted_at, $2) WHERE call_id = $1::uuid`, r.CallID, now)
	return err
}
