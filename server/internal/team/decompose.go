package team

import (
	"fmt"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

// DecomposeRequest is the request body for the decompose endpoint.
type DecomposeRequest struct {
	Prompt       string         `json:"prompt" binding:"required"`
	Context      map[string]any `json:"context,omitempty"`
	FencingToken int64          `json:"fencingToken,omitempty"`
}

// DecomposeResultItem is a single task returned by the teammate's LLM.
type DecomposeResultItem struct {
	TaskID       string   `json:"taskId"`
	Description  string   `json:"description"`
	RepoAffinity []string `json:"repoAffinity,omitempty"`
	FileHints    []string `json:"fileHints,omitempty"`
	Dependencies []string `json:"dependencies,omitempty"`
	Priority     int      `json:"priority,omitempty"`
}

// toTeamTask converts a DecomposeResultItem into a TeamTask for persistence.
func (item DecomposeResultItem) toTeamTask(sessionID string) TeamTask {
	t := TeamTask{
		ID:          uuid.New().String(),
		SessionID:   sessionID,
		Description: item.Description,
		Status:      TaskStatusPending,
		Priority:    item.Priority,
		MaxRetries:  3,
	}
	if t.Priority == 0 {
		t.Priority = 5
	}
	if len(item.RepoAffinity) > 0 {
		t.RepoAffinity = pq.StringArray(item.RepoAffinity)
	}
	if len(item.FileHints) > 0 {
		t.FileHints = pq.StringArray(item.FileHints)
	}
	if len(item.Dependencies) > 0 {
		t.Dependencies = pq.StringArray(item.Dependencies)
	}
	return t
}

// buildFallbackTasks creates a single-task plan when decomposition fails.
func buildFallbackTasks(prompt, sessionID string) []TeamTask {
	return []TeamTask{
		{
			ID:          uuid.New().String(),
			SessionID:   sessionID,
			Description: prompt,
			Status:      TaskStatusPending,
			Priority:    5,
			MaxRetries:  3,
		},
	}
}

// pickDecomposeTarget selects a decomposition target.
// Strategy:
// 1) Prefer an online non-leader teammate.
// 2) If none is available, fall back to the online leader machine itself.
// Returns (memberID, machineID, error).
func pickDecomposeTarget(hub *Hub, store *Store, sessionID string) (string, string, error) {
	leaderMachineID := hub.GetLeaderMachineID(sessionID)
	members, err := store.ListMembers(sessionID)
	if err != nil || len(members) == 0 {
		return "", "", fmt.Errorf("no teammates available")
	}

	var leaderMemberID string

	for _, m := range members {
		if m.Status != MemberStatusOnline {
			continue
		}
		// Verify the selected machine itself has an active WS connection.
		// SessionConnCount(sessionID) only tells whether *someone* is connected,
		// which may still select an offline/stale member and cause 60s timeouts.
		if !hub.IsMachineOnline(sessionID, m.MachineID) {
			continue
		}
		if leaderMachineID != "" && m.MachineID == leaderMachineID {
			leaderMemberID = m.ID
			continue
		}
		return m.ID, m.MachineID, nil
	}

	// No online teammate available; allow leader self-decomposition.
	if leaderMachineID != "" && hub.IsMachineOnline(sessionID, leaderMachineID) {
		return leaderMemberID, leaderMachineID, nil
	}

	return "", "", fmt.Errorf("no online teammate or leader available for decomposition")
}
