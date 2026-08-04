package services

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateExternalRepoURL(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{"empty", "", true},
		{"file scheme", "file:///etc/passwd", true},
		{"file scheme host", "file://localhost/etc/passwd", true},
		{"javascript scheme", "javascript:alert(1)", true},
		{"data scheme", "data:text/plain,hello", true},
		{"ftp scheme", "ftp://example.com/repo.git", true},
		{"not absolute", "example.com/repo.git", true},
		{"missing host", "http://", true},
		{"localhost literal", "http://localhost/repo.git", true},
		{"localhost suffix", "http://attacker.localhost/repo.git", true},
		{"ip6 loopback name", "http://ip6-loopback/repo.git", true},
		{"loopback ipv4", "http://127.0.0.1/repo.git", true},
		{"loopback ipv6", "http://[::1]/repo.git", true},
		{"link-local metadata", "http://169.254.169.254/latest/meta-data/", true},
		{"link-local range", "http://169.254.1.1/repo.git", true},
		{"unspecified ipv4", "http://0.0.0.0/repo.git", true},
		{"valid https", "https://github.com/org/repo.git", false},
		{"valid http", "http://example.com/repo.git", false},
		{"valid git scheme", "git://github.com/org/repo.git", false},
		{"valid ssh scheme", "ssh://git@github.com:22/org/repo.git", false},
		{"valid scp-like", "git@github.com:org/repo.git", false},
		{"valid scp-like other user", "deploy@git.internal.svc:team/repo", false},
		{"valid rfc1918 internal", "http://10.0.0.5/git/repo.git", false},
		{"valid private hostname", "http://git.internal.svc/repo.git", false},
		{"scp-like localhost", "git@localhost:org/repo.git", true},
		{"scp-like loopback ip", "git@127.0.0.1:org/repo.git", true},
		{"scp-like metadata ip", "git@169.254.169.254:org/repo.git", true},
		// Non-canonical IPv4 encodings (inet_aton-style). net.ParseIP rejects
		// these, so without canonicalizeIPv4Host they slip past the IP-class
		// check and become reachable via a libc resolver on cgo builds.
		{"hex loopback single int", "http://0x7f000001/repo.git", true},
		{"decimal loopback single int", "http://2130706433/repo.git", true},
		{"octal loopback dotted", "http://0177.0.0.1/repo.git", true},
		{"hex metadata dotted", "http://0xa9.0xfe.0xa9.0xfe/repo.git", true},
		{"short loopback 2-comp", "http://127.1/repo.git", true},
		{"decimal metadata single int", "http://2852039166/repo.git", true},
		// Negative control: a hostname that starts with digits but contains
		// non-IP-literal characters must NOT be flagged as a bypass attempt.
		{"digit-prefixed hostname still allowed", "http://git01.internal.svc/repo.git", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateExternalRepoURL(tc.raw)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error for %q, got nil", tc.raw)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.raw, err)
			}
		})
	}
}

// TestClone_RejectsLocalDirectory verifies the clone-time guard rejects an
// existing local directory — the pre-fix copyDir fallback was an arbitrary-
// file-read sink. secreport 20260731141243580377 (CVSS 5.3).
func TestClone_RejectsLocalDirectory(t *testing.T) {
	tmp := t.TempDir()
	svcDir := filepath.Join(tmp, "svc")
	if err := os.MkdirAll(svcDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	secret := filepath.Join(svcDir, "secret.txt")
	if err := os.WriteFile(secret, []byte("server-secret"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	s := &GitService{TempBaseDir: tmp}
	_, err := s.Clone(svcDir, "")
	if err == nil {
		t.Fatal("expected Clone to reject local directory, got nil error")
	}
	if !strings.Contains(err.Error(), "refusing to clone local directory") {
		t.Fatalf("expected local-directory refusal, got: %v", err)
	}

	// Verify no sync-* artifacts left behind in TempBaseDir (Clone cleans up).
	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "sync-") {
			t.Fatalf("temp dir not cleaned up: %s", e.Name())
		}
	}
}

// TestClone_RejectsFileScheme verifies the clone-time guard rejects file://
// URLs outright, even if the path doesn't exist on disk.
func TestClone_RejectsFileScheme(t *testing.T) {
	tmp := t.TempDir()
	s := &GitService{TempBaseDir: tmp}
	_, err := s.Clone("file:///etc/passwd", "")
	if err == nil {
		t.Fatal("expected Clone to reject file:// URL, got nil error")
	}
	if !strings.Contains(err.Error(), "refusing to clone file://") {
		t.Fatalf("expected file:// refusal, got: %v", err)
	}
}
