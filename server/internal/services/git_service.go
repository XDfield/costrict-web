package services

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

type GitService struct {
	TempBaseDir string
}

type CloneResult struct {
	LocalPath string
	CommitSHA string
}

func (s *GitService) Clone(repoURL, branch string) (*CloneResult, error) {
	localPath := filepath.Join(s.TempBaseDir, fmt.Sprintf("sync-%d", time.Now().UnixNano()))
	if err := os.MkdirAll(localPath, 0755); err != nil {
		return nil, fmt.Errorf("failed to create temp dir: %w", err)
	}

	// SSRF defense-in-depth (secreport 20260731141243580377, CVSS 5.3):
	// reject local-path inputs outright. Before this guard, a caller supplying
	// an existing local directory as repoURL triggered a copyDir fallback that
	// read arbitrary server files into our temp dir — an arbitrary-file-read
	// sink. file:// URLs are rejected for the same reason. Write-time validation
	// at the registry handlers covers the user-facing entry points; this guard
	// covers any internal caller and any DB row poisoned after registration.
	if strings.HasPrefix(repoURL, "file://") {
		os.RemoveAll(localPath)
		return nil, fmt.Errorf("refusing to clone file:// url: %s", repoURL)
	}
	if fi, err := os.Stat(repoURL); err == nil && fi.IsDir() {
		os.RemoveAll(localPath)
		return nil, fmt.Errorf("refusing to clone local directory: %s", repoURL)
	}

	cloneOpts := &git.CloneOptions{
		URL:          repoURL,
		Depth:        1,
		SingleBranch: true,
	}
	if branch != "" {
		cloneOpts.ReferenceName = plumbing.NewBranchReferenceName(branch)
	}

	repo, err := git.PlainClone(localPath, false, cloneOpts)
	if err != nil {
		os.RemoveAll(localPath)
		return nil, fmt.Errorf("failed to clone repo: %w", err)
	}

	sha, err := s.getHeadSHAFromRepo(repo)
	if err != nil {
		os.RemoveAll(localPath)
		return nil, err
	}

	return &CloneResult{LocalPath: localPath, CommitSHA: sha}, nil
}

// ValidateExternalRepoURL enforces that raw is a safe external repository URL
// for server-side git operations (the sync-registry clone flow). It is the
// write-time gate in front of GitService.Clone — see secreport
// 20260731141243580377 (CVSS 5.3).
//
// Accepted forms match what git itself accepts:
//   - http://, https://, git://, ssh://
//   - SCP-like "git@host:path" / "user@host:path"
//
// Rejected:
//   - empty / non-absolute URLs / missing host
//   - file:// and any non-remote scheme (javascript:, data:, ftp:, ...)
//   - localhost name aliases (localhost, ip6-localhost, ip6-loopback, *.localhost)
//   - loopback / link-local / unspecified / cloud-metadata IPs
//
// RFC1918 / private ranges are intentionally ALLOWED: on-prem customers run
// internal git servers, and the sync flow is already gated by SyncDisabled
// plus the clone-time local-path guard in Clone. DNS-rebinding protection
// relies on go-git's transport at dial time.
func ValidateExternalRepoURL(raw string) error {
	if raw == "" {
		return fmt.Errorf("external url is required")
	}
	if isSCPLike(raw) {
		host := scpHost(raw)
		if host == "" {
			return fmt.Errorf("external url is missing host")
		}
		if isBlockedGitHost(host) {
			return fmt.Errorf("external url host is not allowed: %s", host)
		}
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid external url: %w", err)
	}
	if !u.IsAbs() {
		return fmt.Errorf("external url must be absolute")
	}
	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "http", "https", "git", "ssh":
	default:
		return fmt.Errorf("external url scheme must be http, https, git, or ssh")
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("external url must have a host")
	}
	if isBlockedGitHost(host) {
		return fmt.Errorf("external url host is not allowed: %s", host)
	}
	return nil
}

