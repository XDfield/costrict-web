package services

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
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

// Clone is the context-free entrypoint kept for existing callers (sync). It clones
// with a background context (no timeout).
func (s *GitService) Clone(repoURL, branch string) (*CloneResult, error) {
	return s.CloneContext(context.Background(), repoURL, branch)
}

// CloneContext clones repoURL@branch, honouring ctx cancellation/timeout so a hung
// or slow upstream aborts instead of pinning a worker goroutine indefinitely. The
// local-directory copy path is fast and ignores ctx (no network).
func (s *GitService) CloneContext(ctx context.Context, repoURL, branch string) (*CloneResult, error) {
	localPath := filepath.Join(s.TempBaseDir, fmt.Sprintf("sync-%d", time.Now().UnixNano()))
	if err := os.MkdirAll(localPath, 0755); err != nil {
		return nil, fmt.Errorf("failed to create temp dir: %w", err)
	}

	// If repoURL is a local directory, copy it instead of cloning.
	// go-git's PlainClone against a local non-bare repo may fall back
	// to the external "git" binary, which might not be available.
	if fi, err := os.Stat(repoURL); err == nil && fi.IsDir() {
		if err := copyDir(repoURL, localPath); err != nil {
			os.RemoveAll(localPath)
			return nil, fmt.Errorf("failed to copy local repo: %w", err)
		}
		repo, err := git.PlainOpen(localPath)
		if err != nil {
			os.RemoveAll(localPath)
			return nil, fmt.Errorf("failed to open copied repo: %w", err)
		}
		sha, err := s.getHeadSHAFromRepo(repo)
		if err != nil {
			os.RemoveAll(localPath)
			return nil, err
		}
		return &CloneResult{LocalPath: localPath, CommitSHA: sha}, nil
	}

	cloneOpts := &git.CloneOptions{
		URL:          repoURL,
		Depth:        1,
		SingleBranch: true,
	}
	if branch != "" {
		cloneOpts.ReferenceName = plumbing.NewBranchReferenceName(branch)
	}

	repo, err := git.PlainCloneContext(ctx, localPath, false, cloneOpts)
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

func copyDir(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		// Preserve the source file's mode bits (notably the executable bit) so a
		// copied-from-local-dir clone is byte-and-mode identical to the source. The
		// bundle pipeline relies on this to keep plugin hooks/scripts executable.
		mode := fs.FileMode(0644)
		if fi, statErr := d.Info(); statErr == nil {
			mode = fi.Mode().Perm()
		}
		return os.WriteFile(target, data, mode)
	})
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

// PackZip walks the subtree rooted at localPath/subPath and produces a lossless
// ZIP of every regular file (and symlink), returning the ZIP bytes and their
// sha256 hex digest.
//
// Behaviour required by the DB+HTTP bundle distribution channel:
//   - `.git` and `node_modules` directories are excluded (same skip rule as
//     ListFiles) so the archive is the equivalent of a fresh git clone with `.git`
//     stripped — never re-pack VCS metadata or dependency caches.
//   - File mode bits are preserved via SetMode(fi.Mode()); in particular the
//     executable bit on plugin hooks/scripts survives, otherwise extracted hooks
//     lose +x and fail to run.
//   - Paths inside the ZIP are relative to subPath (the plugin root), so extracting
//     yields the plugin directory directly with no extra prefix.
//   - Entries are written in deterministic (sorted) order with a fixed modtime so
//     the same input tree always yields the same ZIP bytes and therefore the same
//     sha256.
//
// subPath may be empty (pack the whole repo root). An empty subtree yields a valid
// empty ZIP rather than an error.
func (s *GitService) PackZip(localPath, subPath string) ([]byte, string, error) {
	root := localPath
	if subPath != "" {
		root = filepath.Join(localPath, filepath.FromSlash(subPath))
	}

	info, err := os.Stat(root)
	if err != nil {
		return nil, "", fmt.Errorf("pack root %q: %w", root, err)
	}
	if !info.IsDir() {
		return nil, "", fmt.Errorf("pack root %q is not a directory", root)
	}

	// Collect (relPath, absPath) for every file first so we can emit them in a
	// deterministic order -> deterministic ZIP bytes -> deterministic sha256.
	type entry struct {
		rel string
		abs string
	}
	var entries []entry

	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			name := d.Name()
			if name == ".git" || name == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}

		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		entries = append(entries, entry{rel: filepath.ToSlash(rel), abs: path})
		return nil
	})
	if walkErr != nil {
		return nil, "", fmt.Errorf("walk %q: %w", root, walkErr)
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].rel < entries[j].rel })

	// Fixed modtime keeps the archive byte-for-byte reproducible.
	fixedTime := time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		fi, statErr := os.Lstat(e.abs)
		if statErr != nil {
			zw.Close()
			return nil, "", fmt.Errorf("stat %q: %w", e.abs, statErr)
		}

		hdr, hdrErr := zip.FileInfoHeader(fi)
		if hdrErr != nil {
			zw.Close()
			return nil, "", fmt.Errorf("zip header %q: %w", e.rel, hdrErr)
		}
		hdr.Name = e.rel
		hdr.Method = zip.Deflate
		hdr.Modified = fixedTime
		hdr.SetMode(fi.Mode()) // preserve exec bit + symlink mode

		w, createErr := zw.CreateHeader(hdr)
		if createErr != nil {
			zw.Close()
			return nil, "", fmt.Errorf("create zip entry %q: %w", e.rel, createErr)
		}

		if fi.Mode()&os.ModeSymlink != 0 {
			// Store the link target as the entry content (matches archive/zip's
			// representation of symlinks).
			target, linkErr := os.Readlink(e.abs)
			if linkErr != nil {
				zw.Close()
				return nil, "", fmt.Errorf("readlink %q: %w", e.abs, linkErr)
			}
			if _, wErr := io.WriteString(w, target); wErr != nil {
				zw.Close()
				return nil, "", fmt.Errorf("write symlink %q: %w", e.rel, wErr)
			}
			continue
		}

		f, openErr := os.Open(e.abs)
		if openErr != nil {
			zw.Close()
			return nil, "", fmt.Errorf("open %q: %w", e.abs, openErr)
		}
		if _, copyErr := io.Copy(w, f); copyErr != nil {
			f.Close()
			zw.Close()
			return nil, "", fmt.Errorf("copy %q: %w", e.rel, copyErr)
		}
		f.Close()
	}

	if closeErr := zw.Close(); closeErr != nil {
		return nil, "", fmt.Errorf("finalize zip: %w", closeErr)
	}

	data := buf.Bytes()
	return data, s.ContentHash(data), nil
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
