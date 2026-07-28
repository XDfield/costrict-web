// Package workflow implements the workflow repo path / branch algorithm
// from WORKFLOW_REPO_PATH_ALGORITHM.md v2.0 plus the workflow/init handler
// orchestration backing POST /api/internal/workflow/init.
//
// §A: wfRepoPath(workflow_def_slug, team_id) → "t-<team_short>/wf-<def_slug_escaped>"
// §B: wfBranchName(instance_id)              → "inst-<inst_short>"
//
// Both are deterministic pure functions — same inputs in / same outputs out,
// no DB, no cache. The HTTP handler layer composes the algorithm output
// with the tenant Gitea base_url to produce wf_clone_url / wf_web_url.

package workflow

import (
	"errors"
	"regexp"
	"strings"
)

// ErrInvalidSlug — workflow_def_slug was empty or rejected by escape rules.
// escape is permissive (down-escapes unknown chars to _) so empty is the
// only failure mode; surfaced for clean 400s at the handler layer.
var ErrInvalidSlug = errors.New("workflow: workflow_def_slug is required")

// ErrInvalidTeamID — team_id wasn't a UUID we can derive team_short from.
var ErrInvalidTeamID = errors.New("workflow: team_id must be a UUID")

// ErrInvalidInstanceID — instance_id wasn't a UUID we can derive inst_short from.
var ErrInvalidInstanceID = errors.New("workflow: instance_id must be a UUID")

// uuidRe mirrors teamns.uuidRe; duplicated to keep this package leaf-level
// (no cross-package dep on teamns just for a regex).
var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// allowedSlugChar matches the per-char whitelist from §3.A.5.
var allowedSlugChar = regexp.MustCompile(`[a-z0-9._-]`)

// TeamShort derives the 8-hex team_short from a UUID team_id.
func TeamShort(teamID string) (string, error) {
	if !uuidRe.MatchString(teamID) {
		return "", ErrInvalidTeamID
	}
	return teamID[:8], nil
}

// InstanceShort derives the 8-hex inst_short from a UUID instance_id.
func InstanceShort(instanceID string) (string, error) {
	if !uuidRe.MatchString(instanceID) {
		return "", ErrInvalidInstanceID
	}
	return instanceID[:8], nil
}

// EscapeDefSlug applies §3.A.5: keep [a-z0-9._-], lowercase [A-Z], replace
// anything else with _, prefix-leading "." with "_", empty → "unnamed".
func EscapeDefSlug(s string) string {
	if s == "" {
		return "unnamed"
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, ch := range s {
		switch {
		case allowedSlugChar.MatchString(string(ch)):
			b.WriteRune(ch)
		case ch >= 'A' && ch <= 'Z':
			b.WriteRune(ch + ('a' - 'A'))
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if strings.HasPrefix(out, ".") {
		out = "_" + out
	}
	if out == "" {
		out = "unnamed"
	}
	return out
}

// WfRepoPath returns "<owner>/<repo>" for the workflow def type repo.
// Implements §A. Calls TeamShort internally so it returns ErrInvalidTeamID
// when team_id isn't a UUID.
func WfRepoPath(workflowDefSlug, teamID string) (string, error) {
	if strings.TrimSpace(workflowDefSlug) == "" {
		return "", ErrInvalidSlug
	}
	short, err := TeamShort(teamID)
	if err != nil {
		return "", err
	}
	return "t-" + short + "/wf-" + EscapeDefSlug(workflowDefSlug), nil
}

// WfBranchName returns the instance branch "inst-<inst_short>".
// Implements §B.
func WfBranchName(instanceID string) (string, error) {
	short, err := InstanceShort(instanceID)
	if err != nil {
		return "", err
	}
	return "inst-" + short, nil
}
