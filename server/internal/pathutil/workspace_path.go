package pathutil

import (
	"path/filepath"
	"strings"
)

// NormalizeWorkspacePath standardizes workspace paths so they can be
// consistently stored and matched across different path separator styles.
// It always converts backslashes to forward slashes regardless of the host OS,
// because paths originate from client devices which may run on a different OS
// than the server.
func NormalizeWorkspacePath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}

	// Always convert backslashes to forward slashes (handles Windows client
	// paths even when the server runs on Linux).
	p = strings.ReplaceAll(p, `\`, "/")

	// Collapse duplicate slashes and resolve . / ..
	p = filepath.Clean(p)

	// filepath.Clean may reintroduce backslashes on Windows; normalize again.
	p = strings.ReplaceAll(p, `\`, "/")

	if p == "/" {
		return p
	}

	if len(p) == 3 && p[1] == ':' && p[2] == '/' {
		return p
	}

	return strings.TrimSuffix(p, "/")
}
