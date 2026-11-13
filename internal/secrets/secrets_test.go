package secrets_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/VisiOffice/VisiComms/internal/secrets"
	"github.com/VisiOffice/VisiComms/internal/testutil"
)

func TestParseMasterKey(t *testing.T) {
	raw := bytes.Repeat([]byte{7}, 32)
	for _, in := range []string{strings.Repeat("07", 32), base64.StdEncoding.EncodeToString(raw), " " + strings.Repeat("07", 32) + "\n"} {
		if key, err := secrets.ParseMasterKey(in); err != nil || !bytes.Equal(key, raw) {
			t.Fatalf("ParseMasterKey(%q)=%x,%v", in, key, err)
		}
	}
	for _, in := range []string{"", "short", strings.Repeat("07", 31), base64.StdEncoding.EncodeToString(raw[:16])} {
		if _, err := secrets.ParseMasterKey(in); !errors.Is(err, secrets.ErrMasterKey) {
			t.Fatalf("ParseMasterKey(%q) accepted", in)
		}
	}
}

func TestSealBindsWorkspaceAndConnector(t *testing.T) {
	pool := testutil.DB(t)
	ctx := context.Background()
	var ws, other string
	for _, id := range []*string{&ws, &other} {
		if err := pool.QueryRow(ctx, `INSERT INTO workspaces (name, timezone) VALUES ('Sealed', 'UTC') RETURNING id`).Scan(id); err != nil {
			t.Fatal(err)
		}
	}
	keys, _ := secrets.New(bytes.Repeat([]byte{1}, 32))
	connector, another := uuid.NewString(), uuid.NewString()
	plaintext := []byte(`{"token":"PT-secret"}`)

	sealed, err := keys.Seal(ctx, pool, ws, connector, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("PT-secret")) {
		t.Fatal("ciphertext contains the plaintext")
	}
	if again, err := keys.Seal(ctx, pool, ws, connector, plaintext); err != nil || bytes.Equal(again, sealed) {
		t.Fatal("sealing twice should reuse the data key with a fresh nonce")
	}
	if got, err := keys.Open(ctx, pool, ws, connector, sealed); err != nil || !bytes.Equal(got, plaintext) {
		t.Fatalf("Open=%s,%v", got, err)
	}
	if _, err := keys.Open(ctx, pool, ws, another, sealed); !errors.Is(err, secrets.ErrOpen) {
		t.Fatalf("opened on another connector: %v", err)
	}
	if _, err := keys.Seal(ctx, pool, other, another, plaintext); err != nil {
		t.Fatal(err)
	}
	if _, err := keys.Open(ctx, pool, other, connector, sealed); !errors.Is(err, secrets.ErrOpen) {
		t.Fatalf("opened in another workspace: %v", err)
	}
	wrong, _ := secrets.New(bytes.Repeat([]byte{2}, 32))
	if _, err := wrong.Open(ctx, pool, ws, connector, sealed); !errors.Is(err, secrets.ErrOpen) {
		t.Fatalf("opened with the wrong master key: %v", err)
	}
	// A data key swapped in from another workspace does not open either.
	if _, err := pool.Exec(ctx, `UPDATE workspace_keys SET sealed_key = (SELECT sealed_key FROM workspace_keys WHERE workspace_id = $2) WHERE workspace_id = $1`, ws, other); err != nil {
		t.Fatal(err)
	}
	if _, err := keys.Open(ctx, pool, ws, connector, sealed); !errors.Is(err, secrets.ErrOpen) {
		t.Fatalf("opened with a swapped data key: %v", err)
	}
	var nilKeys *secrets.Keyring
	if _, err := nilKeys.Seal(ctx, pool, ws, connector, plaintext); err == nil {
		t.Fatal("sealed without a master key")
	}
}
