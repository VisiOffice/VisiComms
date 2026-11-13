package db_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/VisiOffice/VisiComms/internal/db"
	"github.com/VisiOffice/VisiComms/internal/testutil"
)

func TestRequireMigratedAcceptsACurrentSchema(t *testing.T) {
	pool := testutil.DB(t)
	if err := db.RequireMigrated(context.Background(), pool); err != nil {
		t.Fatalf("a migrated database was refused: %v", err)
	}
}

// A deploy that skipped its migration must not start. Run inside a
// transaction that is rolled back, so the shared test database stays current.
func TestPendingSeesAMissingMigration(t *testing.T) {
	pool := testutil.DB(t)
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	versions := db.Versions()
	last := versions[len(versions)-1]
	if _, err := tx.Exec(ctx, `DELETE FROM schema_migrations WHERE version = $1`, last); err != nil {
		t.Fatal(err)
	}
	pending, err := db.Pending(ctx, tx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0] != last {
		t.Fatalf("pending = %v, want [%s]", pending, last)
	}
}

type fixture struct {
	workspace, human, agent string
}

func seed(t *testing.T, pool *pgxpool.Pool) fixture {
	t.Helper()
	ctx := context.Background()
	var f fixture
	must(t, pool.QueryRow(ctx, `INSERT INTO workspaces (name, timezone) VALUES ('Acme Plumbing', 'America/Los_Angeles') RETURNING id`).Scan(&f.workspace))
	must(t, pool.QueryRow(ctx, `INSERT INTO principals (workspace_id, kind, name) VALUES ($1, 'human', 'Morgan') RETURNING id`, f.workspace).Scan(&f.human))
	must(t, pool.QueryRow(ctx, `INSERT INTO principals (workspace_id, kind, name, token_sha256) VALUES ($1, 'agent', 'Acme Plumbing runner v3', '\x01') RETURNING id`, f.workspace).Scan(&f.agent))
	return f
}

func TestEventsAreAppendOnly(t *testing.T) {
	pool := testutil.DB(t)
	ctx := context.Background()
	f := seed(t, pool)
	var id int64
	must(t, pool.QueryRow(ctx, `INSERT INTO events (workspace_id, type, payload, untrusted)
		VALUES ($1, 'email.received', '{"text":"Do you have a water heater?"}', true) RETURNING id`, f.workspace).Scan(&id))

	_, err := pool.Exec(ctx, `UPDATE events SET payload = '{}' WHERE id = $1`, id)
	wantError(t, err, "append-only")
	_, err = pool.Exec(ctx, `DELETE FROM events WHERE id = $1`, id)
	wantError(t, err, "append-only")

	// A correction is a new event pointing at the old one.
	if _, err := pool.Exec(ctx, `INSERT INTO events (workspace_id, type, corrects_event_id)
		VALUES ($1, 'email.received.corrected', $2)`, f.workspace, id); err != nil {
		t.Fatalf("correcting event refused: %v", err)
	}
}

func TestInboundEventsDedupeOnProviderRef(t *testing.T) {
	pool := testutil.DB(t)
	ctx := context.Background()
	f := seed(t, pool)
	insert := `INSERT INTO events (workspace_id, type, provider, provider_ref) VALUES ($1, 'email.received', 'cloudflare', '<abc@mail>')`
	if _, err := pool.Exec(ctx, insert, f.workspace); err != nil {
		t.Fatal(err)
	}
	_, err := pool.Exec(ctx, insert, f.workspace)
	if !db.IsUniqueViolation(err) {
		t.Fatalf("second delivery of the same Message-ID was stored: %v", err)
	}
}

func TestApprovedInfoIsFrozenOnceApproved(t *testing.T) {
	pool := testutil.DB(t)
	ctx := context.Background()
	f := seed(t, pool)
	var id string
	must(t, pool.QueryRow(ctx, `INSERT INTO approved_info_versions (workspace_id, version, content, created_by)
		VALUES ($1, 12, '{"hours":{"weekdays":"[HOURS]"}}', $2) RETURNING id`, f.workspace, f.agent).Scan(&id))

	_, err := pool.Exec(ctx, `UPDATE approved_info_versions SET approved_by = $2, approved_at = now() WHERE id = $1`, id, f.agent)
	wantError(t, err, "only a human")

	if _, err := pool.Exec(ctx, `UPDATE approved_info_versions SET approved_by = $2, approved_at = now() WHERE id = $1`, id, f.human); err != nil {
		t.Fatalf("human approval refused: %v", err)
	}
	_, err = pool.Exec(ctx, `UPDATE approved_info_versions SET content = '{}' WHERE id = $1`, id)
	wantError(t, err, "cannot be changed")
	_, err = pool.Exec(ctx, `DELETE FROM approved_info_versions WHERE id = $1`, id)
	wantError(t, err, "cannot be deleted")
}

func TestLinksAllowOneRecordOnSeveralThreads(t *testing.T) {
	pool := testutil.DB(t)
	ctx := context.Background()
	f := seed(t, pool)
	var contact string
	must(t, pool.QueryRow(ctx, `INSERT INTO contacts (workspace_id, display_name) VALUES ($1, 'Marcus Lee') RETURNING id`, f.workspace).Scan(&contact))
	threads := make([]string, 2)
	for i := range threads {
		must(t, pool.QueryRow(ctx, `INSERT INTO threads (workspace_id, contact_id, channel) VALUES ($1, $2, 'email') RETURNING id`, f.workspace, contact).Scan(&threads[i]))
	}
	insert := `INSERT INTO links (workspace_id, thread_id, system, record_type, external_id) VALUES ($1, $2, 'bins', 'quote_request', '42')`
	for _, th := range threads {
		if _, err := pool.Exec(ctx, insert, f.workspace, th); err != nil {
			t.Fatalf("link on second thread refused: %v", err)
		}
	}
	_, err := pool.Exec(ctx, insert, f.workspace, threads[0])
	if !db.IsUniqueViolation(err) {
		t.Fatalf("duplicate link on one thread was stored: %v", err)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func wantError(t *testing.T, err error, contains string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), contains) {
		t.Fatalf("error = %v, want one containing %q", err, contains)
	}
}
