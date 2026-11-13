// Package oauth is the authorization server for the MCP endpoint, so hosted
// clients such as Claude can add VisiComms as a connector. It does the
// authorization code flow with PKCE (S256 only), dynamic client registration
// (RFC 7591), the discovery documents (RFC 8414, RFC 9728), refresh token
// rotation and revocation (RFC 7009).
//
// A person approves a client in the console for the workspaces they pick.
// The approval (a grant) makes one agent principal in each of them, in
// approve mode, so everything the client drafts waits for a person and its
// audit trail is its own. Revoking the grant, or one of its agents, stops it.
package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/VisiOffice/VisiComms/internal/agent"
	"github.com/VisiOffice/VisiComms/internal/db"
)

const (
	clientPrefix  = "vcoc_"
	accessPrefix  = "vcoa_"
	refreshPrefix = "vcor_"

	// AccessTTL is short enough that a leaked token expires on its own;
	// clients refresh without asking the person again.
	AccessTTL  = 24 * time.Hour
	RefreshTTL = 90 * 24 * time.Hour
	codeTTL    = 10 * time.Minute
	requestTTL = 30 * time.Minute

	// Scope is the only scope: the MCP tools, limited by each agent's mode.
	Scope = "mcp"
)

// nativeRedirects are app callbacks allowed besides HTTPS and loopback.
// PKCE protects the code; the list keeps any other app scheme out.
var nativeRedirects = []string{"cursor://anysphere.cursor-mcp/oauth/callback"}

// Server issues and checks OAuth tokens.
type Server struct {
	DB *pgxpool.Pool
	// Issuer is the API's public origin, such as https://api.visicomms.com.
	Issuer string
	// AppURL is the console, where people approve a client.
	AppURL string
}

// Resource is the MCP endpoint that tokens are for.
func (s *Server) Resource() string { return s.Issuer + "/mcp" }

// ProtectedResource is the RFC 9728 document for the MCP endpoint.
func (s *Server) ProtectedResource() map[string]any {
	return map[string]any{
		"resource":                 s.Resource(),
		"authorization_servers":    []string{s.Issuer},
		"scopes_supported":         []string{Scope},
		"bearer_methods_supported": []string{"header"},
		"resource_name":            "VisiComms",
	}
}

// Metadata is the RFC 8414 document for the authorization server.
func (s *Server) Metadata() map[string]any {
	return map[string]any{
		"issuer":                                s.Issuer,
		"authorization_endpoint":                s.Issuer + "/oauth/authorize",
		"token_endpoint":                        s.Issuer + "/oauth/token",
		"registration_endpoint":                 s.Issuer + "/oauth/register",
		"revocation_endpoint":                   s.Issuer + "/oauth/revoke",
		"scopes_supported":                      []string{Scope},
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"token_endpoint_auth_methods_supported": []string{"none", "client_secret_post", "client_secret_basic"},
		"code_challenge_methods_supported":      []string{"S256"},
	}
}

// Error is an OAuth error response: a code from the RFCs and words for a
// person reading logs.
type Error struct {
	Code        string `json:"error"`
	Description string `json:"error_description"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Description }

func oauthErr(code, description string) *Error { return &Error{code, description} }

// Registration is the RFC 7591 request.
type Registration struct {
	ClientName              string   `json:"client_name"`
	RedirectURIs            []string `json:"redirect_uris"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
}

