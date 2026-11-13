package slack

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/VisiOffice/VisiComms/internal/db"
	"github.com/VisiOffice/VisiComms/internal/secrets"
)

// ErrNotConnected means the workspace has no Slack connection.
var ErrNotConnected = errors.New("Slack is not connected")

// ErrNoKeys means credentials cannot be sealed on this installation.
var ErrNoKeys = errors.New("set VISICOMMS_MASTER_KEY before connecting Slack")

// FieldError is a setting that is missing or wrong, in words for the form.
type FieldError struct{ Msg string }

func (e *FieldError) Error() string { return e.Msg }

// Mention says when the mention is added: on a new chat only, on every
// message from the visitor, or never.
const (
	MentionNew   = "new"
	MentionEvery = "every"
	MentionNever = "never"
)

// Settings are the parts of the connection that are not secret.
type Settings struct {
	ChannelID string `json:"channel_id"`
	// Mention is put before a new chat, such as <!here> or <!subteam^S123>;
	// typed as @here, @channel or a Slack mention code.
	Mention   string `json:"mention"`
	MentionOn string `json:"mention_on"`
	TeamID    string `json:"team_id,omitempty"`
	TeamName  string `json:"team_name,omitempty"`
	BotUserID string `json:"bot_user_id,omitempty"`
}

type credentials struct {
	BotToken      string `json:"bot_token"`
	SigningSecret string `json:"signing_secret"`
}

// Connection is a workspace's Slack connector, with its credentials open.
type Connection struct {
	ID          string
	WorkspaceID string
	Settings
	Health    string
	LastError string
	creds     credentials
}

// Client returns a Web API client for the connection's bot.
func (c *Connection) Client(base *Client) *Client {
	cl := &Client{Token: c.creds.BotToken}
	if base != nil {
		cl.BaseURL, cl.HTTP = base.BaseURL, base.HTTP
	}
	return cl
}

// SigningSecret verifies requests from Slack.
func (c *Connection) SigningSecret() string { return c.creds.SigningSecret }

var channelID = regexp.MustCompile(`^[CG][A-Z0-9]{6,}$`)
var mentionCode = regexp.MustCompile(`^<(!here|!channel|!everyone|!subteam\^[A-Z0-9]+|@[UW][A-Z0-9]+)>$`)

// normalizeMention accepts what people type and returns Slack's code.
func normalizeMention(m string) (string, error) {
	m = strings.TrimSpace(m)
	switch strings.ToLower(m) {
	case "":
		return "", nil
	case "@here", "here":
		return "<!here>", nil
	case "@channel", "channel":
		return "<!channel>", nil
	}
	if mentionCode.MatchString(m) {
		return m, nil
	}
	return "", &FieldError{"Mention: use @here, @channel, or a Slack mention code such as <!subteam^S0123ABC> for a user group"}
}

// Input is what the settings form sends. Empty secrets keep the stored ones.
type Input struct {
	BotToken      string `json:"bot_token"`
	SigningSecret string `json:"signing_secret"`
	ChannelID     string `json:"channel_id"`
	Mention       string `json:"mention"`
	MentionOn     string `json:"mention_on"`
}

// Store reads and writes Slack connections.
type Store struct {
	DB   *pgxpool.Pool
	Keys *secrets.Keyring
	// API is the base client; tests point it at a fake Slack.
	API *Client
}

// Load returns the workspace's connection.
func (s *Store) Load(ctx context.Context, q db.Querier, workspaceID string) (*Connection, error) {
	return s.load(ctx, q, `workspace_id = $1`, workspaceID)
}

// ByID returns a connection by its connector id, for Slack's callbacks.
func (s *Store) ByID(ctx context.Context, q db.Querier, id string) (*Connection, error) {
	if !validUUID(id) {
		return nil, ErrNotConnected
	}
	return s.load(ctx, q, `id = $1`, id)
}

