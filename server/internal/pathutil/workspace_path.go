package pathutil

import (
	"path/filepath"
	"strings"
)

// NormalizeWorkspacePath standardizes workspace paths so they can be
// consistently stored and matched across different path separator styles.
func NormalizeWorkspacePath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}

	p = filepath.Clean(p)
	p = filepath.ToSlash(p)

	if p == "/" {
		return p
	}

	if len(p) == 3 && p[1] == ':' && p[2] == '/' {
		return p
	}

	return strings.TrimSuffix(p, "/")
}
