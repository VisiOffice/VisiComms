package sending

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/riverqueue/river"

	"github.com/VisiOffice/VisiComms/internal/ingest"
	"github.com/VisiOffice/VisiComms/internal/review"
	"github.com/VisiOffice/VisiComms/internal/secrets"
)

const ownProject = "own-project"

// ownSignalWire answers as a workspace's own project in own.signalwire.com:
// any project ID it is asked about exists, and sent messages are recorded.
func ownSignalWire(t *testing.T, sent *[]string) *http.Client {
	return &http.Client{Transport: resendTransport(func(r *http.Request) (*http.Response, error) {
		project, token, _ := r.BasicAuth()
		if r.URL.Host != "own.signalwire.com" || token != "own-token" {
			t.Fatalf("request to %s with the wrong credentials", r.URL)
		}
		switch {
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/Accounts/"+project+".json"):
			// Reading the account needs Management, which a workspace token lacks.
			return resendResponse(401, `{}`), nil
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/IncomingPhoneNumbers.json"):
			return resendResponse(200, `{"incoming_phone_numbers":[{"sid":"PN2","account_sid":"`+project+`","phone_number":"`+smsLocal+`","capabilities":{"sms":true,"fax":true}}]}`), nil
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/Accounts/"+ownProject+"/Messages.json"):
			*sent = append(*sent, project)
			return resendResponse(201, `{"sid":"SM-own"}`), nil
		}
		t.Fatalf("unexpected request %s %s", r.Method, r.URL)
		return nil, nil
	})}
}

func ownProjects(t *testing.T, w world, sent *[]string) (*SignalWireProjects, *SignalWire) {
	t.Helper()
	keys, err := secrets.New(bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}
	installation := smsProvider(t, func(r *http.Request) (*http.Response, error) {
		t.Fatalf("sent through the installation project: %s", r.URL)
		return nil, nil
	})
	p := &SignalWireProjects{DB: w.pool, Keys: keys, Installation: installation, PublicURL: "https://comms.example.com", HTTP: ownSignalWire(t, sent)}
	own, err := NewSignalWire("own.signalwire.com", ownProject, "own-token", "own-key", p.PublicURL)
	if err != nil {
		t.Fatal(err)
	}
	own.HTTP = p.HTTP
	return p, own
}

func TestWorkspaceProjectIsSealedAndRoutedByProject(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	var sent []string
	p, own := ownProjects(t, w, &sent)
	ws := w.r.WorkspaceID
	var other string
	if err := w.pool.QueryRow(ctx, `INSERT INTO workspaces (name, timezone) VALUES ('Other', 'UTC') RETURNING id`).Scan(&other); err != nil {
		t.Fatal(err)
	}

	if err := p.ConnectProject(ctx, ws, own); err != nil {
		t.Fatal(err)
	}
	var stored []byte
	var settings string
	if err := w.pool.QueryRow(ctx, `SELECT credentials_ciphertext, settings::text FROM connectors WHERE workspace_id=$1 AND kind='signalwire_project'`, ws).Scan(&stored, &settings); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stored, []byte("own-token")) || bytes.Contains(stored, []byte("own-key")) || strings.Contains(settings, "own-token") || strings.Contains(settings, "own-key") {
		t.Fatal("credentials stored in the clear")
	}

	if project, err := p.ProjectID(ctx, ws); err != nil || project != ownProject {
		t.Fatalf("ProjectID=%s,%v", project, err)
	}
	if project, err := p.ProjectID(ctx, other); err != nil || project != smsProject {
		t.Fatalf("a workspace without its own project should use the installation's: %s,%v", project, err)
	}
	sw, owner, err := p.ForProject(ctx, ownProject)
	if err != nil || owner != ws || sw.Token != "own-token" || sw.SigningKey != "own-key" || sw.Space != "own.signalwire.com" {
		t.Fatalf("ForProject=%+v,%s,%v", sw, owner, err)
	}
	if _, _, err := p.ForProject(ctx, "nobody"); !errors.Is(err, ErrUnknownProject) {
		t.Fatalf("unknown project: %v", err)
	}
	wrong := *p
	wrong.Keys, _ = secrets.New(bytes.Repeat([]byte{8}, 32))
	if _, _, err := wrong.ForProject(ctx, ownProject); !errors.Is(err, secrets.ErrOpen) {
		t.Fatalf("opened with another master key: %v", err)
	}
	noKeys := *p
	noKeys.Keys = nil
	if _, _, err := noKeys.ForProject(ctx, ownProject); !errors.Is(err, ErrUnknownProject) {
		t.Fatalf("workspace project without a master key: %v", err)
	}

	if err := p.ConnectProject(ctx, other, own); err == nil || !strings.Contains(err.Error(), "another workspace") {
		t.Fatalf("same project on two workspaces: %v", err)
	}
	if err := p.ConnectProject(ctx, ws, p.Installation); err == nil {
		t.Fatal("connected the installation project as a workspace's own")
	}
	rotated := *own
	rotated.SigningKey = "own-key-2"
	if err := p.ConnectProject(ctx, ws, &rotated); err != nil {
		t.Fatal(err)
	}
	if sw, _, _ := p.ForProject(ctx, ownProject); sw.SigningKey != "own-key-2" {
		t.Fatal("reconnecting did not replace the signing key")
	}

	// Numbers are checked against the workspace's own project, and the queued
	// reply goes out through it, not the installation's.
	sw, err = p.ForWorkspace(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	if err := ConnectSMSNumber(ctx, w.pool, sw, ws, smsLocal); err != nil {
		t.Fatal(err)
	}
	moved := *own
	moved.Project = "new-project"
	if err := p.ConnectProject(ctx, ws, &moved); err == nil || !strings.Contains(err.Error(), "still has numbers") {
		t.Fatalf("switched projects with numbers connected: %v", err)
	}
	in, err := ingest.ReceiveSMS(ctx, w.pool, ingest.SMS{SID: "SM-in", From: smsRemote, To: smsLocal, Body: "Hi", Project: ownProject})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ingest.ReceiveSMS(ctx, w.pool, ingest.SMS{SID: "SM-in-2", From: smsRemote, To: smsLocal, Body: "Hi", Project: smsProject}); !errors.Is(err, ingest.ErrUnknownRecipient) {
		t.Fatalf("number accepted through the installation project: %v", err)
	}
	w.r.SMSProject = ownProject
	reply, err := review.Reply(ctx, w.pool, w.r, in.ThreadID, review.EmailPayload{Body: "Hello"})
	if err != nil {
		t.Fatal(err)
	}
	worker := &sendWorker{pool: w.pool, signalwire: p}
	if err := worker.Work(ctx, &river.Job[SendArgs]{Args: SendArgs{ActionID: reply.ActionID}}); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 1 || sent[0] != ownProject || w.state(t, reply.ActionID) != "accepted" {
		t.Fatalf("sent=%v state=%s", sent, w.state(t, reply.ActionID))
	}
}

