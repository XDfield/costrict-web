package clawagent

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// --- Chat ---

func (rt *ClawAgentRuntime) handleChat(c *gin.Context) {
	userID := rt.resolveUserID(c)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	var req struct {
		Message   string `json:"message"`
		SessionID string `json:"sessionId,omitempty"`
		Stream    bool   `json:"stream,omitempty"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.Message == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "message is required"})
		return
	}

	baseKey := fmt.Sprintf("agent:clawagent:web:%s:%s", c.ClientIP(), userID)
	sessionID, err := rt.resolveActiveSession(userID, baseKey, "direct")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "session error"})
		return
	}

	if req.Stream {
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("Connection", "keep-alive")

		eventCh, err := rt.runner.Run(c.Request.Context(), userID, sessionID, req.Message)
		if err != nil {
			c.SSEvent("error", gin.H{"error": err.Error()})
			return
		}

		c.Stream(func(w io.Writer) bool {
			for evt := range eventCh {
				if evt.IsFinal {
					c.SSEvent("done", gin.H{"sessionId": sessionID, "messageId": uuidString()})
					return false
				}
				if evt.Content != "" {
					c.SSEvent("token", gin.H{"content": evt.Content})
				}
				if evt.Error != "" {
					c.SSEvent("error", gin.H{"error": evt.Error})
				}
			}
			return false
		})
	} else {
		eventCh, err := rt.runner.Run(c.Request.Context(), userID, sessionID, req.Message)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		var fullContent string
		for evt := range eventCh {
			if evt.Content != "" {
				fullContent += evt.Content
			}
			if evt.IsFinal {
				break
			}
		}

		c.JSON(http.StatusOK, gin.H{
			"content":   fullContent,
			"sessionId": sessionID,
		})
	}
}

// --- Sessions ---

func (rt *ClawAgentRuntime) handleListSessions(c *gin.Context) {
	userID := rt.resolveUserID(c)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	metas, err := rt.SessionMeta.ListByUser(c.Request.Context(), userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	type sessionItem struct {
		ID           string `json:"id"`
		LastMessage  string `json:"lastMessage"`
		LastActiveAt string `json:"lastActiveAt"`
		MessageCount int    `json:"messageCount"`
	}
	items := make([]sessionItem, 0, len(metas))
	for _, m := range metas {
		items = append(items, sessionItem{
			ID:           m.SessionID,
			LastActiveAt: m.LastMessageAt.Format(time.RFC3339),
			MessageCount: m.MessageCount,
		})
	}
	c.JSON(http.StatusOK, gin.H{"sessions": items})
}

func (rt *ClawAgentRuntime) handleGetSession(c *gin.Context) {
	sessionID := c.Param("id")
	meta, err := rt.SessionMeta.Get(c.Request.Context(), sessionID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return
	}
	c.JSON(http.StatusOK, meta)
}

func (rt *ClawAgentRuntime) handleDeleteSession(c *gin.Context) {
	sessionID := c.Param("id")
	if err := rt.SessionMeta.Archive(c.Request.Context(), sessionID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true})
}

// --- Personas ---

func (rt *ClawAgentRuntime) handleListPersonas(c *gin.Context) {
	userID := rt.resolveUserID(c)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	personas, err := rt.PersonaMgr.ListByUser(c.Request.Context(), userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"personas": personas})
}

func (rt *ClawAgentRuntime) handleCreatePersona(c *gin.Context) {
	userID := rt.resolveUserID(c)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	var p Persona
	if err := c.ShouldBindJSON(&p); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	p.UserID = userID

	if err := rt.PersonaMgr.Create(c.Request.Context(), &p); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, gin.H{
		"id":        p.ID,
		"name":      p.Name,
		"isDefault": p.IsDefault,
	})
}

func (rt *ClawAgentRuntime) handleUpdatePersona(c *gin.Context) {
	id := c.Param("id")
	persona, err := rt.PersonaMgr.LoadByID(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "persona not found"})
		return
	}

	var updates Persona
	if err := c.ShouldBindJSON(&updates); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if updates.SoulContent != "" {
		persona.SoulContent = updates.SoulContent
	}
	if updates.IdentityContent != "" {
		persona.IdentityContent = updates.IdentityContent
	}
	if updates.UserContext != "" {
		persona.UserContext = updates.UserContext
	}
		if updates.Name != "" {
			persona.Name = updates.Name
		}
		persona.IsDefault = updates.IsDefault

	if err := rt.PersonaMgr.Update(c.Request.Context(), persona); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, persona)
}

func (rt *ClawAgentRuntime) handleDeletePersona(c *gin.Context) {
	id := c.Param("id")
	if err := rt.PersonaMgr.Delete(c.Request.Context(), id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true})
}

func (rt *ClawAgentRuntime) handleSetDefaultPersona(c *gin.Context) {
	userID := rt.resolveUserID(c)
	id := c.Param("id")
	if err := rt.PersonaMgr.SetDefault(c.Request.Context(), userID, id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true})
}

// --- Providers ---

func (rt *ClawAgentRuntime) handleListProviders(c *gin.Context) {
	userID := rt.resolveUserID(c)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	providers, err := rt.ProviderMgr.ListByUser(c.Request.Context(), userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Mask API keys in response
	type providerView struct {
		ID           uint   `json:"id"`
		Name         string `json:"name"`
		ProviderType string `json:"providerType"`
		ModelName    string `json:"modelName"`
		IsDefault    bool   `json:"isDefault"`
	}
	views := make([]providerView, 0, len(providers))
	for _, p := range providers {
		views = append(views, providerView{
			ID:           p.ID,
			Name:         p.Name,
			ProviderType: p.ProviderType,
			ModelName:    p.ModelName,
			IsDefault:    p.IsDefault,
		})
	}
	c.JSON(http.StatusOK, gin.H{"providers": views})
}

func (rt *ClawAgentRuntime) handleCreateProvider(c *gin.Context) {
	userID := rt.resolveUserID(c)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	var p Provider
	if err := c.ShouldBindJSON(&p); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	p.UserID = userID

	if err := rt.ProviderMgr.Create(c.Request.Context(), &p); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, gin.H{
		"id":           p.ID,
		"name":         p.Name,
		"providerType": p.ProviderType,
		"modelName":    p.ModelName,
		"isDefault":    p.IsDefault,
	})
}

func (rt *ClawAgentRuntime) handleUpdateProvider(c *gin.Context) {
	userID := rt.resolveUserID(c)
	id := parseInt(c.Param("id"))

	prov, err := rt.ProviderMgr.LoadByID(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "provider not found"})
		return
	}
	prov.UserID = userID

	var updates Provider
	if err := c.ShouldBindJSON(&updates); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if updates.APIKeyEncrypted != "" {
		prov.APIKeyEncrypted = updates.APIKeyEncrypted
	}
	if updates.BaseURL != "" {
		prov.BaseURL = updates.BaseURL
	}
	if updates.ModelName != "" {
		prov.ModelName = updates.ModelName
	}
	if updates.Name != "" {
		prov.Name = updates.Name
	}
	prov.IsDefault = updates.IsDefault

	if err := rt.ProviderMgr.Update(c.Request.Context(), prov); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"id":           prov.ID,
		"name":         prov.Name,
		"providerType": prov.ProviderType,
		"modelName":    prov.ModelName,
		"isDefault":    prov.IsDefault,
	})
}

func (rt *ClawAgentRuntime) handleDeleteProvider(c *gin.Context) {
	id := parseInt(c.Param("id"))
	if err := rt.ProviderMgr.Delete(c.Request.Context(), id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true})
}

func (rt *ClawAgentRuntime) handleTestProvider(c *gin.Context) {
	id := parseInt(c.Param("id"))
	result, err := rt.ProviderMgr.TestProvider(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, result)
}

// --- Memory ---

func (rt *ClawAgentRuntime) handleGetMemory(c *gin.Context) {
	userID := rt.resolveUserID(c)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	content, err := rt.MemoryMgr.Load(c.Request.Context(), userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"content":     content,
		"lengthBytes": len(content),
	})
}

func (rt *ClawAgentRuntime) handleUpdateMemory(c *gin.Context) {
	userID := rt.resolveUserID(c)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	var req struct {
		Content string `json:"content"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	truncated := false
	if len(req.Content) > MaxMemoryBytes {
		req.Content = req.Content[:MaxMemoryBytes]
		truncated = true
	}

	if err := rt.MemoryMgr.Save(c.Request.Context(), userID, req.Content); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"content":     req.Content,
		"lengthBytes": len(req.Content),
		"truncated":   truncated,
	})
}

