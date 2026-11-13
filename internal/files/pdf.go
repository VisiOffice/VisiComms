// Package files stores immutable PDF documents and temporary provider grants.
package files

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path"
	"strings"

	"github.com/VisiOffice/VisiComms/internal/db"
)

const MaxPDF = 10 << 20

var ErrPDF = errors.New("upload a PDF no larger than 10 MB")

type Document struct {
	ID       string `json:"id"`
	Filename string `json:"filename"`
	SHA256   string `json:"sha256"`
	Size     int    `json:"size"`
	Content  []byte `json:"-"`
}

func ValidatePDF(data []byte) error {
	if len(data) > MaxPDF || !bytes.HasPrefix(data, []byte("%PDF-")) {
		return ErrPDF
	}
	return nil
}
func Store(ctx context.Context, q db.Querier, workspaceID, filename string, data []byte) (Document, error) {
	var d Document
	if err := ValidatePDF(data); err != nil {
		return d, err
	}
	filename = path.Base(strings.ReplaceAll(filename, "\\", "/"))
	filename = strings.ReplaceAll(strings.ReplaceAll(filename, "\r", ""), "\n", "")
	if filename == "." || filename == "" || len(filename) > 200 {
		filename = "document.pdf"
	}
	checksum := sha256.Sum256(data)
	d.Filename, d.SHA256, d.Size = filename, hex.EncodeToString(checksum[:]), len(data)
	err := q.QueryRow(ctx, `INSERT INTO documents(workspace_id,filename,content,sha256) VALUES($1,$2,$3,$4) RETURNING id`, workspaceID, filename, data, checksum[:]).Scan(&d.ID)
	return d, err
}
func Load(ctx context.Context, q db.Querier, workspaceID, id string) (Document, error) {
	var d Document
	var checksum []byte
	err := q.QueryRow(ctx, `SELECT id,filename,content,sha256 FROM documents WHERE workspace_id=$1 AND id=$2`, workspaceID, id).Scan(&d.ID, &d.Filename, &d.Content, &checksum)
	d.SHA256, d.Size = hex.EncodeToString(checksum), len(d.Content)
	return d, err
}
func Grant(ctx context.Context, q db.Querier, workspaceID, documentID, actionID string) (string, error) {
	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		return "", err
	}
	raw := hex.EncodeToString(token)
	checksum := sha256.Sum256([]byte(raw))
	_, err := q.Exec(ctx, `INSERT INTO document_grants(token_sha256,workspace_id,document_id,action_id) VALUES($1,$2,$3,$4)`, checksum[:], workspaceID, documentID, actionID)
	return raw, err
}
func LoadGrant(ctx context.Context, q db.Querier, token string) (Document, error) {
	var d Document
	var checksum []byte
	hash := sha256.Sum256([]byte(token))
	err := q.QueryRow(ctx, `SELECT d.id,d.filename,d.content,d.sha256 FROM document_grants g
 JOIN documents d ON d.id=g.document_id AND d.workspace_id=g.workspace_id JOIN actions a ON a.id=g.action_id
 WHERE g.token_sha256=$1 AND g.expires_at>now() AND a.workspace_id=g.workspace_id
 AND a.action='fax.send' AND a.state IN ('dispatching','accepted','unknown')`, hash[:]).Scan(&d.ID, &d.Filename, &d.Content, &checksum)
	d.SHA256, d.Size = hex.EncodeToString(checksum), len(d.Content)
	return d, err
}
