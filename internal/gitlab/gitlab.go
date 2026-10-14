// Package gitlab performs host-side GitLab operations — issue fetch, feature-
// branch push, merge request create, issue note — through the REST API with
// one token (D3): on the build lane, the generated-apps group's bot user's
// personal access token (api + write_repository). The token lives ONLY here and
// only on the host: it reaches the API in a request header and git through a
// one-shot credential helper via the environment (never argv), and it is never
// placed in a container, the source export, the bundle, the DB, or an error.
package gitlab

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/iQonAi/devbox/internal/gitx"
	"github.com/iQonAi/devbox/internal/prompt"
)

// DefaultAPIURL is GitLab.com's REST API base.
const DefaultAPIURL = "https://gitlab.com/api/v4"

// tokenEnvVar carries the token to git's credential helper via the environment.
const tokenEnvVar = "AGENT_TASK_GL_TOKEN"

// credentialHelper presents the token as the password. GitLab accepts any
// username with a personal or project access token; oauth2 is the documented one.
const credentialHelper = `!f() { echo username=oauth2; echo password=$` + tokenEnvVar + `; }; f`

// Client runs GitLab operations for one project with one token.
type Client struct {
	apiURL  string
	webBase string
	owner   string // the group's full path (subgroups allowed)
	repo    string // the project's path
	token   string
	http    *http.Client
}

// New returns a client for owner/repo on the GitLab instance whose API base is
// apiURL ("" = GitLab.com), authenticating with token.
func New(apiURL, owner, repo, token string) *Client {
	if apiURL == "" {
		apiURL = DefaultAPIURL
	}
	apiURL = strings.TrimRight(apiURL, "/")
	return &Client{
		apiURL: apiURL, webBase: strings.TrimSuffix(apiURL, "/api/v4"),
		owner: owner, repo: repo, token: token,
		http: &http.Client{Timeout: 60 * time.Second},
	}
}

// CredentialUsername is what git's credential helper presents with the token.
func (c *Client) CredentialUsername() string { return "oauth2" }

// CloneURL is the project's HTTPS remote.
func (c *Client) CloneURL() string { return fmt.Sprintf("%s/%s/%s.git", c.webBase, c.owner, c.repo) }

// project is the URL-encoded project path GitLab takes in place of an id.
func (c *Client) project() string { return url.PathEscape(c.owner + "/" + c.repo) }

// FetchIssue renders an issue into the prompt input (--issue).
func (c *Client) FetchIssue(ctx context.Context, number int) (prompt.Issue, error) {
	var raw struct {
		IID         int    `json:"iid"`
		Title       string `json:"title"`
		Description string `json:"description"`
		WebURL      string `json:"web_url"`
	}
	if err := c.api(ctx, http.MethodGet, "/projects/"+c.project()+"/issues/"+strconv.Itoa(number), nil, &raw); err != nil {
		return prompt.Issue{}, err
	}
	return prompt.Issue{Number: raw.IID, Title: raw.Title, Body: raw.Description, URL: raw.WebURL}, nil
}

// Push pushes the feature branch using the token via a credential helper (never
// in argv or written to .git/config), to the explicit project URL rather than
// the `origin` remote (the mirror clone's remote.origin.mirror=true rejects a
// single-branch refspec). All git runs hook-disabled (gitx).
func (c *Client) Push(ctx context.Context, worktreeDir, branch string) error {
	env := []string{tokenEnvVar + "=" + c.token}
	args := []string{
		"-c", "credential.helper=",
		"-c", "credential.helper=" + credentialHelper,
		"push", c.CloneURL(), branch,
	}
	if _, err := gitx.RunEnv(ctx, worktreeDir, env, args...); err != nil {
		return fmt.Errorf("push %s: %w", branch, err)
	}
	return nil
}

// OpenChange opens a merge request (target = the default branch) and returns
// its URL. The source branch is deleted on merge. Never auto-merged.
func (c *Client) OpenChange(ctx context.Context, _ string, branch, base, title, body string) (string, error) {
	in := map[string]any{
		"source_branch": branch, "target_branch": base, "title": title, "description": body,
		"remove_source_branch": true,
	}
	var out struct {
		WebURL string `json:"web_url"`
	}
	if err := c.api(ctx, http.MethodPost, "/projects/"+c.project()+"/merge_requests", in, &out); err != nil {
		return "", err
	}
	return out.WebURL, nil
}

// CommentIssue posts a note (used to back-link the merge request onto the source issue).
func (c *Client) CommentIssue(ctx context.Context, number int, body string) error {
	return c.api(ctx, http.MethodPost, "/projects/"+c.project()+"/issues/"+strconv.Itoa(number)+"/notes", map[string]any{"body": body}, nil)
}

// api performs one REST call. The token travels only in the PRIVATE-TOKEN
// header; errors carry the method, path, status and GitLab's message, never the
// token or the request body.
func (c *Client) api(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("gitlab %s %s: encode: %w", method, path, err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.apiURL+path, body)
	if err != nil {
		return fmt.Errorf("gitlab %s %s: %w", method, path, err)
	}
	req.Header.Set("PRIVATE-TOKEN", c.token)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("gitlab %s %s: %w", method, path, err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("gitlab %s %s: read: %w", method, path, err)
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return fmt.Errorf("gitlab %s %s: %s: %s", method, path, res.Status, apiMessage(data))
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("gitlab %s %s: decode: %w", method, path, err)
		}
	}
	return nil
}

// apiMessage extracts GitLab's error message ({"message": …} or {"error": …};
// message may be a string or an object), bounded, for an error string.
func apiMessage(data []byte) string {
	var raw struct {
		Message json.RawMessage `json:"message"`
		Error   string          `json:"error"`
	}
	msg := strings.TrimSpace(string(data))
	if json.Unmarshal(data, &raw) == nil {
		switch {
		case len(raw.Message) > 0:
			var s string
			if json.Unmarshal(raw.Message, &s) == nil {
				msg = s
			} else {
				msg = string(raw.Message)
			}
		case raw.Error != "":
			msg = raw.Error
		}
	}
	if len(msg) > 200 {
		msg = msg[:200] + "…"
	}
	return msg
}