// --- Workspaces ---

type workspaceView struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	DeviceID string `json:"deviceId"`
}

func (rt *ClawAgentRuntime) handleListWorkspaces(c *gin.Context) {
	userID := rt.resolveUserID(c)

	var workspaces []struct {
		ID   string `gorm:"column:id"`
		Name string `gorm:"column:name"`
	}
	if err := rt.db.WithContext(c.Request.Context()).
		Table("workspaces").
		Select("id, name").
		Where("user_id = ?", userID).
		Find(&workspaces).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	views := make([]workspaceView, 0, len(workspaces))
	for _, w := range workspaces {
		views = append(views, workspaceView{
			ID:   w.ID,
			Name: w.Name,
		})
	}
	c.JSON(http.StatusOK, gin.H{"workspaces": views})
}

func (rt *ClawAgentRuntime) handleListDelegationTasks(c *gin.Context) {
	workspaceID := c.Param("id")
	tasks, err := rt.TaskRegistry.ListByWorkspace(c.Request.Context(), workspaceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"tasks": tasks})
}

func (rt *ClawAgentRuntime) handleGetDelegationTask(c *gin.Context) {
	taskID := c.Param("taskId")
	task, err := rt.TaskRegistry.Get(c.Request.Context(), taskID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
		return
	}
	c.JSON(http.StatusOK, task)
}

func (rt *ClawAgentRuntime) handleAbortDelegationTask(c *gin.Context) {
	taskID := c.Param("taskId")
	task, err := rt.TaskRegistry.Get(c.Request.Context(), taskID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
		return
	}

	if task.DeviceID != "" && task.ConversationID != "" {
		if err := rt.DeviceProxy.AbortPrompt(c.Request.Context(), task.DeviceID, task.ConversationID); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	}

	if err := rt.TaskRegistry.UpdateStatus(c.Request.Context(), taskID, TaskStatusCancelled); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true})
}

// --- Helpers ---

func parseInt(s string) uint {
	var i uint
	_, _ = fmt.Sscanf(s, "%d", &i)
	return i
}

// estimateTokens provides a rough token count estimation.
func estimateTokens(data string) int {
	return int(math.Ceil(float64(len(data)) / 4.0))
}
