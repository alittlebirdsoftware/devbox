package creds

import (
	"os"
	"path/filepath"
	"testing"
)

// write drops a credential file into a temp dir and points
// CREDENTIALS_DIRECTORY at it for the duration of the test
func write(t *testing.T, name, content string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o400); err != nil {
		t.Fatalf("write credential: %v", err)
	}
	t.Setenv("CREDENTIALS_DIRECTORY", dir)
}

func TestGetReadsCredential(t *testing.T) {
	write(t, "gh-token-devbox", "github_pat_example\n")

	token, ok, err := Get("gh-token-devbox")
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	if !ok {
		t.Fatal("credentials not found")
	}

	if token != "github_pat_example" {
		t.Fatalf("token = %q (trailing new line not trimmed?)", token)
	}
}

// A missing credential is the public-repo case: not found, not an error.
func TestMissingCredentialIsNotAnError(t *testing.T) {
	write(t, "other", "x")

	_, ok, err := Get("gh-token-devbox")
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	if ok {
		t.Error("reported a credential that does not exist")
	}
}

// Outside systemd there is no credentials directory at all.
func TestNoCredentialsDirectory(t *testing.T) {
	t.Setenv("CREDENTIALS_DIRECTORY", "")

	_, ok, err := Get("gh-token-devbox")
	if err != nil || ok {
		t.Errorf("got (ok=%v, err=%v), want (false, nil)", ok, err)
	}
}

// An empty file is a file a real misconfiguration and must not look like "no token".
func TestEmptyCredentialIsAnError(t *testing.T) {
	write(t, "gh-token-devbox", "\n")

	if _, _, err := Get("gh-token-devbox"); err == nil {
		t.Fatal("expected an error for an empty credential, got nil")
	}
}

func TestRefCannotEscapeDirectory(t *testing.T) {
	write(t, "gh-token-devbox", "x")

	for _, ref := range []string{"../../etc/passwd", "sub/token", ""} {
		if _, _, err := Get(ref); err == nil {
			t.Errorf("ref %q was accepted, want rejection", ref)
		}
	}
}

func TestUseDirReadsAtUseTime(t *testing.T) {
	t.Setenv("CREDENTIALS_DIRECTORY", t.TempDir())
	live := t.TempDir()
	UseDir(live)
	defer UseDir("")
	p := filepath.Join(live, "gh-token-app")
	if err := os.WriteFile(p, []byte("first\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	if v, ok, err := Get("gh-token-app"); err != nil || !ok || v != "first" {
		t.Fatalf("Get = %q %v %v", v, ok, err)
	}
	// A refresher replaces the file (write + rename); the next Get sees it without a restart.
	if err := os.WriteFile(p+".tmp", []byte("second"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(p+".tmp", p); err != nil {
		t.Fatal(err)
	}
	if v, _, _ := Get("gh-token-app"); v != "second" {
		t.Fatalf("after rotation Get = %q, want second", v)
	}
}
