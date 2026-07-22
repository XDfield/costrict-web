package workflow

import (
	"errors"
	"testing"
)

func TestTeamShort_HappyPath(t *testing.T) {
	got, err := TeamShort("7f3c9a1e-d4b2-4c8e-9a3f-1b2c3d4e5f6a")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "7f3c9a1e" {
		t.Errorf("got %q", got)
	}
}

func TestTeamShort_BadUUID(t *testing.T) {
	_, err := TeamShort("not-a-uuid")
	if !errors.Is(err, ErrInvalidTeamID) {
		t.Errorf("got %v, want ErrInvalidTeamID", err)
	}
}

func TestInstanceShort_HappyPath(t *testing.T) {
	got, err := InstanceShort("f3a8b2c1-9d7e-4a2b-8e1f-1234567890ab")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "f3a8b2c1" {
		t.Errorf("got %q", got)
	}
}

func TestEscapeDefSlug_HappyPath(t *testing.T) {
	cases := []struct{ in, want string }{
		{"bug-fix-flow", "bug-fix-flow"},
		{"BugFixFlow", "bugfixflow"},         // uppercase → lowercase
		{"bug.fix_flow-1", "bug.fix_flow-1"}, // allowed punctuation
		{"café", "caf_"},                     // non-ASCII → _
		{"", "unnamed"},                      // empty fallback
		{".hidden", "_.hidden"},              // leading dot → prefixed
		{"UPPER-Case", "upper-case"},
	}
	for i, c := range cases {
		got := EscapeDefSlug(c.in)
		if got != c.want {
			t.Errorf("case %d: got %q want %q", i, got, c.want)
		}
	}
}

func TestWfRepoPath_HappyPath(t *testing.T) {
	got, err := WfRepoPath("bug-fix-flow", "7f3c9a1e-d4b2-4c8e-9a3f-1b2c3d4e5f6a")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	want := "t-7f3c9a1e/wf-bug-fix-flow"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestWfRepoPath_EmptySlug(t *testing.T) {
	_, err := WfRepoPath("", "7f3c9a1e-d4b2-4c8e-9a3f-1b2c3d4e5f6a")
	if !errors.Is(err, ErrInvalidSlug) {
		t.Errorf("got %v, want ErrInvalidSlug", err)
	}
}

func TestWfRepoPath_BadTeamID(t *testing.T) {
	_, err := WfRepoPath("bug-fix-flow", "not-a-uuid")
	if !errors.Is(err, ErrInvalidTeamID) {
		t.Errorf("got %v, want ErrInvalidTeamID", err)
	}
}

func TestWfBranchName_HappyPath(t *testing.T) {
	got, err := WfBranchName("f3a8b2c1-9d7e-4a2b-8e1f-1234567890ab")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "inst-f3a8b2c1" {
		t.Errorf("got %q", got)
	}
}

func TestWfBranchName_BadInstanceID(t *testing.T) {
	_, err := WfBranchName("not-a-uuid")
	if !errors.Is(err, ErrInvalidInstanceID) {
		t.Errorf("got %v, want ErrInvalidInstanceID", err)
	}
}
