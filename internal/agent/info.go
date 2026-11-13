package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/VisiOffice/VisiComms/internal/db"
)

// Approved info is the facts the business stands behind (hours, policies,
// articles), one JSON document per version. Agents read the latest approved
// version and may propose a new one, which only a person approves.

var (
	// ErrInfoChange is a proposal that is malformed or changes nothing.
	ErrInfoChange = errors.New("invalid change")
	// ErrInfoStale is a proposal based on a version that is no longer the
	// latest approved one.
	ErrInfoStale = errors.New("stale version")
)

// InfoSummary is what info_list shows: the latest approved version's
// sections and articles, without their full text, and the drafts waiting.
type InfoSummary struct {
	Version    int           `json:"version"`
	ApprovedAt *time.Time    `json:"approved_at,omitempty"`
	ApprovedBy string        `json:"approved_by,omitempty"`
	Sections   []InfoSection `json:"sections"`
	Articles   []InfoArticle `json:"articles,omitempty"`
	Pending    []InfoDraft   `json:"pending_drafts"`
	Note       string        `json:"note,omitempty"`
}

type InfoSection struct {
	Key     string `json:"key"`
	Summary string `json:"summary"`
}

type InfoArticle struct {
	Title    string   `json:"title"`
	Channels []string `json:"channels,omitempty"`
	Length   int      `json:"characters"`
}

