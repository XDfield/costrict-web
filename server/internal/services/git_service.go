package services

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
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
