// Package secrets seals connector credentials. Each workspace has a random
// data key, itself sealed with the installation master key
// (VISICOMMS_MASTER_KEY). Credentials are sealed with the workspace's data key
// and bound to their connector, so a ciphertext copied onto another connector
// or workspace fails to open. Losing the master key means re-entering
// credentials; nothing else depends on it.
package secrets

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/VisiOffice/VisiComms/internal/db"
)

// version prefixes every sealed value so the format can change later.
const version = 1

var ErrMasterKey = errors.New("VISICOMMS_MASTER_KEY must be 32 bytes, as 64 hex characters or base64")

// ErrOpen means a value did not decrypt: the wrong master key, or a ciphertext
// moved from where it was sealed.
var ErrOpen = errors.New("stored credentials could not be decrypted; re-enter them")

// Keyring holds the master key. A nil Keyring stores nothing.
type Keyring struct{ master cipher.AEAD }

// ParseMasterKey accepts 64 hex characters or standard base64 of 32 bytes.
func ParseMasterKey(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if key, err := hex.DecodeString(s); err == nil && len(key) == 32 {
		return key, nil
	}
	if key, err := base64.StdEncoding.DecodeString(s); err == nil && len(key) == 32 {
		return key, nil
	}
	return nil, ErrMasterKey
}

func New(master []byte) (*Keyring, error) {
	if len(master) != 32 {
		return nil, ErrMasterKey
	}
	aead, err := newAEAD(master)
	if err != nil {
		return nil, err
	}
	return &Keyring{master: aead}, nil
}

// Seal encrypts plaintext for one connector, creating the workspace's data key
// on first use. Run it in the transaction that stores the result.
func (k *Keyring) Seal(ctx context.Context, q db.Querier, workspaceID, connectorID string, plaintext []byte) ([]byte, error) {
	dataKey, err := k.dataKey(ctx, q, workspaceID, true)
	if err != nil {
		return nil, err
	}
	return seal(dataKey, plaintext, connectorAD(connectorID))
}

func (k *Keyring) Open(ctx context.Context, q db.Querier, workspaceID, connectorID string, sealed []byte) ([]byte, error) {
	dataKey, err := k.dataKey(ctx, q, workspaceID, false)
	if err != nil {
		return nil, err
	}
	return open(dataKey, sealed, connectorAD(connectorID))
}

func (k *Keyring) dataKey(ctx context.Context, q db.Querier, workspaceID string, create bool) (cipher.AEAD, error) {
	if k == nil {
		return nil, errors.New("VISICOMMS_MASTER_KEY is not set: workspace credentials cannot be stored or read")
	}
	if create {
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return nil, err
		}
		sealed, err := seal(k.master, raw, workspaceAD(workspaceID))
		if err != nil {
			return nil, err
		}
		if _, err := q.Exec(ctx, `INSERT INTO workspace_keys (workspace_id, sealed_key) VALUES ($1, $2)
			ON CONFLICT (workspace_id) DO NOTHING`, workspaceID, sealed); err != nil {
			return nil, err
		}
	}
	var sealed []byte
	if err := q.QueryRow(ctx, `SELECT sealed_key FROM workspace_keys WHERE workspace_id = $1`, workspaceID).Scan(&sealed); err != nil {
		if db.NotFound(err) {
			return nil, ErrOpen
		}
		return nil, err
	}
	raw, err := open(k.master, sealed, workspaceAD(workspaceID))
	if err != nil {
		return nil, err
	}
	return newAEAD(raw)
}

func workspaceAD(id string) []byte { return []byte("visicomms workspace key " + id) }
func connectorAD(id string) []byte { return []byte("visicomms connector " + id) }

func newAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// seal returns version || nonce || ciphertext. Nonces are random: a data
// key seals a handful of values, far below GCM's random-nonce limit.
func seal(aead cipher.AEAD, plaintext, ad []byte) ([]byte, error) {
	out := make([]byte, 1+aead.NonceSize(), 1+aead.NonceSize()+len(plaintext)+aead.Overhead())
	out[0] = version
	if _, err := rand.Read(out[1:]); err != nil {
		return nil, err
	}
	return aead.Seal(out, out[1:], plaintext, ad), nil
}

func open(aead cipher.AEAD, sealed, ad []byte) ([]byte, error) {
	n := aead.NonceSize()
	if len(sealed) < 1+n || sealed[0] != version {
		return nil, ErrOpen
	}
	plaintext, err := aead.Open(nil, sealed[1:1+n], sealed[1+n:], ad)
	if err != nil {
		return nil, ErrOpen
	}
	return plaintext, nil
}
