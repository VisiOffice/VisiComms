package auth_test

import (
	"context"
	"errors"
	"testing"

	"github.com/VisiOffice/VisiComms/internal/auth"
	"github.com/VisiOffice/VisiComms/internal/testutil"
)

func names(t *testing.T, s *auth.Service, p auth.Person) map[string]bool {
	t.Helper()
	list, err := s.Workspaces(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, m := range list {
		out[m.Name] = m.Current
	}
	return out
}

func TestAcceptingAnInviteWhileSignedInAddsAWorkspaceToSwitchTo(t *testing.T) {
	s, pool, bins := service(t)
	ctx := context.Background()
	var pine string
	if err := pool.QueryRow(ctx, `INSERT INTO workspaces (name, timezone) VALUES ('Birch Dental', 'UTC') RETURNING id`).Scan(&pine); err != nil {
		t.Fatal(err)
	}
	first, _ := s.CreateInvite(ctx, bins, "r@example.com", "Morgan", nil)
	me, token := register(t, s, testutil.NewSoftPasskey(), first)

	second, _ := s.CreateInvite(ctx, pine, "r@example.com", "Morgan (Birch Dental)", nil)
	joined, err := s.AcceptInvite(ctx, token, second)
	if err != nil || joined.WorkspaceID != pine {
		t.Fatalf("accept = %+v, %v", joined, err)
	}
	if got, _ := s.Session(ctx, token); got.ID != joined.ID {
		t.Fatalf("session is %s after joining, want the new person %s", got.ID, joined.ID)
	}
	if _, err := s.AcceptInvite(ctx, token, second); !errors.Is(err, auth.ErrInviteInvalid) {
		t.Fatalf("used invite accepted again: %v", err)
	}
	third, _ := s.CreateInvite(ctx, pine, "r@example.com", "Morgan", nil)
	if _, err := s.AcceptInvite(ctx, token, third); !errors.Is(err, auth.ErrAlreadyMember) {
		t.Fatalf("joined the same workspace twice: %v", err)
	}

	if got := names(t, s, joined); len(got) != 2 || !got["Birch Dental"] || got["Acme Plumbing"] {
		t.Fatalf("workspaces = %v", got)
	}
	back, err := s.Switch(ctx, token, bins)
	if err != nil || back.ID != me.ID {
		t.Fatalf("switch back = %+v, %v", back, err)
	}
	if got, _ := s.Session(ctx, token); got.WorkspaceID != bins {
		t.Fatalf("session workspace %s after switching", got.WorkspaceID)
	}

	// Someone else's workspace, a revoked membership and junk are refused.
	var other string
	_ = pool.QueryRow(ctx, `INSERT INTO workspaces (name, timezone) VALUES ('Other', 'UTC') RETURNING id`).Scan(&other)
	for _, ws := range []string{other, "not-a-uuid"} {
		if _, err := s.Switch(ctx, token, ws); !errors.Is(err, auth.ErrNoWorkspace) {
			t.Fatalf("switched to %s: %v", ws, err)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE principals SET revoked_at = now() WHERE id = $1`, joined.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Switch(ctx, token, pine); !errors.Is(err, auth.ErrNoWorkspace) {
		t.Fatalf("switched into a workspace that removed this person: %v", err)
	}
}

func TestLinkingNeedsTheOtherWorkspacesPasskey(t *testing.T) {
	s, pool, bins := service(t)
	ctx := context.Background()
	var pine string
	_ = pool.QueryRow(ctx, `INSERT INTO workspaces (name, timezone) VALUES ('Birch Dental', 'UTC') RETURNING id`).Scan(&pine)
	a, _ := s.CreateInvite(ctx, bins, "r@example.com", "Morgan", nil)
	b, _ := s.CreateInvite(ctx, pine, "r@example.com", "Morgan (Birch Dental)", nil)
	binsKey, pineKey := testutil.NewSoftPasskey(), testutil.NewSoftPasskey()
	me, token := register(t, s, binsKey, a)
	register(t, s, pineKey, b)

	if _, err := s.Switch(ctx, token, pine); !errors.Is(err, auth.ErrNoWorkspace) {
		t.Fatalf("switched before linking: %v", err)
	}
	link := func(key *testutil.SoftPasskey) error {
		c, err := s.BeginLogin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		assertion, err := key.Authenticate(origin, c.Options)
		if err != nil {
			t.Fatal(err)
		}
		_, err = s.FinishLink(ctx, token, c.ID, assertion)
		return err
	}
	if err := link(binsKey); !errors.Is(err, auth.ErrAlreadyMember) {
		t.Fatalf("linking your own passkey: %v", err)
	}
	if err := link(pineKey); err != nil {
		t.Fatal(err)
	}
	if got := names(t, s, me); len(got) != 2 || !got["Acme Plumbing"] || got["Birch Dental"] {
		t.Fatalf("workspaces after linking = %v", got)
	}
	if p, err := s.Switch(ctx, token, pine); err != nil || p.Name != "Morgan (Birch Dental)" {
		t.Fatalf("switch after linking = %+v, %v", p, err)
	}
	var logins int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM logins`).Scan(&logins)
	if logins != 1 {
		t.Fatalf("%d logins after linking, want 1", logins)
	}
}
