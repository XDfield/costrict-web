package services

import (
	"archive/zip"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// writeFakePlugin lays out a fake plugin working tree under root:
//
//	root/
//	  .plugin.json            (regular)
//	  README.md               (regular)
//	  hooks/run.sh            (executable, 0755)
//	  scripts/util.py         (regular, nested subdir)
//	  .git/config             (VCS noise -> must be excluded)
//	  node_modules/dep/i.js   (dependency noise -> must be excluded)
//
// When sub != "" the plugin lives at root/<sub> and the .git noise is placed at
// both the repo root and inside the plugin dir to prove subPath rooting + exclusion.
func writeFakePlugin(t *testing.T, root, sub string) {
	t.Helper()

	pluginDir := root
	if sub != "" {
		pluginDir = filepath.Join(root, filepath.FromSlash(sub))
	}

	mkfile := func(rel string, content []byte, mode os.FileMode) {
		full := filepath.Join(pluginDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", rel, err)
		}
		if err := os.WriteFile(full, content, 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
		if err := os.Chmod(full, mode); err != nil {
			t.Fatalf("chmod %s: %v", rel, err)
		}
	}

	mkfile(".plugin.json", []byte(`{"name":"demo"}`), 0o644)
	mkfile("README.md", []byte("# Demo plugin\n"), 0o644)
	mkfile("hooks/run.sh", []byte("#!/bin/sh\necho hi\n"), 0o755)
	mkfile("scripts/util.py", []byte("print('util')\n"), 0o644)

	// .git noise inside the plugin dir (and at repo root when sub set).
	if err := os.MkdirAll(filepath.Join(pluginDir, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, ".git", "config"), []byte("[core]\n"), 0o644); err != nil {
		t.Fatalf("write .git/config: %v", err)
	}
	// node_modules noise inside the plugin dir.
	if err := os.MkdirAll(filepath.Join(pluginDir, "node_modules", "dep"), 0o755); err != nil {
		t.Fatalf("mkdir node_modules: %v", err)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, "node_modules", "dep", "i.js"), []byte("module.exports={}\n"), 0o644); err != nil {
		t.Fatalf("write node_modules file: %v", err)
	}

	if sub != "" {
		// Repo-root .git that must never appear when packing the subPath.
		if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
			t.Fatalf("mkdir root .git: %v", err)
		}
		if err := os.WriteFile(filepath.Join(root, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
			t.Fatalf("write root .git/HEAD: %v", err)
		}
		// A sibling file at the repo root that must NOT be in the subPath bundle.
		if err := os.WriteFile(filepath.Join(root, "OUTSIDE.txt"), []byte("not in bundle\n"), 0o644); err != nil {
			t.Fatalf("write OUTSIDE.txt: %v", err)
		}
	}
}

// readZipEntries returns name -> content and name -> exec-bit for every entry.
func readZipEntries(t *testing.T, data []byte) (map[string][]byte, map[string]bool) {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	contents := make(map[string][]byte)
	execBits := make(map[string]bool)
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open zip entry %s: %v", f.Name, err)
		}
		b, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("read zip entry %s: %v", f.Name, err)
		}
		contents[f.Name] = b
		execBits[f.Name] = f.Mode()&0o100 != 0
	}
	return contents, execBits
}

// sortedContentKeys returns the entry names of a packed zip in sorted order.
// (services already has a generic sortedKeys helper; this avoids relying on its
// iteration order assumptions in tests.)
func sortedContentKeys(m map[string][]byte) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func TestPackZip_RepoRoot_Lossless(t *testing.T) {
	root := t.TempDir()
	writeFakePlugin(t, root, "")

	svc := &GitService{}
	data, sha, err := svc.PackZip(root, "")
	if err != nil {
		t.Fatalf("PackZip: %v", err)
	}
	if sha == "" {
		t.Fatal("expected non-empty sha")
	}

	contents, execBits := readZipEntries(t, data)

	want := []string{".plugin.json", "README.md", "hooks/run.sh", "scripts/util.py"}
	got := sortedContentKeys(contents)
	if !equalStrSlice(got, want) {
		t.Fatalf("zip entries = %v, want %v", got, want)
	}

	if string(contents["hooks/run.sh"]) != "#!/bin/sh\necho hi\n" {
		t.Errorf("hooks/run.sh content mismatch: %q", contents["hooks/run.sh"])
	}
	if string(contents["scripts/util.py"]) != "print('util')\n" {
		t.Errorf("scripts/util.py content mismatch: %q", contents["scripts/util.py"])
	}

	if !execBits["hooks/run.sh"] {
		t.Error("hooks/run.sh lost its executable bit")
	}
	if execBits["README.md"] {
		t.Error("README.md unexpectedly has executable bit")
	}
}

func TestPackZip_ExcludesGitAndNodeModules(t *testing.T) {
	root := t.TempDir()
	writeFakePlugin(t, root, "")

	svc := &GitService{}
	data, _, err := svc.PackZip(root, "")
	if err != nil {
		t.Fatalf("PackZip: %v", err)
	}
	contents, _ := readZipEntries(t, data)
	for name := range contents {
		if name == ".git" || hasPrefixSlash(name, ".git/") {
			t.Errorf(".git entry leaked into bundle: %s", name)
		}
		if name == "node_modules" || hasPrefixSlash(name, "node_modules/") {
			t.Errorf("node_modules entry leaked into bundle: %s", name)
		}
	}
}

func TestPackZip_SubPathRooted(t *testing.T) {
	root := t.TempDir()
	writeFakePlugin(t, root, "plugins/demo")

	svc := &GitService{}
	data, _, err := svc.PackZip(root, "plugins/demo")
	if err != nil {
		t.Fatalf("PackZip: %v", err)
	}
	contents, execBits := readZipEntries(t, data)

	// Paths must be relative to the subPath (plugin root), with no "plugins/demo/" prefix.
	want := []string{".plugin.json", "README.md", "hooks/run.sh", "scripts/util.py"}
	got := sortedContentKeys(contents)
	if !equalStrSlice(got, want) {
		t.Fatalf("subpath zip entries = %v, want %v", got, want)
	}
	if !execBits["hooks/run.sh"] {
		t.Error("hooks/run.sh lost its executable bit in subpath pack")
	}
	if _, ok := contents["OUTSIDE.txt"]; ok {
		t.Error("repo-root sibling OUTSIDE.txt leaked into subpath bundle")
	}
}

func TestPackZip_Deterministic(t *testing.T) {
	root := t.TempDir()
	writeFakePlugin(t, root, "")

	svc := &GitService{}
	_, sha1, err := svc.PackZip(root, "")
	if err != nil {
		t.Fatalf("PackZip first: %v", err)
	}
	_, sha2, err := svc.PackZip(root, "")
	if err != nil {
		t.Fatalf("PackZip second: %v", err)
	}
	if sha1 != sha2 {
		t.Errorf("sha not deterministic: %s != %s", sha1, sha2)
	}
}

func TestPackZip_MissingRootErrors(t *testing.T) {
	svc := &GitService{}
	if _, _, err := svc.PackZip(filepath.Join(t.TempDir(), "does-not-exist"), ""); err == nil {
		t.Fatal("expected error for missing root")
	}
}

func equalStrSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func hasPrefixSlash(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}
