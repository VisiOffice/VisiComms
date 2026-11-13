package sending

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// fakeParent answers SignalWire's project API as the parent of own-project,
// counting each call by path.
func fakeParent(t *testing.T, calls map[string]int, rotatedParent *string) *SignalWireParent {
	t.Helper()
	parent, err := NewSignalWireParent("own.signalwire.com", "parent-project", "parent-token")
	if err != nil {
		t.Fatal(err)
	}
	parent.HTTP = &http.Client{Transport: resendTransport(func(r *http.Request) (*http.Response, error) {
		if user, token, _ := r.BasicAuth(); user != "parent-project" || token != "parent-token" || r.Method != "POST" || r.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("project API call without the parent's credentials: %s %s", r.Method, r.URL)
		}
		calls[r.URL.Path]++
		var body map[string]any
		data, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(data, &body)
		switch r.URL.Path {
		case "/api/projects":
			if !strings.HasPrefix(body["name"].(string), "visicomms-") {
				t.Fatalf("subproject name %v", body["name"])
			}
			return resendResponse(201, `{"id":"`+ownProject+`","name":"`+body["name"].(string)+`","parent_project_id":"parent-project","signing_key":"created-key"}`), nil
		case "/api/project/tokens":
			if body["subproject_id"] != ownProject || !strings.Contains(string(data), `"messaging"`) || !strings.Contains(string(data), `"fax"`) {
				t.Fatalf("token request %s", data)
			}
			return resendResponse(200, `{"id":"tok-1","token":"own-token"}`), nil
		case "/api/projects/" + ownProject + "/signing-key/rotate":
			return resendResponse(200, `{"id":"`+ownProject+`","parent_project_id":"`+*rotatedParent+`","signing_key":"rotated-key"}`), nil
		}
		return resendResponse(404, `{}`), nil
	})}
	return parent
}

func TestProvisionAndAdoptSealWhatSignalWireHandsOver(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	var sent []string
	p, _ := ownProjects(t, w, &sent)
	calls := map[string]int{}
	rotatedParent := "parent-project"
	parent := fakeParent(t, calls, &rotatedParent)
	ws := w.r.WorkspaceID
	var other string
	if err := w.pool.QueryRow(ctx, `INSERT INTO workspaces (name, timezone) VALUES ('Other', 'UTC') RETURNING id`).Scan(&other); err != nil {
		t.Fatal(err)
	}
	signingKey := func() string {
		sw, owner, err := p.ForProject(ctx, ownProject)
		if err != nil || owner != ws || sw.Token != "own-token" {
			t.Fatalf("ForProject=%+v,%s,%v", sw, owner, err)
		}
		return sw.SigningKey
	}

	sub, err := p.Provision(ctx, parent, ws)
	if err != nil || sub.ID != ownProject {
		t.Fatalf("Provision=%+v,%v", sub, err)
	}
	if k := signingKey(); k != "created-key" {
		t.Fatalf("signing key %q", k)
	}
	if _, err := p.Provision(ctx, parent, ws); err == nil || calls["/api/projects"] != 1 {
		t.Fatalf("provisioned twice: %v, %d subprojects", err, calls["/api/projects"])
	}

	// Another workspace cannot take the project, and is refused before the
	// key is rotated out from under the workspace that has it.
	if err := p.Adopt(ctx, parent, other, ownProject, ""); err == nil || calls["/api/projects/"+ownProject+"/signing-key/rotate"] != 0 {
		t.Fatalf("adopted another workspace's project: %v, %d rotations", err, calls["/api/projects/"+ownProject+"/signing-key/rotate"])
	}
	if err := p.Adopt(ctx, parent, other, ownProject, "copied-key"); err == nil || calls["/api/project/tokens"] != 1 {
		t.Fatalf("adopted another workspace's project with a copied key: %v, %d tokens", err, calls["/api/project/tokens"])
	}

	if err := p.Adopt(ctx, parent, ws, ownProject, "copied-key"); err != nil {
		t.Fatal(err)
	}
	if k := signingKey(); k != "copied-key" {
		t.Fatalf("signing key %q", k)
	}
	if err := p.Adopt(ctx, parent, ws, ownProject, ""); err != nil {
		t.Fatal(err)
	}
	if k := signingKey(); k != "rotated-key" || calls["/api/project/tokens"] != 3 {
		t.Fatalf("signing key %q after rotating, %d tokens", k, calls["/api/project/tokens"])
	}

	rotatedParent = "someone-else"
	if err := p.Adopt(ctx, parent, ws, ownProject, ""); err == nil || !strings.Contains(err.Error(), "not a subproject") {
		t.Fatalf("adopted a project of another parent: %v", err)
	}
}