// isSCPLike detects the "user@host:path" SSH shorthand. url.Parse would treat
// such strings as a relative path with an empty scheme, so we intercept them
// before parsing.
func isSCPLike(raw string) bool {
	if strings.Contains(raw, "://") {
		return false
	}
	at := strings.IndexByte(raw, '@')
	if at < 0 {
		return false
	}
	rest := raw[at+1:]
	// Require a colon after the host portion to distinguish from a bare email.
	colon := strings.IndexByte(rest, ':')
	return colon > 0
}

// scpHost extracts the host segment from a "user@host:path" string.
func scpHost(raw string) string {
	at := strings.IndexByte(raw, '@')
	if at < 0 {
		return ""
	}
	rest := raw[at+1:]
	colon := strings.IndexByte(rest, ':')
	if colon < 0 {
		return ""
	}
	return rest[:colon]
}

// isBlockedGitHost catches localhost name aliases and the IP sinkholes that no
// legitimate external git remote would target. Private RFC1918 ranges are
// allowed (on-prem git servers).
func isBlockedGitHost(host string) bool {
	lower := strings.ToLower(host)
	switch lower {
	case "localhost", "ip6-localhost", "ip6-loopback":
		return true
	}
	if strings.HasSuffix(lower, ".localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return isBlockedGitIP(ip)
	}
	// Defense against non-canonical IPv4 encodings (inet_aton-style: hex 0x,
	// octal 0, decimal single-int, and multi-component forms like "127.1").
	// net.ParseIP rejects these forms, so without this canonicalization step
	// a host like "0x7f000001", "2130706433", or "0177.0.0.1" would slip
	// past the IP-class checks and be treated as a plain hostname —
	// exploitable on deployments using a libc resolver (cgo build with
	// getaddrinfo/inet_aton) that expands these forms to loopback /
	// link-local / metadata addresses. secreport 20260731141243580377.
	if canon := canonicalizeIPv4Host(host); canon != "" {
		if ip := net.ParseIP(canon); ip != nil {
			return isBlockedGitIP(ip)
		}
	}
	return false
}

// isBlockedGitIP classifies a parsed IP against the sinkhole set. Shared by
// the canonical-form path and the inet_aton-canonicalized path.
func isBlockedGitIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsUnspecified() {
		return true
	}
	if ip.Equal(net.IPv4(169, 254, 169, 254)) {
		return true
	}
	return false
}

