package services

import "testing"

func TestParseSourceURL(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		cloneURL string
		branch   string
		subPath  string
		wantErr  bool
	}{
		{
			name:     "tree with branch and nested subpath",
			raw:      "https://github.com/owner/repo/tree/main/a/b/c",
			cloneURL: "https://github.com/owner/repo",
			branch:   "main",
			subPath:  "a/b/c",
		},
		{
			name:     "tree with branch and single subpath segment",
			raw:      "https://github.com/abhigyanpatwari/GitNexus/tree/main/gitnexus-claude-plugin",
			cloneURL: "https://github.com/abhigyanpatwari/GitNexus",
			branch:   "main",
			subPath:  "gitnexus-claude-plugin",
		},
		{
			name:     "tree with branch no subpath",
			raw:      "https://github.com/owner/repo/tree/develop",
			cloneURL: "https://github.com/owner/repo",
			branch:   "develop",
			subPath:  "",
		},
		{
			name:     "repo root no tree",
			raw:      "https://github.com/owner/repo",
			cloneURL: "https://github.com/owner/repo",
			branch:   "",
			subPath:  "",
		},
		{
			name:     "repo root with trailing slash",
			raw:      "https://github.com/owner/repo/",
			cloneURL: "https://github.com/owner/repo",
			branch:   "",
			subPath:  "",
		},
		{
			name:     "strips .git suffix",
			raw:      "https://github.com/owner/repo.git",
			cloneURL: "https://github.com/owner/repo",
			branch:   "",
			subPath:  "",
		},
		{
			name:     "self-hosted host preserved",
			raw:      "https://gitea.costrict.ai/costrict-plugins-repo/some-plugin/tree/main/sub",
			cloneURL: "https://gitea.costrict.ai/costrict-plugins-repo/some-plugin",
			branch:   "main",
			subPath:  "sub",
		},
		{
			name:     "branch with surrounding whitespace trimmed",
			raw:      "  https://github.com/owner/repo/tree/main/x  ",
			cloneURL: "https://github.com/owner/repo",
			branch:   "main",
			subPath:  "x",
		},
		{
			name:     "unrecognised blob path degrades to repo root",
			raw:      "https://github.com/owner/repo/blob/main/file.md",
			cloneURL: "https://github.com/owner/repo",
			branch:   "",
			subPath:  "",
		},
		{
			name:     "file scheme returns local path as clone target",
			raw:      "file:///tmp/local/repo",
			cloneURL: "/tmp/local/repo",
			branch:   "",
			subPath:  "",
		},
		{
			name:    "empty string errors",
			raw:     "",
			wantErr: true,
		},
		{
			name:    "whitespace only errors",
			raw:     "   ",
			wantErr: true,
		},
		{
			name:    "missing scheme errors",
			raw:     "github.com/owner/repo",
			wantErr: true,
		},
		{
			name:    "owner only errors",
			raw:     "https://github.com/owner",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cloneURL, branch, subPath, err := parseSourceURL(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got none (clone=%q branch=%q sub=%q)", tt.raw, cloneURL, branch, subPath)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", tt.raw, err)
			}
			if cloneURL != tt.cloneURL {
				t.Errorf("cloneURL = %q, want %q", cloneURL, tt.cloneURL)
			}
			if branch != tt.branch {
				t.Errorf("branch = %q, want %q", branch, tt.branch)
			}
			if subPath != tt.subPath {
				t.Errorf("subPath = %q, want %q", subPath, tt.subPath)
			}
		})
	}
}

func TestMapToMirror(t *testing.T) {
	tests := []struct {
		name       string
		cloneURL   string
		mirrorBase string
		want       string
	}{
		{
			name:       "empty mirror base returns input unchanged",
			cloneURL:   "https://github.com/owner/repo",
			mirrorBase: "",
			want:       "https://github.com/owner/repo",
		},
		{
			name:       "whitespace mirror base treated as empty",
			cloneURL:   "https://github.com/owner/repo",
			mirrorBase: "   ",
			want:       "https://github.com/owner/repo",
		},
		{
			name:       "github rewritten to mirror flat scheme",
			cloneURL:   "https://github.com/owner/repo",
			mirrorBase: "https://gitea.costrict.ai/costrict-plugins-repo",
			want:       "https://gitea.costrict.ai/costrict-plugins-repo/owner-repo",
		},
		{
			name:       "trailing slash on mirror base normalised",
			cloneURL:   "https://github.com/owner/repo",
			mirrorBase: "https://gitea.costrict.ai/costrict-plugins-repo/",
			want:       "https://gitea.costrict.ai/costrict-plugins-repo/owner-repo",
		},
		{
			name:       "non-github host left untouched",
			cloneURL:   "https://gitlab.com/owner/repo",
			mirrorBase: "https://gitea.costrict.ai/costrict-plugins-repo",
			want:       "https://gitlab.com/owner/repo",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := mapToMirror(tt.cloneURL, tt.mirrorBase); got != tt.want {
				t.Errorf("mapToMirror(%q, %q) = %q, want %q", tt.cloneURL, tt.mirrorBase, got, tt.want)
			}
		})
	}
}