// Register records a client. Registering is open: it only earns the right
// to ask a person, who approves it in the console.
func (s *Server) Register(ctx context.Context, in Registration) (map[string]any, error) {
	name := strings.TrimSpace(in.ClientName)
	if name == "" {
		name = "MCP client"
	}
	if utf8.RuneCountInString(name) > 80 {
		name = string([]rune(name)[:80])
	}
	if len(in.RedirectURIs) == 0 || len(in.RedirectURIs) > 10 {
		return nil, oauthErr("invalid_redirect_uri", "register between 1 and 10 redirect_uris")
	}
	for _, u := range in.RedirectURIs {
		if !redirectAllowed(u) {
			return nil, oauthErr("invalid_redirect_uri", "redirect_uris must be https, http on loopback, or a known app callback: "+u)
		}
	}
	method := in.TokenEndpointAuthMethod
	switch method {
	case "":
		method = "none"
	case "none", "client_secret_post", "client_secret_basic":
	default:
		return nil, oauthErr("invalid_client_metadata", "token_endpoint_auth_method must be none, client_secret_post or client_secret_basic")
	}
	id := clientPrefix + random()
	var secret string
	var secretHash []byte
	if method != "none" {
		secret = random()
		secretHash = digest(secret)
	}
	var created time.Time
	if err := s.DB.QueryRow(ctx, `INSERT INTO oauth_clients (id, name, redirect_uris, secret_sha256) VALUES ($1, $2, $3, $4) RETURNING created_at`,
		id, name, in.RedirectURIs, secretHash).Scan(&created); err != nil {
		return nil, err
	}
	out := map[string]any{
		"client_id":                  id,
		"client_id_issued_at":        created.Unix(),
		"client_name":                name,
		"redirect_uris":              in.RedirectURIs,
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": method,
		"scope":                      Scope,
	}
	if secret != "" {
		out["client_secret"] = secret
		out["client_secret_expires_at"] = 0
	}
	return out, nil
}

