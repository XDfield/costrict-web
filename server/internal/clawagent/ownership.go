package clawagent

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
)

// requireOwnedProvider loads a provider and verifies it belongs to userID.
// On miss or ownership mismatch, writes 404 and returns ok=false to avoid
// leaking whether the resource exists for another user.
func (rt *ClawAgentRuntime) requireOwnedProvider(
	c *gin.Context, ctx context.Context, id uint, userID string,
) (*Provider, bool) {
	prov, err := rt.ProviderMgr.LoadByID(ctx, id)
	if err != nil || prov == nil || prov.UserID != userID {
		c.JSON(http.StatusNotFound, gin.H{"error": "provider not found"})
		return nil, false
	}
	return prov, true
}

// requireOwnedSession loads a session meta and verifies ownership.
func (rt *ClawAgentRuntime) requireOwnedSession(
	c *gin.Context, ctx context.Context, sessionID, userID string,
) (*SessionMeta, bool) {
	meta, err := rt.SessionMeta.Get(ctx, sessionID)
	if err != nil || meta == nil || meta.UserID != userID {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return nil, false
	}
	return meta, true
}

// requireOwnedPersona loads a persona and verifies ownership.
func (rt *ClawAgentRuntime) requireOwnedPersona(
	c *gin.Context, ctx context.Context, id, userID string,
) (*Persona, bool) {
	persona, err := rt.PersonaMgr.LoadByID(ctx, id)
	if err != nil || persona == nil || persona.UserID != userID {
		c.JSON(http.StatusNotFound, gin.H{"error": "persona not found"})
		return nil, false
	}
	return persona, true
}

// requireOwnedTask loads a delegation task and verifies ownership.
func (rt *ClawAgentRuntime) requireOwnedTask(
	c *gin.Context, ctx context.Context, taskID, userID string,
) (*WorkspaceTask, bool) {
	task, err := rt.TaskRegistry.Get(ctx, taskID)
	if err != nil || task == nil || task.UserID != userID {
		c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
		return nil, false
	}
	return task, true
}