func (s *Store) load(ctx context.Context, q db.Querier, where, arg string) (*Connection, error) {
	if s.Keys == nil {
		return nil, ErrNotConnected
	}
	var c Connection
	var settings, sealed []byte
	err := q.QueryRow(ctx, `SELECT id, workspace_id, settings, credentials_ciphertext, health, coalesce(last_error, '')
		FROM connectors WHERE kind = 'slack' AND `+where, arg).Scan(&c.ID, &c.WorkspaceID, &settings, &sealed, &c.Health, &c.LastError)
	if db.NotFound(err) {
		return nil, ErrNotConnected
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(settings, &c.Settings); err != nil {
		return nil, err
	}
	plain, err := s.Keys.Open(ctx, q, c.WorkspaceID, c.ID, sealed)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(plain, &c.creds); err != nil {
		return nil, err
	}
	return &c, nil
}

// Save checks the token and channel with Slack, posts a test message, and
// stores the connection. Chats that started before it are not copied.
func (s *Store) Save(ctx context.Context, workspaceID string, in Input) (*Connection, error) {
	if s.Keys == nil {
		return nil, ErrNoKeys
	}
	in.ChannelID = strings.TrimSpace(in.ChannelID)
	in.BotToken = strings.TrimSpace(in.BotToken)
	in.SigningSecret = strings.TrimSpace(in.SigningSecret)
	if !channelID.MatchString(in.ChannelID) {
		return nil, &FieldError{"Channel ID: open the channel's details in Slack and copy the ID at the bottom, such as C0123ABCDEF"}
	}
	mention, err := normalizeMention(in.Mention)
	if err != nil {
		return nil, err
	}
	switch in.MentionOn {
	case "":
		in.MentionOn = MentionNew
	case MentionNew, MentionEvery, MentionNever:
	default:
		return nil, &FieldError{"Mention on: new, every or never"}
	}
	if in.BotToken != "" && !strings.HasPrefix(in.BotToken, "xoxb-") {
		return nil, &FieldError{"Bot token: use the Bot User OAuth Token, which starts with xoxb-"}
	}

	existing, err := s.Load(ctx, s.DB, workspaceID)
	if err != nil && !errors.Is(err, ErrNotConnected) {
		return nil, err
	}
	creds := credentials{BotToken: in.BotToken, SigningSecret: in.SigningSecret}
	if existing != nil {
		if creds.BotToken == "" {
			creds.BotToken = existing.creds.BotToken
		}
		if creds.SigningSecret == "" {
			creds.SigningSecret = existing.creds.SigningSecret
		}
	}
	if creds.BotToken == "" {
		return nil, &FieldError{"Bot token is required"}
	}
	if creds.SigningSecret == "" {
		return nil, &FieldError{"Signing secret is required: it is on the app's Basic Information page"}
	}

	c := &Connection{WorkspaceID: workspaceID, creds: creds, Health: "ok"}
	cl := c.Client(s.API)
	id, err := cl.AuthTest(ctx)
	if err != nil {
		return nil, &FieldError{err.Error()}
	}
	if _, err := cl.Post(ctx, in.ChannelID, "VisiComms is connected. New website chats will appear in this channel, and replies in a chat's thread go to the visitor.", ""); err != nil {
		return nil, &FieldError{err.Error()}
	}
	c.Settings = Settings{ChannelID: in.ChannelID, Mention: mention, MentionOn: in.MentionOn, TeamID: id.TeamID, TeamName: id.Team, BotUserID: id.BotUserID}
	settings, err := json.Marshal(c.Settings)
	if err != nil {
		return nil, err
	}
	plain, err := json.Marshal(creds)
	if err != nil {
		return nil, err
	}
	err = db.Tx(ctx, s.DB, func(tx pgx.Tx) error {
		if existing != nil {
			c.ID = existing.ID
			if _, err := tx.Exec(ctx, `UPDATE connectors SET settings = $2, health = 'ok', last_error = NULL, checked_at = now(), updated_at = now() WHERE id = $1`, c.ID, settings); err != nil {
				return err
			}
		} else if err := tx.QueryRow(ctx, `INSERT INTO connectors (workspace_id, kind, name, settings, health, checked_at)
			VALUES ($1, 'slack', 'Slack', $2, 'ok', now()) RETURNING id`, workspaceID, settings).Scan(&c.ID); err != nil {
			return err
		}
		sealed, err := s.Keys.Seal(ctx, tx, workspaceID, c.ID, plain)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE connectors SET credentials_ciphertext = $2 WHERE id = $1`, c.ID, sealed)
		return err
	})
	if err != nil {
		return nil, err
	}
	return c, nil
}

// Delete disconnects Slack. Chats already copied stay in Slack.
func (s *Store) Delete(ctx context.Context, workspaceID string) error {
	_, err := s.DB.Exec(ctx, `DELETE FROM connectors WHERE workspace_id = $1 AND kind = 'slack'`, workspaceID)
	return err
}

// SetHealth records whether Slack is accepting the bot's posts.
func SetHealth(ctx context.Context, q db.Querier, connectorID string, err error) {
	if err == nil {
		_, _ = q.Exec(ctx, `UPDATE connectors SET health = 'ok', last_error = NULL, checked_at = now() WHERE id = $1 AND health <> 'ok'`, connectorID)
		return
	}
	_, _ = q.Exec(ctx, `UPDATE connectors SET health = 'failing', last_error = $2, fix_owner = 'customer', checked_at = now() WHERE id = $1`, connectorID, err.Error())
}

// Member is a workspace person and the Slack user they are linked to.
type Member struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Email       string `json:"email"`
	SlackUserID string `json:"slack_user_id"`
}

// Members lists the people who can reply from Slack once linked.
func Members(ctx context.Context, q db.Querier, workspaceID string) ([]Member, error) {
	rows, err := q.Query(ctx, `SELECT id, name, coalesce(email, ''), coalesce(slack_user_id, '') FROM principals
		WHERE workspace_id = $1 AND kind = 'human' AND revoked_at IS NULL ORDER BY name`, workspaceID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Member])
}

var slackUserID = regexp.MustCompile(`^[UW][A-Z0-9]{6,}$`)

// Link sets or clears the Slack user a member replies as.
func Link(ctx context.Context, q db.Querier, workspaceID, principalID, slackUser string) error {
	slackUser = strings.TrimSpace(slackUser)
	if slackUser != "" && !slackUserID.MatchString(slackUser) {
		return &FieldError{"Slack member ID: in Slack, open the person's profile, choose More, then Copy member ID; it looks like U0123ABCDEF"}
	}
	if !validUUID(principalID) {
		return ErrNotConnected
	}
	tag, err := q.Exec(ctx, `UPDATE principals SET slack_user_id = nullif($3, '') WHERE id = $1 AND workspace_id = $2 AND kind = 'human'`, principalID, workspaceID, slackUser)
	if err != nil {
		if db.IsUniqueViolation(err) {
			return &FieldError{"That Slack member is already linked to someone else"}
		}
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotConnected
	}
	return nil
}

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func validUUID(s string) bool { return uuidPattern.MatchString(s) }
