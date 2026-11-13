package sending

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/VisiOffice/VisiComms/internal/files"
	"github.com/VisiOffice/VisiComms/internal/phone"
)

type FaxMessage struct {
	From       string `json:"from"`
	DocumentID string `json:"document_id"`
	SHA256     string `json:"sha256"`
}

type FaxSender interface {
	ProjectID() string
	FaxMediaURL(string) string
	SendFax(context.Context, FaxMessage, string, string) (string, error)
}

func (s *SignalWire) FaxInboundURL() string { return s.PublicURL + "/webhooks/signalwire/fax" }
func (s *SignalWire) FaxReceivedURL() string {
	return s.PublicURL + "/webhooks/signalwire/fax/received"
}
func (s *SignalWire) FaxStatusURL() string            { return s.PublicURL + "/webhooks/signalwire/fax/status" }
func (s *SignalWire) FaxMediaURL(token string) string { return s.PublicURL + "/fax-media/" + token }
func (s *SignalWire) SendFax(ctx context.Context, msg FaxMessage, to, mediaURL string) (string, error) {
	if _, err := phone.Normalize(msg.From); err != nil {
		return "", &Rejected{Reason: err.Error()}
	}
	if _, err := phone.Normalize(to); err != nil {
		return "", &Rejected{Reason: err.Error()}
	}
	var result struct {
		SID    string `json:"sid"`
		Status string `json:"status"`
	}
	err := s.request(ctx, http.MethodPost, "/Faxes.json", url.Values{"From": {msg.From}, "To": {to}, "MediaUrl": {mediaURL}, "Quality": {"fine"}, "StatusCallback": {s.FaxStatusURL()}}, &result)
	if err != nil {
		return "", err
	}
	if result.SID == "" {
		return "", errors.New("SignalWire accepted the fax without a SID")
	}
	if result.Status == "failed" {
		return result.SID, &Rejected{Reason: "SignalWire reported an immediate fax failure"}
	}
	return result.SID, nil
}

// OpenRecording fetches a call recording from SignalWire, under the same
// rules as DownloadFax: trusted hosts only, no redirects. A Range header is
// passed through so the console's player can seek. The caller closes the
// response body.
func (s *SignalWire) OpenRecording(ctx context.Context, raw, byteRange string) (*http.Response, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || (u.Host != s.Space && u.Host != "files.signalwire.com") || u.Fragment != "" {
		return nil, errors.New("recordings must use a trusted SignalWire HTTPS host")
	}
	// SignalWire serves a recording as MP3 with ".mp3" and as WAV with no
	// extension. Ask for MP3 first, and fall back to WAV when there is none.
	candidates := []string{u.String()}
	if path.Ext(u.Path) == "" {
		mp3 := *u
		mp3.Path += ".mp3"
		candidates = []string{mp3.String(), u.String()}
	}
	client := &http.Client{Timeout: 2 * time.Minute}
	if s.HTTP != nil {
		*client = *s.HTTP
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	var status int
	for _, candidate := range candidates {
		res, err := s.fetchRecording(ctx, client, candidate, byteRange, true)
		if err != nil {
			return nil, err
		}
		if res.StatusCode >= 300 && res.StatusCode < 400 {
			// SignalWire answers with a redirect to the file on its media
			// host. Follow it once, there only, without the credentials.
			res.Body.Close()
			next, err := u.Parse(res.Header.Get("Location"))
			if err != nil || next.Scheme != "https" || next.User != nil || next.Host != "files.signalwire.com" {
				return nil, errors.New("SignalWire redirected the recording off its media host")
			}
			if res, err = s.fetchRecording(ctx, client, next.String(), byteRange, false); err != nil {
				return nil, err
			}
		}
		if res.StatusCode == http.StatusOK || res.StatusCode == http.StatusPartialContent {
			return res, nil
		}
		res.Body.Close()
		status = res.StatusCode
		if status != http.StatusNotFound {
			break
		}
	}
	return nil, fmt.Errorf("SignalWire recording could not be retrieved: HTTP %d", status)
}

func (s *SignalWire) fetchRecording(ctx context.Context, client *http.Client, raw, byteRange string, authenticated bool) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, err
	}
	if authenticated {
		req.SetBasicAuth(s.Project, s.Token)
	}
	if byteRange != "" {
		req.Header.Set("Range", byteRange)
	}
	return client.Do(req)
}

// DeleteRecording deletes a recording from SignalWire once VisiComms holds
// its own copy, or when it expires. A recording SignalWire no longer has
// counts as deleted.
func (s *SignalWire) DeleteRecording(ctx context.Context, raw string) error {
	sid, err := s.recordingSID(raw)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, "https://"+s.Space+"/api/laml/2010-04-01/Accounts/"+s.Project+"/Recordings/"+url.PathEscape(sid)+".json", nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(s.Project, s.Token)
	client := &http.Client{Timeout: 30 * time.Second}
	if s.HTTP != nil {
		*client = *s.HTTP
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	res.Body.Close()
	if res.StatusCode/100 == 2 || res.StatusCode == http.StatusNotFound {
		return nil
	}
	return fmt.Errorf("SignalWire would not delete the recording: HTTP %d", res.StatusCode)
}

var recordingID = regexp.MustCompile(`^(RE[0-9A-Za-z]+|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$`)

// recordingSID reads the recording's ID from a URL SignalWire gave us. It
// accepts the LaML form (…/Accounts/{project}/[Calls/{call}/]Recordings/{id})
// and the media form ({host}/{space}/{project}/recordings/{id}.wav), and only for
// this project, so a delete can never reach another project's recording.
func (s *SignalWire) recordingSID(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || (u.Host != s.Space && u.Host != "files.signalwire.com") {
		return "", errors.New("recordings must use a trusted SignalWire HTTPS host")
	}
	parts := strings.Split(strings.Trim(strings.TrimSuffix(u.Path, path.Ext(u.Path)), "/"), "/")
	n := len(parts)
	if n < 3 || !strings.EqualFold(parts[n-2], "recordings") || !recordingID.MatchString(parts[n-1]) {
		return "", errors.New("not a SignalWire recording URL")
	}
	owned := false
	if n == 4 && parts[0] != "api" {
		// Media form: /{space}/{project}/recordings/{id}.
		owned = parts[1] == s.Project
	} else {
		for i, p := range parts[:n-2] {
			if p == s.Project && i > 0 && parts[i-1] == "Accounts" {
				owned = true
			}
		}
	}
	if !owned {
		return "", errors.New("not a recording of this SignalWire project")
	}
	return parts[n-1], nil
}

// DownloadFax only fetches provider media hosts, never caller-supplied URLs.
// Redirects are refused so credentials cannot be forwarded to another host.
func (s *SignalWire) DownloadFax(ctx context.Context, raw string) ([]byte, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || (u.Host != s.Space && u.Host != "files.signalwire.com") || u.Fragment != "" {
		return nil, errors.New("fax media must use a trusted SignalWire HTTPS host")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(s.Project, s.Token)
	client := &http.Client{Timeout: 20 * time.Second}
	if s.HTTP != nil {
		*client = *s.HTTP
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return nil, errors.New("SignalWire fax document could not be retrieved")
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, files.MaxPDF+1))
	if err != nil {
		return nil, err
	}
	if err := files.ValidatePDF(data); err != nil {
		return nil, err
	}
	return data, nil
}
