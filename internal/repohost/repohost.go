// Package repohost is the host-side seam between the controller and the
// repository host a registered repo lives on: GitHub (a pull request through
// gh) or GitLab (a merge request through the REST API). Every adapter keeps
// the token on the host (D3): in the environment for a one-shot git credential
// helper or in a request header, never in argv, config, the container, the
// source export, the bundle, the DB, or an error message.
package repohost

import (
	"context"
	"fmt"

	"github.com/iQonAi/devbox/internal/config"
	"github.com/iQonAi/devbox/internal/github"
	"github.com/iQonAi/devbox/internal/gitlab"
	"github.com/iQonAi/devbox/internal/prompt"
)

// Host runs the publish-side operations for one repository with one token.
type Host interface {
	// CredentialUsername is the username git's credential helper presents with
	// the token (GitHub: x-access-token; GitLab: oauth2).
	CredentialUsername() string
	// FetchIssue renders an issue into the prompt input (--issue).
	FetchIssue(ctx context.Context, number int) (prompt.Issue, error)
	// Push pushes the feature branch to the repository.
	Push(ctx context.Context, worktreeDir, branch string) error
	// OpenChange opens the host's review object for branch onto base — a pull
	// request or a merge request — and returns its URL. Never auto-merged.
	OpenChange(ctx context.Context, worktreeDir, branch, base, title, body string) (string, error)
	// CommentIssue posts a comment on the source issue (the back-link).
	CommentIssue(ctx context.Context, number int, body string) error
}

// New returns the adapter for kind ("" = github) over owner/repo with token.
// apiURL overrides the host's public API base (self-managed instances).
func New(kind, apiURL, owner, repo, token string) (Host, error) {
	switch kind {
	case "", config.HostGitHub:
		return github.New(owner, repo, token), nil
	case config.HostGitLab:
		return gitlab.New(apiURL, owner, repo, token), nil
	}
	return nil, fmt.Errorf("unknown repository host %q", kind)
}