// canonicalizeIPv4Host attempts to interpret host as an IPv4 literal in any
// of the forms libc inet_aton accepts that net.ParseIP does not, and returns
// the canonical dotted-decimal form. Returns "" if host is not an IPv4
// literal under these rules.
//
// Accepted forms (mirrors glibc inet_aton):
//   - 1 component: full 32-bit value in any base (e.g. 0x7f000001, 2130706433, 017700000001)
//   - 2 components: a.b where a is the high byte and b is a 24-bit value
//   - 3 components: a.b.c where a, b are bytes and c is 16-bit
//   - 4 components: standard dotted-quad with per-octet base-0 parsing
//
// Each component is parsed with strconv.ParseUint base=0, which auto-detects
// 0x (hex), 0o/0 (octal), 0b (binary), and decimal. We restrict the input
// charset to digits / dots / [xobXOB] so hostnames like "git01.internal"
// don't get misinterpreted.
func canonicalizeIPv4Host(host string) string {
	// Charset gate: anything outside [0-9.xXoObBa-fA-F] means it's a hostname.
	// The hex letters a-f are needed for 0x-prefixed components; we still
	// reject hostnames because they contain other letters (g-z, G-Z) or hyphens.
	for i := 0; i < len(host); i++ {
		c := host[i]
		if !((c >= '0' && c <= '9') || c == '.' || c == 'x' || c == 'X' ||
			c == 'o' || c == 'O' || c == 'b' || c == 'B' ||
			(c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return ""
		}
	}
	parts := strings.Split(host, ".")
	if len(parts) == 0 || len(parts) > 4 {
		return ""
	}
	vals := make([]uint32, len(parts))
	for i, p := range parts {
		if p == "" {
			return ""
		}
		v, err := strconv.ParseUint(p, 0, 32)
		if err != nil {
			return ""
		}
		vals[i] = uint32(v)
	}
	var ip uint32
	switch len(vals) {
	case 1:
		ip = vals[0]
	case 2:
		if vals[0] > 0xff || vals[1] > 0xffffff {
			return ""
		}
		ip = (vals[0] << 24) | vals[1]
	case 3:
		if vals[0] > 0xff || vals[1] > 0xff || vals[2] > 0xffff {
			return ""
		}
		ip = (vals[0] << 24) | (vals[1] << 16) | vals[2]
	case 4:
		for _, v := range vals {
			if v > 0xff {
				return ""
			}
		}
		ip = (vals[0] << 24) | (vals[1] << 16) | (vals[2] << 8) | vals[3]
	}
	return fmt.Sprintf("%d.%d.%d.%d", byte(ip>>24), byte(ip>>16), byte(ip>>8), byte(ip))
}

func (s *GitService) Fetch(localPath, branch string) (string, error) {
	repo, err := git.PlainOpen(localPath)
	if err != nil {
		return "", fmt.Errorf("failed to open repo: %w", err)
	}

	w, err := repo.Worktree()
	if err != nil {
		return "", fmt.Errorf("failed to get worktree: %w", err)
	}

	pullOpts := &git.PullOptions{Depth: 1}
	if branch != "" {
		pullOpts.ReferenceName = plumbing.NewBranchReferenceName(branch)
	}
	if err := w.Pull(pullOpts); err != nil && err != git.NoErrAlreadyUpToDate {
		return "", fmt.Errorf("failed to pull: %w", err)
	}

	return s.getHeadSHAFromRepo(repo)
}

func (s *GitService) GetHeadSHA(localPath string) (string, error) {
	repo, err := git.PlainOpen(localPath)
	if err != nil {
		return "", fmt.Errorf("failed to open repo: %w", err)
	}
	return s.getHeadSHAFromRepo(repo)
}

func (s *GitService) getHeadSHAFromRepo(repo *git.Repository) (string, error) {
	ref, err := repo.Head()
	if err != nil {
		return "", fmt.Errorf("failed to get HEAD: %w", err)
	}
	return ref.Hash().String(), nil
}

func (s *GitService) ListFiles(localPath string, includes, excludes []string) ([]string, error) {
	var files []string

	err := filepath.WalkDir(localPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if name == ".git" || name == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}

		rel, err := filepath.Rel(localPath, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)

		if len(includes) > 0 {
			matched := false
			for _, pattern := range includes {
				ok, _ := matchGlob(pattern, rel)
				if ok {
					matched = true
					break
				}
			}
			if !matched {
				return nil
			}
		}

		for _, pattern := range excludes {
			ok, _ := matchGlob(pattern, rel)
			if ok {
				return nil
			}
		}

		files = append(files, rel)
		return nil
	})

	return files, err
}

func (s *GitService) ReadFile(localPath, relPath string) ([]byte, error) {
	full := filepath.Join(localPath, filepath.FromSlash(relPath))
	return os.ReadFile(full)
}

func (s *GitService) ContentHash(content []byte) string {
	h := sha256.Sum256(content)
	return fmt.Sprintf("%x", h)
}

func (s *GitService) Cleanup(localPath string) error {
	return os.RemoveAll(localPath)
}

func matchGlob(pattern, name string) (bool, error) {
	if strings.Contains(pattern, "**") {
		parts := strings.SplitN(pattern, "**", 2)
		prefix := strings.TrimSuffix(parts[0], "/")
		suffix := strings.TrimPrefix(parts[1], "/")

		if prefix != "" && !strings.HasPrefix(name, prefix+"/") && name != prefix {
			return false, nil
		}

		checkName := name
		if prefix != "" {
			checkName = strings.TrimPrefix(name, prefix+"/")
		}

		if suffix == "" {
			return true, nil
		}

		if strings.Contains(suffix, "/") {
			return strings.HasSuffix(checkName, "/"+suffix) || checkName == suffix, nil
		}
		return filepath.Match(suffix, filepath.Base(checkName))
	}
	return filepath.Match(pattern, name)
}
