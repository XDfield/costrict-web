package clawagent

import (
	"context"
	"fmt"
	"log"
	"time"
)

// resolveActiveSession resolves the active session ID, handling freshness and version bumps.
func (rt *ClawAgentRuntime) resolveActiveSession(userID, baseKey, resetType string) (string, error) {
	meta, err := rt.SessionMeta.Active(rt.bgCtx, userID, baseKey)
	if err == ErrSessionNotFound {
		// First time: create v1
		sid := NewSessionID(baseKey, 1)
		_, err := rt.SessionMeta.Create(rt.bgCtx, userID, baseKey, 1, resetType)
		if err != nil {
			return "", fmt.Errorf("create session: %w", err)
		}
		return sid, nil
	}
	if err != nil {
		return "", err
	}

	if rt.isStale(meta, time.Now()) {
		// Bump version: archive old, create new
		_ = rt.SessionMeta.Archive(rt.bgCtx, meta.SessionID)
		newVer := meta.Version + 1
		sid := NewSessionID(baseKey, newVer)
		_, err := rt.SessionMeta.Create(rt.bgCtx, userID, baseKey, newVer, meta.ResetType)
		if err != nil {
			return "", fmt.Errorf("create new session version: %w", err)
		}
		return sid, nil
	}

	return meta.SessionID, nil
}

// isStale checks if a session is stale based on its reset type.
func (rt *ClawAgentRuntime) isStale(meta *SessionMeta, now time.Time) bool {
	switch meta.ResetType {
	case "direct":
		// Daily reset at configured hour
		return meta.LastMessageAt.Before(dailyResetAt(now, rt.agentCfg.SessionDailyResetHour))
	case "group", "thread":
		// Idle reset (default 30 min)
		return now.Sub(meta.LastMessageAt) > time.Duration(rt.agentCfg.SessionGroupIdleMinutes)*time.Minute
	case "event":
		// Idle reset (default 60 min)
		return now.Sub(meta.LastMessageAt) > time.Duration(rt.agentCfg.SessionEventIdleMinutes)*time.Minute
	case "task":
		// Idle reset (default 120 min)
		return now.Sub(meta.LastMessageAt) > time.Duration(rt.agentCfg.SessionTaskIdleMinutes)*time.Minute
	}
	return false
}

// dailyResetAt computes the most recent daily reset time.
func dailyResetAt(now time.Time, resetHour int) time.Time {
	reset := time.Date(now.Year(), now.Month(), now.Day(), resetHour, 0, 0, 0, now.Location())
	if now.Before(reset) {
		return reset.AddDate(0, 0, -1)
	}
	return reset
}

// pruneSessions removes old archived sessions.
func (rt *ClawAgentRuntime) pruneSessions() {
	ctx := rt.bgCtx
	cutoff := time.Now().AddDate(0, 0, -rt.agentCfg.SessionPruneAfterDays)

	// Delete archived sessions older than cutoff
	if _, err := rt.SessionMeta.DeleteArchivedBefore(ctx, cutoff); err != nil {
		return
	}

	// Prune excess sessions per user
	if _, err := rt.SessionMeta.PruneExcess(ctx, rt.agentCfg.SessionMaxPerUser); err != nil {
		return
	}
}

// checkTimeouts checks for timed-out delegation tasks.
func (rt *ClawAgentRuntime) checkTimeouts() {
	ctx := rt.bgCtx
	tasks, err := rt.TaskRegistry.ListRunning(ctx)
	if err != nil {
		return
	}

	for _, task := range tasks {
		if task.LastEventAt != nil && time.Since(*task.LastEventAt) > 30*time.Minute {
			_ = rt.TaskRegistry.TimedOut(ctx, task.TaskID)
		}
	}
}

// maybeCompact checks if a session needs compaction and performs it.
func (rt *ClawAgentRuntime) maybeCompact(ctx context.Context, sessionID string) {
	meta, err := rt.SessionMeta.Get(ctx, sessionID)
	if err != nil {
		return
	}

	if meta.TokenEstimate < rt.agentCfg.SessionMaxTokens {
		return
	}

	// Snapshot current message count
	beforeMsgCount := meta.MessageCount

	// Read session data
	session := rt.runner.GetSession(sessionID)
	if session == nil {
		return
	}

	// Double-check: if new messages arrived, skip
	currentMeta, _ := rt.SessionMeta.Get(ctx, sessionID)
	if currentMeta.MessageCount != beforeMsgCount {
		return
	}

	// LLM summarize
	summary, err := rt.runner.SummarizeSession(ctx, sessionID, rt.agentCfg.CompactionKeepRecent)
	if err != nil {
		return
	}

	// Update token estimate
	est := estimateTokens(summary)
	_ = rt.SessionMeta.UpdateTokenEstimate(ctx, sessionID, est)
}

// maybeCompactAll scans all active sessions for compaction need.
func (rt *ClawAgentRuntime) maybeCompactAll() {
	// This is a periodic check; actual compaction is triggered per-session in streamResponse
}

// recoverTasks scans non-final tasks on startup and recovers or marks them as lost.
func (rt *ClawAgentRuntime) recoverTasks() {
	ctx := rt.bgCtx
	tasks, err := rt.TaskRegistry.ListNonFinal(ctx)
	if err != nil {
		log.Printf("[clawagent] recoverTasks: failed to list non-final tasks: %v", err)
		return
	}

	if len(tasks) == 0 {
		return
	}

	log.Printf("[clawagent] recoverTasks: found %d non-final tasks", len(tasks))

	for _, task := range tasks {
		deviceID := task.DeviceID

		// Check if device is online via gateway
		_, gwErr := rt.gwRegistry.GetDeviceGateway(deviceID)
		online := gwErr == nil

		if online {
			if task.ConversationID != "" {
				// Device is online and conversation exists — it's still running
				log.Printf("[clawagent] recoverTasks: task %s still running on device %s, keeping as-is", task.TaskID, deviceID)
			} else {
				// Device is online but no conversation — queued task never sent, reset to queued
				log.Printf("[clawagent] recoverTasks: task %s was queued but not sent, resetting", task.TaskID)
				_ = rt.TaskRegistry.UpdateStatus(ctx, task.TaskID, TaskStatusQueued)
			}
		} else {
			// Device is offline — mark as lost
			log.Printf("[clawagent] recoverTasks: task %s device %s offline, marking lost", task.TaskID, deviceID)
			_ = rt.TaskRegistry.MarkLost(ctx, deviceID)
		}
	}
}
