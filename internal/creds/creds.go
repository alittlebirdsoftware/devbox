// Package creds resolves secrets delivered by systemd LoadCredential.
//
// Secrets exist only as 0400 files in $CREDENTIALS_DIRECTORY, owned by the
// service user, for the lifetime of the process (D3: the token is host-only and
// never enters a container). Nothing here is ever logged: a token that reaches
// the journal is a leaked token.
package creds

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// liveDir, when set, replaces $CREDENTIALS_DIRECTORY: a directory the daemon
// reads at use time, which a root-owned refresher rewrites. LoadCredential is a
// snapshot taken at service start, so a credential that rotates faster than
// the daemon restarts (a one-hour, repo-scoped GitHub App installation token)
// must come from here instead. The refresher keeps the same guarantees: files
// owned by the service user, mode 0400, in a 0500 directory.
var liveDir string

// UseDir makes Get read from dir. Call once at startup, before serving.
func UseDir(dir string) { liveDir = dir }

// Get returns the credential name ref. The boolean reports whether one was
// found; a missing credeintal is not an error, because a public repo needs no
// token and the daemon also runs outside systemd during development.
func Get(ref string) (string, bool, error) {
	dir := os.Getenv("CREDENTIALS_DIRECTORY")
	if liveDir != "" {
		dir = liveDir
	}
	if dir == "" {
		return "", false, nil // not running under systemd
	}

	// ref comes from config.yaml, so it is operator-controlled rather than
	// hostile m-- but it names a file, and a name containing a separator would
	// reach outside the credentials directory, Refuse rather than resolve
	if ref == "" || strings.ContainsAny(ref, `/\`) || ref == ".." {
		return "", false, fmt.Errorf("invalid credentials ref %q: must be a bare file name", ref)
	}

	data, err := os.ReadFile(filepath.Join(dir, ref))
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		// Note the path is safe to report; the contents never are.
		return "", false, fmt.Errorf("read credentials %q: %w", ref, err)
	}

	// Trailing newlines are near-universal in secret files and break auth in
	// ways that are miserable to debug.
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", false, fmt.Errorf("credential %q is empty", ref)
	}
	return token, true, nil
}
