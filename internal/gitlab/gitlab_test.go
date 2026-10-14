package gitlab

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const token = "glpat-SUPERSECRET-should-not-leak"

type call struct {
	method, path, auth string
	body               map[string]any
}

// fakeGitLab records every request and answers from a script keyed by method+path.
func fakeGitLab(t *testing.T, answer func(c call) (int, any)) (*httptest.Server, *[]call) {
	t.Helper()
	var calls []call
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := call{method: r.Method, path: r.URL.EscapedPath(), auth: r.Header.Get("PRIVATE-TOKEN")}
		if b, _ := io.ReadAll(r.Body); len(b) > 0 {
			_ = json.Unmarshal(b, &c.body)
		}
		calls = append(calls, c)
		status, body := answer(c)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func TestOpenChangeOpensMergeRequestWithTokenInHeaderOnly(t *testing.T) {
	srv, calls := fakeGitLab(t, func(c call) (int, any) {
		return 201, map[string]any{"iid": 3, "web_url": "https://gitlab.com/smoovly-apps/tab-split/-/merge_requests/3"}
	})
	c := New(srv.URL+"/api/v4/", "smoovly-apps", "tab-split", token)
	url, err := c.OpenChange(context.Background(), "", "agent/t1", "main", "Split", "body")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if !strings.HasSuffix(url, "/merge_requests/3") {
		t.Errorf("url = %q", url)
	}
	got := (*calls)[0]
	if got.method != "POST" || got.path != "/api/v4/projects/smoovly-apps%2Ftab-split/merge_requests" {
		t.Errorf("call = %s %s", got.method, got.path)
	}
	if got.auth != token {
		t.Errorf("PRIVATE-TOKEN header = %q", got.auth)
	}
	if got.body["source_branch"] != "agent/t1" || got.body["target_branch"] != "main" || got.body["title"] != "Split" || got.body["description"] != "body" || got.body["remove_source_branch"] != true {
		t.Errorf("body = %v", got.body)
	}
	if strings.Contains(got.path, token) {
		t.Error("token in the path")
	}
}

func TestFetchIssueMapsFieldsAndCommentPostsNote(t *testing.T) {
	srv, calls := fakeGitLab(t, func(c call) (int, any) {
		if c.method == "GET" {
			return 200, map[string]any{"iid": 42, "title": "Login is broken", "description": "Steps...", "web_url": "https://gitlab.com/g/p/-/issues/42"}
		}
		return 201, map[string]any{"id": 1}
	})
	c := New(srv.URL+"/api/v4", "g/sub", "p", token)
	issue, err := c.FetchIssue(context.Background(), 42)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if issue.Number != 42 || issue.Title != "Login is broken" || issue.Body != "Steps..." || issue.URL == "" {
		t.Errorf("issue = %+v", issue)
	}
	if (*calls)[0].path != "/api/v4/projects/g%2Fsub%2Fp/issues/42" {
		t.Errorf("path = %q", (*calls)[0].path)
	}
	if err := c.CommentIssue(context.Background(), 42, "Agent-produced MR: x"); err != nil {
		t.Fatalf("comment: %v", err)
	}
	if n := (*calls)[1]; n.method != "POST" || n.path != "/api/v4/projects/g%2Fsub%2Fp/issues/42/notes" || n.body["body"] != "Agent-produced MR: x" {
		t.Errorf("note call = %+v", n)
	}
}

func TestErrorsCarryGitLabsMessageNeverTheToken(t *testing.T) {
	srv, _ := fakeGitLab(t, func(c call) (int, any) {
		return 403, map[string]any{"message": "403 Forbidden - insufficient scope " + strings.Repeat("x", 300)}
	})
	c := New(srv.URL+"/api/v4", "g", "p", token)
	_, err := c.OpenChange(context.Background(), "", "b", "main", "t", "d")
	if err == nil {
		t.Fatal("expected an error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "403") || !strings.Contains(msg, "insufficient scope") || !strings.Contains(msg, "POST /projects/g%2Fp/merge_requests") {
		t.Errorf("message = %q", msg)
	}
	if strings.Contains(msg, token) {
		t.Error("token leaked into the error")
	}
	if len(msg) > 400 {
		t.Errorf("message not bounded: %d chars", len(msg))
	}
}

func TestCloneURLAndCredentialUsername(t *testing.T) {
	c := New("", "smoovly-apps", "tab-split", token)
	if c.CloneURL() != "https://gitlab.com/smoovly-apps/tab-split.git" {
		t.Errorf("clone = %q", c.CloneURL())
	}
	self := New("https://gitlab.example.com/api/v4/", "g/sub", "p", token)
	if self.CloneURL() != "https://gitlab.example.com/g/sub/p.git" {
		t.Errorf("self-managed clone = %q", self.CloneURL())
	}
	if c.CredentialUsername() != "oauth2" {
		t.Errorf("username = %q", c.CredentialUsername())
	}
	if strings.Contains(credentialHelper, token) || !strings.Contains(credentialHelper, "$"+tokenEnvVar) {
		t.Error("the credential helper must read the token from the environment")
	}
}

func TestAPIMessage(t *testing.T) {
	for in, want := range map[string]string{
		`{"message":"401 Unauthorized"}`:            "401 Unauthorized",
		`{"message":{"source_branch":["missing"]}}`: `{"source_branch":["missing"]}`,
		`{"error":"invalid_token"}`:                 "invalid_token",
		`<html>bad gateway</html>`:                  "<html>bad gateway</html>",
	} {
		if got := apiMessage([]byte(in)); got != want {
			t.Errorf("apiMessage(%s) = %q, want %q", in, got, want)
		}
	}
}
