package auth_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/VisiOffice/VisiComms/internal/auth"
	"github.com/VisiOffice/VisiComms/internal/testutil"
)

const origin = "http://localhost:5173"

func service(t *testing.T) (*auth.Service, *pgxpool.Pool, string) {
	t.Helper()
	pool := testutil.DB(t)
	s, err := auth.New(pool, auth.Config{RPID: "localhost", Origins: []string{origin}})
	if err != nil {
		t.Fatal(err)
	}
	var ws string
	if err := pool.QueryRow(context.Background(), `INSERT INTO workspaces (name, timezone) VALUES ('Acme Plumbing', 'UTC') RETURNING id`).Scan(&ws); err != nil {
		t.Fatal(err)
	}
	return s, pool, ws
}

func register(t *testing.T, s *auth.Service, key *testutil.SoftPasskey, invite string) (auth.Person, string) {
	t.Helper()
	ctx := context.Background()
	c, err := s.BeginRegistration(ctx, invite)
	if err != nil {
		t.Fatal(err)
	}
	cred, err := key.Register(origin, c.Options)
	if err != nil {
		t.Fatal(err)
	}
	p, token, err := s.FinishRegistration(ctx, c.ID, cred)
	if err != nil {
		t.Fatal(err)
	}
	return p, token
}

func TestInviteRegisterSignInSignOut(t *testing.T) {
	s, _, ws := service(t)
	ctx := context.Background()
	invite, err := s.CreateInvite(ctx, ws, " Morgan@Example.com ", "Morgan", nil)
	if err != nil {
		t.Fatal(err)
	}
	in, err := s.LookupInvite(ctx, invite)
	if err != nil || in.Email != "morgan@example.com" || in.WorkspaceName != "Acme Plumbing" {
		t.Fatalf("invite = %+v, %v", in, err)
	}

	key := testutil.NewSoftPasskey()
	p, token := register(t, s, key, invite)
	if p.WorkspaceID != ws || p.Name != "Morgan" {
		t.Fatalf("registered %+v", p)
	}
	if got, err := s.Session(ctx, token); err != nil || got.ID != p.ID {
		t.Fatalf("session = %+v, %v", got, err)
	}
	if _, err := s.LookupInvite(ctx, invite); !errors.Is(err, auth.ErrInviteInvalid) {
		t.Fatal("invite still usable after registering")
	}
	if _, err := s.BeginRegistration(ctx, invite); !errors.Is(err, auth.ErrInviteInvalid) {
		t.Fatal("a used invite started another registration")
	}

	c, err := s.BeginLogin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	assertion, err := key.Authenticate(origin, c.Options)
	if err != nil {
		t.Fatal(err)
	}
	again, token2, err := s.FinishLogin(ctx, c.ID, assertion)
	if err != nil || again.ID != p.ID {
		t.Fatalf("login = %+v, %v", again, err)
	}
	// A challenge answers once.
	if _, _, err := s.FinishLogin(ctx, c.ID, assertion); !errors.Is(err, auth.ErrDenied) {
		t.Fatalf("replayed assertion: %v", err)
	}

	if err := s.EndSession(ctx, token2); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Session(ctx, token2); !errors.Is(err, auth.ErrDenied) {
		t.Fatal("signed-out session still works")
	}
}

func TestWrongOriginIsRefused(t *testing.T) {
	s, _, ws := service(t)
	ctx := context.Background()
	invite, _ := s.CreateInvite(ctx, ws, "a@example.com", "A", nil)
	c, err := s.BeginRegistration(ctx, invite)
	if err != nil {
		t.Fatal(err)
	}
	cred, err := testutil.NewSoftPasskey().Register("https://evil.example", c.Options)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.FinishRegistration(ctx, c.ID, cred); !errors.Is(err, auth.ErrDenied) {
		t.Fatalf("phishing origin registered: %v", err)
	}
}

func TestRevokedPersonCannotSignIn(t *testing.T) {
	s, pool, ws := service(t)
	ctx := context.Background()
	invite, _ := s.CreateInvite(ctx, ws, "a@example.com", "A", nil)
	key := testutil.NewSoftPasskey()
	p, token := register(t, s, key, invite)
	if _, err := pool.Exec(ctx, `UPDATE principals SET revoked_at = now() WHERE id = $1`, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Session(ctx, token); !errors.Is(err, auth.ErrDenied) {
		t.Fatal("revoked person's session still works")
	}
	c, _ := s.BeginLogin(ctx)
	assertion, _ := key.Authenticate(origin, c.Options)
	if _, _, err := s.FinishLogin(ctx, c.ID, assertion); !errors.Is(err, auth.ErrDenied) {
		t.Fatalf("revoked person signed in: %v", err)
	}
}