func redirectAllowed(raw string) bool {
	if slices.Contains(nativeRedirects, raw) {
		return true
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Fragment != "" {
		return false
	}
	switch u.Scheme {
	case "https":
		return true
	case "http":
		h := u.Hostname()
		return h == "localhost" || h == "127.0.0.1" || h == "::1"
	}
	return false
}

// ErrBadRedirect means the client or its redirect_uri is unknown. The
// error is shown in the browser and never sent to the redirect_uri, or an
// attacker could collect codes at a URI of their choosing.
var ErrBadRedirect = errors.New("unknown client_id, or a redirect_uri it did not register")

// Authorize checks an authorization request and returns where to send the
// browser: the console's consent page, or back to the client with an error.
func (s *Server) Authorize(ctx context.Context, q url.Values) (string, error) {
	clientID, redirect := q.Get("client_id"), q.Get("redirect_uri")
	var uris []string
	err := s.DB.QueryRow(ctx, `SELECT redirect_uris FROM oauth_clients WHERE id = $1`, clientID).Scan(&uris)
	if db.NotFound(err) {
		return "", ErrBadRedirect
	}
	if err != nil {
		return "", err
	}
	if redirect == "" && len(uris) == 1 {
		redirect = uris[0]
	}
	if !slices.Contains(uris, redirect) {
		return "", ErrBadRedirect
	}
	state := q.Get("state")
	fail := func(code, description string) (string, error) {
		return withQuery(redirect, "error", code, "error_description", description, "state", state), nil
	}
	if q.Get("response_type") != "code" {
		return fail("unsupported_response_type", "only response_type=code is supported")
	}
	challenge := q.Get("code_challenge")
	if challenge == "" || q.Get("code_challenge_method") != "S256" {
		return fail("invalid_request", "PKCE is required, with code_challenge_method=S256")
	}
	if len(challenge) < 43 || len(challenge) > 128 {
		return fail("invalid_request", "code_challenge must be a base64url SHA-256")
	}
	if r := q.Get("resource"); r != "" && strings.TrimSuffix(r, "/") != s.Resource() {
		return fail("invalid_target", "resource must be "+s.Resource())
	}
	if sc := q.Get("scope"); sc != "" && !slices.Contains(strings.Fields(sc), Scope) {
		return fail("invalid_scope", "the only scope is "+Scope)
	}
	id := random()
	if _, err := s.DB.Exec(ctx, `DELETE FROM oauth_requests WHERE expires_at < now()`); err != nil {
		return "", err
	}
	if _, err := s.DB.Exec(ctx, `INSERT INTO oauth_requests (id, client_id, redirect_uri, state, code_challenge, expires_at)
		VALUES ($1, $2, $3, nullif($4, ''), $5, now() + make_interval(secs => $6))`, id, clientID, redirect, state, challenge, requestTTL.Seconds()); err != nil {
		return "", err
	}
	return s.AppURL + "/authorize/" + id, nil
}

// ErrNoRequest means the request is unknown or expired.
var ErrNoRequest = errors.New("this authorization request has expired; start again from the app you were connecting")

// Workspace is one the person can allow.
type Workspace struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Request is what the consent page shows.
type Request struct {
	ClientName   string      `json:"client_name"`
	RedirectHost string      `json:"redirect_host"`
	Workspaces   []Workspace `json:"workspaces"`
}

// memberships are the workspaces the person's login belongs to, with the
// person's principal in each.
func memberships(ctx context.Context, q db.Querier, personID string) (map[string]string, []Workspace, error) {
	rows, err := q.Query(ctx, `SELECT w.id, w.name, other.id
		FROM principals me JOIN principals other ON other.id = me.id OR other.login_id = me.login_id
		JOIN workspaces w ON w.id = other.workspace_id
		WHERE me.id = $1 AND other.kind = 'human' AND other.revoked_at IS NULL
		ORDER BY lower(w.name), w.id`, personID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	people := map[string]string{}
	var list []Workspace
	for rows.Next() {
		var w Workspace
		var person string
		if err := rows.Scan(&w.ID, &w.Name, &person); err != nil {
			return nil, nil, err
		}
		people[w.ID] = person
		list = append(list, w)
	}
	return people, list, rows.Err()
}

// Pending loads a request for the consent page.
func (s *Server) Pending(ctx context.Context, requestID, personID string) (Request, error) {
	var r Request
	var redirect string
	err := s.DB.QueryRow(ctx, `SELECT c.name, r.redirect_uri FROM oauth_requests r JOIN oauth_clients c ON c.id = r.client_id
		WHERE r.id = $1 AND r.expires_at > now()`, requestID).Scan(&r.ClientName, &redirect)
	if db.NotFound(err) {
		return r, ErrNoRequest
	}
	if err != nil {
		return r, err
	}
	if u, err := url.Parse(redirect); err == nil {
		r.RedirectHost = u.Host
		if r.RedirectHost == "" {
			r.RedirectHost = u.Scheme
		}
	}
	_, r.Workspaces, err = memberships(ctx, s.DB, personID)
	return r, err
}

// ErrNoWorkspace means none of the chosen workspaces is the person's.
var ErrNoWorkspace = errors.New("choose at least one of your workspaces")

// Approve records the person's approval for the chosen workspaces and
// returns the client's redirect with a one-time code.
func (s *Server) Approve(ctx context.Context, requestID, personID string, workspaceIDs []string) (string, error) {
	var redirect string
	err := db.Tx(ctx, s.DB, func(tx pgx.Tx) error {
		var clientID, clientName, challenge string
		var state *string
		err := tx.QueryRow(ctx, `DELETE FROM oauth_requests r USING oauth_clients c
			WHERE r.id = $1 AND c.id = r.client_id AND r.expires_at > now()
			RETURNING r.client_id, c.name, r.redirect_uri, r.state, r.code_challenge`, requestID).
			Scan(&clientID, &clientName, &redirect, &state, &challenge)
		if db.NotFound(err) {
			return ErrNoRequest
		}
		if err != nil {
			return err
		}
		people, _, err := memberships(ctx, tx, personID)
		if err != nil {
			return err
		}
		var chosen []string
		for _, id := range workspaceIDs {
			if people[id] != "" && !slices.Contains(chosen, id) {
				chosen = append(chosen, id)
			}
		}
		if len(chosen) == 0 {
			return ErrNoWorkspace
		}
		var grantID string
		if err := tx.QueryRow(ctx, `INSERT INTO oauth_grants (client_id, approved_by) VALUES ($1, $2) RETURNING id`,
			clientID, personID).Scan(&grantID); err != nil {
			return err
		}
		for _, ws := range chosen {
			var first string
			if err := tx.QueryRow(ctx, `SELECT split_part(name, ' ', 1) FROM principals WHERE id = $1`, people[ws]).Scan(&first); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO principals (workspace_id, kind, name, autonomy, created_by, oauth_grant_id)
				VALUES ($1, 'agent', $2, 'approve', $3, $4)`, ws, clientName+" ("+first+")", people[ws], grantID); err != nil {
				return err
			}
		}
		code := random()
		if _, err := tx.Exec(ctx, `INSERT INTO oauth_codes (code_sha256, grant_id, redirect_uri, code_challenge, expires_at)
			VALUES ($1, $2, $3, $4, now() + make_interval(secs => $5))`, digest(code), grantID, redirect, challenge, codeTTL.Seconds()); err != nil {
			return err
		}
		st := ""
		if state != nil {
			st = *state
		}
		redirect = withQuery(redirect, "code", code, "state", st, "iss", s.Issuer)
		return nil
	})
	return redirect, err
}

// Deny sends the person back to the client without access.
func (s *Server) Deny(ctx context.Context, requestID string) (string, error) {
	var redirect string
	var state *string
	err := s.DB.QueryRow(ctx, `DELETE FROM oauth_requests WHERE id = $1 RETURNING redirect_uri, state`, requestID).Scan(&redirect, &state)
	if db.NotFound(err) {
		return "", ErrNoRequest
	}
	if err != nil {
		return "", err
	}
	st := ""
	if state != nil {
		st = *state
	}
	return withQuery(redirect, "error", "access_denied", "error_description", "The person declined", "state", st), nil
}

// TokenResponse is what the token endpoint returns.
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
}

// client checks the client's credentials: none for a public client, the
// secret for a confidential one.
func (s *Server) client(ctx context.Context, id, secret string) error {
	var hash []byte
	err := s.DB.QueryRow(ctx, `SELECT secret_sha256 FROM oauth_clients WHERE id = $1`, id).Scan(&hash)
	if db.NotFound(err) {
		return oauthErr("invalid_client", "unknown client_id")
	}
	if err != nil {
		return err
	}
	if hash != nil && subtle.ConstantTimeCompare(hash, digest(secret)) != 1 {
		return oauthErr("invalid_client", "wrong client_secret")
	}
	return nil
}

// Token answers the token endpoint. clientID and secret come from HTTP
// Basic or the form.
func (s *Server) Token(ctx context.Context, form url.Values, clientID, secret string) (TokenResponse, error) {
	if clientID == "" {
		clientID, secret = form.Get("client_id"), form.Get("client_secret")
	}
	if err := s.client(ctx, clientID, secret); err != nil {
		return TokenResponse{}, err
	}
	var res TokenResponse
	var reused string
	err := db.Tx(ctx, s.DB, func(tx pgx.Tx) error {
		var grantID string
		switch form.Get("grant_type") {
		case "authorization_code":
			var redirect, challenge string
			var used *time.Time
			err := tx.QueryRow(ctx, `SELECT c.grant_id, c.redirect_uri, c.code_challenge, c.used_at FROM oauth_codes c
				JOIN oauth_grants g ON g.id = c.grant_id
				WHERE c.code_sha256 = $1 AND c.expires_at > now() AND g.client_id = $2 AND g.revoked_at IS NULL FOR UPDATE OF c`,
				digest(form.Get("code")), clientID).Scan(&grantID, &redirect, &challenge, &used)
			if db.NotFound(err) {
				return oauthErr("invalid_grant", "the code is unknown, expired, or not for this client")
			}
			if err != nil {
				return err
			}
			if used != nil {
				// A code used twice may have been stolen: stop everything it
				// gave out (RFC 6749, 4.1.2).
				reused = grantID
				return oauthErr("invalid_grant", "the code was already used")
			}
			if form.Get("redirect_uri") != "" && form.Get("redirect_uri") != redirect {
				return oauthErr("invalid_grant", "redirect_uri does not match the authorization request")
			}
			if !pkce(form.Get("code_verifier"), challenge) {
				return oauthErr("invalid_grant", "code_verifier does not match the code_challenge")
			}
			if _, err := tx.Exec(ctx, `UPDATE oauth_codes SET used_at = now() WHERE code_sha256 = $1`, digest(form.Get("code"))); err != nil {
				return err
			}
		case "refresh_token":
			err := tx.QueryRow(ctx, `UPDATE oauth_tokens t SET revoked_at = now() FROM oauth_grants g
				WHERE t.token_sha256 = $1 AND t.kind = 'refresh' AND t.revoked_at IS NULL AND t.expires_at > now()
				AND g.id = t.grant_id AND g.client_id = $2 AND g.revoked_at IS NULL
				RETURNING t.grant_id`, digest(form.Get("refresh_token")), clientID).Scan(&grantID)
			if db.NotFound(err) {
				return oauthErr("invalid_grant", "the refresh token is unknown, expired, revoked or already used")
			}
			if err != nil {
				return err
			}
		default:
			return oauthErr("unsupported_grant_type", "grant_type must be authorization_code or refresh_token")
		}
		access, refresh := accessPrefix+random(), refreshPrefix+random()
		if _, err := tx.Exec(ctx, `INSERT INTO oauth_tokens (token_sha256, grant_id, kind, expires_at) VALUES
			($1, $3, 'access', now() + make_interval(secs => $4)), ($2, $3, 'refresh', now() + make_interval(secs => $5))`,
			digest(access), digest(refresh), grantID, AccessTTL.Seconds(), RefreshTTL.Seconds()); err != nil {
			return err
		}
		res = TokenResponse{AccessToken: access, TokenType: "Bearer", ExpiresIn: int(AccessTTL.Seconds()), RefreshToken: refresh, Scope: Scope}
		return nil
	})
	if reused != "" {
		if _, rerr := s.DB.Exec(ctx, `UPDATE oauth_grants SET revoked_at = coalesce(revoked_at, now()) WHERE id = $1`, reused); rerr != nil {
			return res, rerr
		}
	}
	return res, err
}

// Revoke ends a token the client holds (RFC 7009). Revoking a refresh token
// ends the whole grant: the person disconnected the client.
func (s *Server) Revoke(ctx context.Context, token, clientID, secret string) error {
	if err := s.client(ctx, clientID, secret); err != nil {
		return err
	}
	var grantID, kind string
	err := s.DB.QueryRow(ctx, `SELECT t.grant_id, t.kind FROM oauth_tokens t JOIN oauth_grants g ON g.id = t.grant_id
		WHERE t.token_sha256 = $1 AND g.client_id = $2`, digest(token), clientID).Scan(&grantID, &kind)
	if db.NotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if kind == "refresh" {
		_, err = s.DB.Exec(ctx, `UPDATE oauth_grants SET revoked_at = coalesce(revoked_at, now()) WHERE id = $1`, grantID)
		return err
	}
	_, err = s.DB.Exec(ctx, `UPDATE oauth_tokens SET revoked_at = coalesce(revoked_at, now()) WHERE token_sha256 = $1`, digest(token))
	return err
}

// IsToken reports whether a bearer token is one of ours, so the caller
// knows which check to use.
func IsToken(token string) bool { return strings.HasPrefix(token, accessPrefix) }

// Authenticate returns the agents an access token acts as, one per
// workspace, in workspace name order.
func (s *Server) Authenticate(ctx context.Context, token string) ([]agent.Agent, error) {
	if !IsToken(token) {
		return nil, agent.ErrDenied
	}
	rows, err := s.DB.Query(ctx, `UPDATE principals p SET last_used_at = CASE
			WHEN p.last_used_at IS NULL OR p.last_used_at < now() - interval '1 minute' THEN now() ELSE p.last_used_at END
		FROM oauth_tokens t JOIN oauth_grants g ON g.id = t.grant_id
		WHERE t.token_sha256 = $1 AND t.kind = 'access' AND t.revoked_at IS NULL AND t.expires_at > now()
		AND g.revoked_at IS NULL AND p.oauth_grant_id = g.id AND p.kind = 'agent' AND p.revoked_at IS NULL
		RETURNING p.id, p.workspace_id, p.name, p.autonomy, p.created_at, p.last_used_at`, digest(token))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var agents []agent.Agent
	for rows.Next() {
		var a agent.Agent
		if err := rows.Scan(&a.ID, &a.WorkspaceID, &a.Name, &a.Autonomy, &a.CreatedAt, &a.LastUsedAt); err != nil {
			return nil, err
		}
		agents = append(agents, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(agents) == 0 {
		return nil, agent.ErrDenied
	}
	return agents, nil
}

func pkce(verifier, challenge string) bool {
	if len(verifier) < 43 || len(verifier) > 128 {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	return subtle.ConstantTimeCompare([]byte(base64.RawURLEncoding.EncodeToString(sum[:])), []byte(challenge)) == 1
}

func withQuery(raw string, pairs ...string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := u.Query()
	for i := 0; i+1 < len(pairs); i += 2 {
		if pairs[i+1] != "" {
			q.Set(pairs[i], pairs[i+1])
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func random() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func digest(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}