type InfoDraft struct {
	Version    int       `json:"version"`
	BasedOn    *int      `json:"based_on,omitempty"`
	ProposedBy string    `json:"proposed_by,omitempty"`
	Reason     string    `json:"reason,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

type infoVersion struct {
	version    int
	content    map[string]json.RawMessage
	approvedAt *time.Time
	approvedBy string
}

func latestInfo(ctx context.Context, q db.Querier, workspaceID string) (infoVersion, error) {
	var v infoVersion
	var raw []byte
	err := q.QueryRow(ctx, `SELECT v.version, v.content, v.approved_at, coalesce(p.name, '')
		FROM approved_info_versions v LEFT JOIN principals p ON p.id = v.approved_by
		WHERE v.workspace_id = $1 AND v.approved_at IS NOT NULL ORDER BY v.version DESC LIMIT 1`, workspaceID).
		Scan(&v.version, &raw, &v.approvedAt, &v.approvedBy)
	if db.NotFound(err) {
		return infoVersion{content: map[string]json.RawMessage{}}, nil
	}
	if err != nil {
		return v, err
	}
	if err := json.Unmarshal(raw, &v.content); err != nil || v.content == nil {
		v.content = map[string]json.RawMessage{}
	}
	return v, nil
}

// Info lists the latest approved info and the drafts waiting for a person.
func Info(ctx context.Context, q db.Querier, workspaceID string) (InfoSummary, error) {
	v, err := latestInfo(ctx, q, workspaceID)
	if err != nil {
		return InfoSummary{}, err
	}
	out := InfoSummary{Version: v.version, ApprovedAt: v.approvedAt, ApprovedBy: v.approvedBy, Sections: []InfoSection{}}
	if v.version == 0 {
		out.Note = "No approved info yet: there is nothing an agent may quote. info_propose with based_on_version 0 drafts the first version."
	}
	for _, key := range sortedKeys(v.content) {
		if key == "articles" {
			for _, a := range articles(v.content) {
				out.Articles = append(out.Articles, InfoArticle{Title: str(a["title"]), Channels: strs(a["channels"]), Length: len(str(a["content"]))})
			}
			out.Sections = append(out.Sections, InfoSection{Key: key, Summary: fmt.Sprintf("%d articles", len(out.Articles))})
			continue
		}
		out.Sections = append(out.Sections, InfoSection{Key: key, Summary: summarize(v.content[key])})
	}
	rows, err := q.Query(ctx, `SELECT d.version, d.based_on, coalesce(p.name, ''), coalesce(d.reason, ''), d.created_at
		FROM approved_info_versions d LEFT JOIN principals p ON p.id = d.created_by
		WHERE d.workspace_id = $1 AND d.approved_at IS NULL AND d.version > $2 ORDER BY d.version`, workspaceID, v.version)
	if err != nil {
		return out, err
	}
	out.Pending, err = pgx.CollectRows(rows, pgx.RowToStructByPos[InfoDraft])
	if out.Pending == nil {
		out.Pending = []InfoDraft{}
	}
	return out, err
}

// InfoPart is one section or article of the latest approved info, or all
// of it when no section is named.
type InfoPart struct {
	Version int             `json:"version"`
	Section string          `json:"section,omitempty"`
	Content json.RawMessage `json:"content"`
}

// GetInfo reads a top-level section by key, an article by title (case does
// not matter), or the whole document.
func GetInfo(ctx context.Context, q db.Querier, workspaceID, section string) (InfoPart, error) {
	v, err := latestInfo(ctx, q, workspaceID)
	if err != nil {
		return InfoPart{}, err
	}
	section = strings.TrimSpace(section)
	if section == "" {
		all, _ := json.Marshal(v.content)
		return InfoPart{Version: v.version, Content: all}, nil
	}
	if raw, ok := v.content[section]; ok {
		return InfoPart{Version: v.version, Section: section, Content: raw}, nil
	}
	for _, a := range articles(v.content) {
		if strings.EqualFold(strings.TrimSpace(str(a["title"])), section) {
			raw, _ := json.Marshal(a)
			return InfoPart{Version: v.version, Section: str(a["title"]), Content: raw}, nil
		}
	}
	return InfoPart{}, fmt.Errorf("%w: no section or article %q in v%d; info_list shows what there is", ErrNotFound, section, v.version)
}

// ArticleChange adds, replaces or removes one article, matched by title.
type ArticleChange struct {
	Title    string   `json:"title"`
	Content  *string  `json:"content"`
	Channels []string `json:"channels"`
	Remove   bool     `json:"remove"`
}

// InfoProposal is a change to approved info, made on top of BasedOn.
type InfoProposal struct {
	BasedOn int `json:"based_on_version"`
	// Set replaces top-level sections; null removes one.
	Set      map[string]json.RawMessage `json:"set"`
	Articles []ArticleChange            `json:"articles"`
	Reason   string                     `json:"reason"`
}

// ProposedInfo is the draft version a proposal made.
type ProposedInfo struct {
	Version int    `json:"version"`
	BasedOn int    `json:"based_on"`
	Status  string `json:"status"`
	Result  string `json:"result"`
}

// ProposeInfo drafts a new version of approved info for a person to approve
// in the console. It must start from the latest approved version, and
// approving it is refused if another version is approved first, so an agent
// working from an old copy can never overwrite newer work. Nothing quotes
// the draft until a person approves it.
func ProposeInfo(ctx context.Context, pool *pgxpool.Pool, a Agent, p InfoProposal) (ProposedInfo, error) {
	p.Reason = strings.TrimSpace(p.Reason)
	if p.Reason == "" {
		return ProposedInfo{}, fmt.Errorf("%w: say why, for the person approving it", ErrInfoChange)
	}
	if len(p.Set) == 0 && len(p.Articles) == 0 {
		return ProposedInfo{}, fmt.Errorf("%w: give set or articles", ErrInfoChange)
	}
	if a.Autonomy == "observe" {
		return ProposedInfo{}, fmt.Errorf("%w: an observing agent cannot propose changes", ErrInfoChange)
	}
	var out ProposedInfo
	err := db.Tx(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, a.WorkspaceID+":approved_info"); err != nil {
			return err
		}
		v, err := latestInfo(ctx, tx, a.WorkspaceID)
		if err != nil {
			return err
		}
		if p.BasedOn != v.version {
			return fmt.Errorf("%w: v%d is the latest approved info, not v%d; read it again with info_get and propose on top of it", ErrInfoStale, v.version, p.BasedOn)
		}
		before := canonical(v.content)
		next := v.content
		for _, key := range sortedKeys(p.Set) {
			raw := p.Set[key]
			if strings.TrimSpace(key) == "" {
				return fmt.Errorf("%w: a section needs a key", ErrInfoChange)
			}
			if string(bytes.TrimSpace(raw)) == "null" {
				delete(next, key)
			} else {
				next[key] = raw
			}
		}
		if len(p.Articles) > 0 {
			list := articles(next)
			for _, c := range p.Articles {
				if list, err = applyArticle(list, c); err != nil {
					return err
				}
			}
			next["articles"], _ = json.Marshal(list)
		}
		after, _ := json.Marshal(next)
		if bytes.Equal(before, canonical(next)) {
			return fmt.Errorf("%w: that is what v%d already says", ErrInfoChange, v.version)
		}
		out.BasedOn = v.version
		return tx.QueryRow(ctx, `INSERT INTO approved_info_versions (workspace_id, version, content, created_by, based_on, reason)
			VALUES ($1, (SELECT coalesce(max(version), 0) + 1 FROM approved_info_versions WHERE workspace_id = $1), $2, $3, $4, $5)
			RETURNING version`, a.WorkspaceID, after, a.ID, v.version, p.Reason).Scan(&out.Version)
	})
	out.Status = "pending"
	out.Result = fmt.Sprintf("Draft v%d waits for a person to approve it in the console (Rules and approved info). Until then, v%d is what may be quoted.", out.Version, out.BasedOn)
	return out, err
}

// canonical is a document with every object's keys in one order, since
// Postgres stores jsonb keys in its own.
func canonical(content map[string]json.RawMessage) []byte {
	raw, _ := json.Marshal(content)
	var v any
	_ = json.Unmarshal(raw, &v)
	out, _ := json.Marshal(v)
	return out
}

func applyArticle(list []map[string]any, c ArticleChange) ([]map[string]any, error) {
	title := strings.TrimSpace(c.Title)
	if title == "" {
		return nil, fmt.Errorf("%w: an article needs a title", ErrInfoChange)
	}
	at := -1
	for i, a := range list {
		if strings.EqualFold(strings.TrimSpace(str(a["title"])), title) {
			at = i
		}
	}
	if c.Remove {
		if at < 0 {
			return nil, fmt.Errorf("%w: there is no article %q to remove", ErrInfoChange, title)
		}
		return append(list[:at], list[at+1:]...), nil
	}
	if at < 0 {
		if c.Content == nil || strings.TrimSpace(*c.Content) == "" {
			return nil, fmt.Errorf("%w: new article %q needs content", ErrInfoChange, title)
		}
		list, at = append(list, map[string]any{"title": title}), len(list)
	}
	if c.Content != nil {
		list[at]["content"] = strings.TrimSpace(*c.Content)
	}
	if c.Channels != nil {
		list[at]["channels"] = c.Channels
	}
	return list, nil
}

func articles(content map[string]json.RawMessage) []map[string]any {
	var list []map[string]any
	_ = json.Unmarshal(content["articles"], &list)
	if list == nil {
		list = []map[string]any{}
	}
	return list
}

func summarize(raw json.RawMessage) string {
	var v any
	_ = json.Unmarshal(raw, &v)
	switch x := v.(type) {
	case []any:
		return fmt.Sprintf("a list of %d", len(x))
	case map[string]any:
		return fmt.Sprintf("%d fields", len(x))
	case string:
		if r := []rune(x); len(r) > 80 {
			return string(r[:77]) + "..."
		}
		return x
	}
	return string(raw)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func strs(v any) []string {
	list, _ := v.([]any)
	var out []string
	for _, x := range list {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