func TestAttachRegistryChecksTheParentAndSurvivesReconnect(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	var sent []string
	p, own := ownProjects(t, w, &sent)
	parent, err := NewSignalWireParent("parent.signalwire.com", "parent-project", "parent-token")
	if err != nil {
		t.Fatal(err)
	}
	parent.HTTP = &http.Client{Transport: resendTransport(func(r *http.Request) (*http.Response, error) {
		if user, token, _ := r.BasicAuth(); user != "parent-project" || token != "parent-token" || r.URL.Host != "parent.signalwire.com" {
			t.Fatalf("registry read without the parent's credentials: %s", r.URL)
		}
		switch r.URL.Path {
		case "/api/relay/rest/registry/beta/brands/brand-1":
			return resendResponse(200, `{"id":"brand-1","name":"Acme Plumbing","state":"completed"}`), nil
		case "/api/relay/rest/registry/beta/brands/brand-1/campaigns":
			return resendResponse(200, `{"data":[{"id":"camp-1","name":"Delivery / Pickup Notifications","state":"active","sms_use_case":"CUSTOMER_CARE"},{"id":"camp-2","name":"Old","state":"expired"}]}`), nil
		}
		return resendResponse(404, `{}`), nil
	})}
	ws := w.r.WorkspaceID

	if _, _, err := AttachRegistry(ctx, w.pool, parent, ws, "brand-1", nil); err == nil || !strings.Contains(err.Error(), "connect the workspace") {
		t.Fatalf("attached without a project: %v", err)
	}
	if err := p.ConnectProject(ctx, ws, own); err != nil {
		t.Fatal(err)
	}
	if _, _, err := AttachRegistry(ctx, w.pool, parent, ws, "brand-1", []string{"camp-9"}); err == nil {
		t.Fatal("attached a campaign from another brand")
	}
	if _, _, err := AttachRegistry(ctx, w.pool, parent, ws, "brand-2", nil); err == nil {
		t.Fatal("attached an unknown brand")
	}
	brand, campaigns, err := AttachRegistry(ctx, w.pool, parent, ws, "brand-1", []string{"camp-1"})
	if err != nil || brand.Name != "Acme Plumbing" || len(campaigns) != 1 || campaigns[0].SMSUseCase != "CUSTOMER_CARE" {
		t.Fatalf("AttachRegistry=%+v,%+v,%v", brand, campaigns, err)
	}
	settings := func() string {
		var s string
		if err := w.pool.QueryRow(ctx, `SELECT settings::text FROM connectors WHERE workspace_id=$1 AND kind='signalwire_project'`, ws).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	if s := settings(); !strings.Contains(s, `"brand_id": "brand-1"`) || !strings.Contains(s, `"campaign_ids": ["camp-1"]`) {
		t.Fatalf("settings=%s", s)
	}
	if err := p.ConnectProject(ctx, ws, own); err != nil {
		t.Fatal(err)
	}
	if s := settings(); !strings.Contains(s, "brand-1") {
		t.Fatalf("reconnecting the same project lost the brand: %s", s)
	}
	moved := *own
	moved.Project = "new-project"
	if err := p.ConnectProject(ctx, ws, &moved); err != nil {
		t.Fatal(err)
	}
	if s := settings(); strings.Contains(s, "brand-1") || !strings.Contains(s, "new-project") {
		t.Fatalf("another project kept the old brand: %s", s)
	}
}
