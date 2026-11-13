// Package slack copies web chats into a Slack channel and turns replies typed
// in Slack into replies to the visitor.
package slack

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// APIError is Slack's own error code, such as invalid_auth or
// channel_not_found.
type APIError struct{ Code string }

func (e *APIError) Error() string { return "Slack said " + e.Code + ": " + explain(e.Code) }

// explain puts the common Slack errors into plain words.
func explain(code string) string {
	switch code {
	case "invalid_auth", "not_authed", "account_inactive", "token_revoked", "token_expired":
		return "the bot token is not valid; copy the Bot User OAuth Token again"
	case "channel_not_found":
		return "the channel was not found; check the channel ID, and for a private channel invite the app first"
	case "not_in_channel":
		return "the app is not in the channel; type /invite @<app name> in it"
	case "is_archived":
		return "the channel is archived"
	case "missing_scope":
		return "the app is missing a permission; it needs chat:write, users:read, users:read.email and channels:history (groups:history for a private channel)"
	case "ratelimited":
		return "Slack is limiting requests; it will be retried"
	}
	return "see Slack's API documentation for this error"
}

// Client calls Slack's Web API with a bot token.
type Client struct {
	Token string
	// BaseURL may be replaced by tests.
	BaseURL string
	HTTP    *http.Client
}

func (c *Client) call(ctx context.Context, method string, form url.Values, out any) error {
	base := c.BaseURL
	if base == "" {
		base = "https://slack.com/api"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/"+method, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	res, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusTooManyRequests {
		return &APIError{Code: "ratelimited"}
	}
	var raw bytes.Buffer
	if _, err := raw.ReadFrom(io.LimitReader(res.Body, 1<<20)); err != nil {
		return err
	}
	var status struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw.Bytes(), &status); err != nil {
		return fmt.Errorf("slack %s: HTTP %d", method, res.StatusCode)
	}
	if !status.OK {
		return &APIError{Code: status.Error}
	}
	if out != nil {
		return json.Unmarshal(raw.Bytes(), out)
	}
	return nil
}

// Identity is who the bot token belongs to.
type Identity struct {
	TeamID    string `json:"team_id"`
	Team      string `json:"team"`
	BotUserID string `json:"user_id"`
}

// AuthTest checks the token.
func (c *Client) AuthTest(ctx context.Context) (Identity, error) {
	var id Identity
	err := c.call(ctx, "auth.test", url.Values{}, &id)
	return id, err
}

// Post sends text to a channel, in a thread when threadTS is set, and
// returns the new message's ts. Links are not unfurled: a visitor's message
// should not make Slack fetch the URLs in it.
func (c *Client) Post(ctx context.Context, channel, text, threadTS string) (string, error) {
	form := url.Values{"channel": {channel}, "text": {text}, "unfurl_links": {"false"}, "unfurl_media": {"false"}}
	if threadTS != "" {
		form.Set("thread_ts", threadTS)
	}
	var out struct {
		TS string `json:"ts"`
	}
	err := c.call(ctx, "chat.postMessage", form, &out)
	return out.TS, err
}

// User is a Slack user's name and email.
type User struct {
	Name  string
	Email string
}

// UserInfo looks a user up; the email needs the users:read.email scope.
func (c *Client) UserInfo(ctx context.Context, id string) (User, error) {
	var out struct {
		User struct {
			Name     string `json:"name"`
			RealName string `json:"real_name"`
			Profile  struct {
				Email string `json:"email"`
			} `json:"profile"`
		} `json:"user"`
	}
	if err := c.call(ctx, "users.info", url.Values{"user": {id}}, &out); err != nil {
		return User{}, err
	}
	name := out.User.RealName
	if name == "" {
		name = out.User.Name
	}
	return User{Name: name, Email: out.User.Profile.Email}, nil
}

// ErrSignature means a request did not come from Slack, or is too old.
var ErrSignature = errors.New("slack signature is missing, wrong or too old")

// Verify checks Slack's request signature: v0= HMAC-SHA256 of
// "v0:{timestamp}:{body}" with the signing secret, within five minutes.
func Verify(secret, timestamp, signature string, body []byte, now time.Time) error {
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil || secret == "" {
		return ErrSignature
	}
	if d := now.Sub(time.Unix(ts, 0)); d > 5*time.Minute || d < -5*time.Minute {
		return ErrSignature
	}
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "v0:%s:", timestamp)
	mac.Write(body)
	want := "v0=" + hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(signature)) {
		return ErrSignature
	}
	return nil
}

// Escape makes text safe to post: Slack treats <...> as mentions, links and
// commands such as <!channel>, so a visitor's words must not contain them.
func Escape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// Plain turns a Slack message back into the text a person typed: entities
// unescaped, <url|label> and <url> to the URL, <@U123> to @name when known.
func Plain(s string, names map[string]string) string {
	var b strings.Builder
	for {
		i := strings.IndexByte(s, '<')
		if i < 0 {
			break
		}
		j := strings.IndexByte(s[i:], '>')
		if j < 0 {
			break
		}
		b.WriteString(s[:i])
		inner := s[i+1 : i+j]
		target, label, _ := strings.Cut(inner, "|")
		switch {
		case strings.HasPrefix(target, "@"):
			if n := names[target[1:]]; n != "" {
				b.WriteString("@" + n)
			} else if label != "" {
				b.WriteString("@" + label)
			} else {
				b.WriteString("@someone")
			}
		case strings.HasPrefix(target, "#"):
			b.WriteString("#" + label)
		case strings.HasPrefix(target, "!"):
			b.WriteString(label)
		case strings.HasPrefix(target, "mailto:"):
			b.WriteString(strings.TrimPrefix(target, "mailto:"))
		default:
			b.WriteString(target)
		}
		s = s[i+j+1:]
	}
	b.WriteString(s)
	return strings.NewReplacer("&lt;", "<", "&gt;", ">", "&amp;", "&").Replace(b.String())
}
