package config

import "testing"

// A repo's host decides where it is cloned from and how its changes are opened;
// github stays the default so existing registries are unchanged.
func TestRepoHostDefaultsAndURLs(t *testing.T) {
	cfg, err := Load(writeTempConfig(t, "repos:\n - {name: a, owner: o, repo: r, token_ref: t}\n - {name: b, owner: smoovly-apps, repo: tab-split, token_ref: t, host: gitlab}\n - {name: c, owner: g/sub, repo: p, token_ref: t, host: gitlab, api_url: https://gitlab.example.com/api/v4/}\n"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	want := []struct{ host, clone, api string }{
		{HostGitHub, "https://github.com/o/r.git", "https://api.github.com"},
		{HostGitLab, "https://gitlab.com/smoovly-apps/tab-split.git", "https://gitlab.com/api/v4"},
		{HostGitLab, "https://gitlab.example.com/g/sub/p.git", "https://gitlab.example.com/api/v4"},
	}
	for i, w := range want {
		r := cfg.Repos[i]
		if r.Host != w.host || r.CloneURL() != w.clone || r.APIBase() != w.api {
			t.Errorf("repos[%d] = host %q clone %q api %q, want %q %q %q", i, r.Host, r.CloneURL(), r.APIBase(), w.host, w.clone, w.api)
		}
	}
}

func TestLoadRejectsBadHost(t *testing.T) {
	for name, doc := range map[string]string{
		"unknown host": "repos:\n  - {name: a, owner: o, repo: r, token_ref: t, host: bitbucket}\n",
		"http api_url": "repos:\n  - {name: a, owner: o, repo: r, token_ref: t, host: gitlab, api_url: http://gitlab.local/api/v4}\n",
	} {
		if _, err := Load(writeTempConfig(t, doc)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
