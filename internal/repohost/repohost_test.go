package repohost

import (
	"testing"

	"github.com/iQonAi/devbox/internal/github"
	"github.com/iQonAi/devbox/internal/gitlab"
)

func TestNewPicksTheAdapter(t *testing.T) {
	gh, err := New("", "", "o", "r", "t")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := gh.(*github.Client); !ok || gh.CredentialUsername() != "x-access-token" {
		t.Errorf("default host = %T (%s), want github", gh, gh.CredentialUsername())
	}
	gl, err := New("gitlab", "", "smoovly-apps", "tab-split", "t")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := gl.(*gitlab.Client); !ok || gl.CredentialUsername() != "oauth2" {
		t.Errorf("gitlab host = %T (%s)", gl, gl.CredentialUsername())
	}
	if _, err := New("bitbucket", "", "o", "r", "t"); err == nil {
		t.Error("unknown host accepted")
	}
}
