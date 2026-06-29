package clawagent

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"gorm.io/gorm"
)

// TaskRegistry manages workspace delegation tasks.
type TaskRegistry struct {
	db    *gorm.DB
	mu    sync.RWMutex
	tasks map[string]*WorkspaceTask
}

// NewTaskRegistry creates a new TaskRegistry.
func NewTaskRegistry(db *gorm.DB) *TaskRegistry {
	return &TaskRegistry{
		db:    db,
		tasks: make(map[string]*WorkspaceTask),
	}
}

// Create creates a new delegation task record.
func (r *TaskRegistry) Create(ctx context.Context, task *WorkspaceTask) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if err := r.db.WithContext(ctx).Create(task).Error; err != nil {
		return err
	}
	r.tasks[task.TaskID] = task
	return nil
}

// Get retrieves a task by taskID.
func (r *TaskRegistry) Get(ctx context.Context, taskID string) (*WorkspaceTask, error) {
	r.mu.RLock()
	task, ok := r.tasks[taskID]
	r.mu.RUnlock()
	if ok {
		return task, nil
	}

	var t WorkspaceTask
	if err := r.db.WithContext(ctx).Where("task_id = ?", taskID).First(&t).Error; err != nil {
		return nil, err
	}
	r.mu.Lock()
	r.tasks[taskID] = &t
	r.mu.Unlock()
	return &t, nil
}

// UpdateStatus updates the status of a task.
func (r *TaskRegistry) UpdateStatus(ctx context.Context, taskID, status string) error {
	return r.db.WithContext(ctx).
		Model(&WorkspaceTask{}).
		Where("task_id = ?", taskID).
		Update("status", status).Error
}

// UpdateProgress updates the progress summary of a task.
func (r *TaskRegistry) UpdateProgress(ctx context.Context, taskID, summary string) error {
	now := time.Now()
	return r.db.WithContext(ctx).
		Model(&WorkspaceTask{}).
		Where("task_id = ?", taskID).
		Updates(map[string]any{
			"progress_summary": summary,
			"last_event_at":    now,
		}).Error
}

// Complete marks a task as succeeded with output.
func (r *TaskRegistry) Complete(ctx context.Context, taskID, output string) error {
	now := time.Now()
	return r.db.WithContext(ctx).
		Model(&WorkspaceTask{}).
		Where("task_id = ?", taskID).
		Updates(map[string]any{
			"status":       TaskStatusSucceeded,
			"output":       output,
			"completed_at": now,
			"last_event_at": now,
		}).Error
}

// Fail marks a task as failed.
func (r *TaskRegistry) Fail(ctx context.Context, taskID, errMsg string) error {
	now := time.Now()
	return r.db.WithContext(ctx).
		Model(&WorkspaceTask{}).
		Where("task_id = ?", taskID).
		Updates(map[string]any{
			"status":       TaskStatusFailed,
			"error":        errMsg,
			"completed_at": now,
			"last_event_at": now,
		}).Error
}

// TimedOut marks a task as timed out.
func (r *TaskRegistry) TimedOut(ctx context.Context, taskID string) error {
	now := time.Now()
	return r.db.WithContext(ctx).
		Model(&WorkspaceTask{}).
		Where("task_id = ?", taskID).
		Updates(map[string]any{
			"status":       TaskStatusTimedOut,
			"completed_at": now,
		}).Error
}

// MarkDelivered marks a task as delivered.
func (r *TaskRegistry) MarkDelivered(ctx context.Context, taskID string) error {
	return r.db.WithContext(ctx).
		Model(&WorkspaceTask{}).
		Where("task_id = ?", taskID).
		Update("delivery_status", DeliveryStatusDelivered).Error
}

// MarkDeliveryFailed marks a task's delivery as failed.
func (r *TaskRegistry) MarkDeliveryFailed(ctx context.Context, taskID string) error {
	return r.db.WithContext(ctx).
		Model(&WorkspaceTask{}).
		Where("task_id = ?", taskID).
		Update("delivery_status", DeliveryStatusFailed).Error
}

// UpdateAnnounceRetry increments the announce retry count.
func (r *TaskRegistry) UpdateAnnounceRetry(ctx context.Context, taskID string, count int) error {
	return r.db.WithContext(ctx).
		Model(&WorkspaceTask{}).
		Where("task_id = ?", taskID).
		Update("announce_retry_count", count).Error
}

// ListRunning returns all running tasks for timeout checking.
func (r *TaskRegistry) ListRunning(ctx context.Context) ([]WorkspaceTask, error) {
	var tasks []WorkspaceTask
	if err := r.db.WithContext(ctx).
		Where("status IN ?", []string{TaskStatusQueued, TaskStatusRunning}).
		Find(&tasks).Error; err != nil {
		return nil, err
	}
	return tasks, nil
}

// MarkLost marks all non-terminal tasks for a device as lost (crash recovery).
func (r *TaskRegistry) MarkLost(ctx context.Context, deviceID string) error {
	return r.db.WithContext(ctx).
		Model(&WorkspaceTask{}).
		Where("device_id = ? AND status IN ?", deviceID, []string{TaskStatusQueued, TaskStatusRunning}).
		Update("status", TaskStatusLost).Error
}

// ListByWorkspace lists tasks for a workspace.
func (r *TaskRegistry) ListByWorkspace(ctx context.Context, workspaceID string) ([]WorkspaceTask, error) {
	var tasks []WorkspaceTask
	if err := r.db.WithContext(ctx).
		Where("workspace_id = ?", workspaceID).
		Order("created_at DESC").
		Limit(50).
		Find(&tasks).Error; err != nil {
		return nil, err
	}
	return tasks, nil
}

// ListByUser lists tasks for a user.
func (r *TaskRegistry) ListByUser(ctx context.Context, userID string) ([]WorkspaceTask, error) {
	var tasks []WorkspaceTask
	if err := r.db.WithContext(ctx).
		Where("user_id = ?", userID).
		Order("created_at DESC").
		Limit(50).
		Find(&tasks).Error; err != nil {
		return nil, err
	}
	return tasks, nil
}

// ListNonFinal returns tasks in non-terminal states (for crash recovery).
func (r *TaskRegistry) ListNonFinal(ctx context.Context) ([]WorkspaceTask, error) {
	var tasks []WorkspaceTask
	if err := r.db.WithContext(ctx).
		Where("status IN ?", []string{TaskStatusQueued, TaskStatusRunning}).
		Find(&tasks).Error; err != nil {
		return nil, err
	}
	return tasks, nil
}

const MaxAnnounceRetry = 5

// announceRetryDelay returns exponential backoff duration.
func announceRetryDelay(retryCount int) time.Duration {
	d := time.Duration(1<<retryCount) * time.Second
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	return d
}

// watchAndAnnounce subscribes to device SSE events for a task and announces the result.
func (rt *ClawAgentRuntime) watchAndAnnounce(ctx context.Context, taskID, deviceID, workspaceDir, convID string) {
	task, err := rt.TaskRegistry.Get(ctx, taskID)
	if err != nil {
		log.Printf("[clawagent] watchAndAnnounce: task %s not found: %v", taskID, err)
		return
	}

	eventCh, err := rt.DeviceProxy.SubscribeEvents(ctx, deviceID, workspaceDir)
	if err != nil {
		log.Printf("[clawagent] watchAndAnnounce: subscribe events failed: %v", err)
		return
	}

	for evt := range eventCh {
		// Filter by conversation_id
		if convID != "" {
			if cid, ok := evt.Properties["conversation_id"].(string); ok && cid != convID {
				continue
			}
		}

		switch evt.Type {
		case "message.part.updated":
			// Update progress summary
			if text, ok := evt.Properties["text"].(string); ok {
				_ = rt.TaskRegistry.UpdateProgress(ctx, taskID, text)
			}

		case "session.idle":
			// Task completed successfully
			messages, err := rt.DeviceProxy.GetMessages(ctx, deviceID, convID)
			output := ""
			if err == nil {
				for _, msg := range messages {
					if msg.Role == "assistant" {
						output += msg.Content + "\n"
					}
				}
			}
			_ = rt.TaskRegistry.Complete(ctx, taskID, output)
			log.Printf("[clawagent] watchAndAnnounce: task %s completed", taskID)
			rt.announceToAgent(task)
			return

		case "session.error":
			errMsg := ""
			if msg, ok := evt.Properties["message"].(string); ok {
				errMsg = msg
			}
			_ = rt.TaskRegistry.Fail(ctx, taskID, errMsg)
			log.Printf("[clawagent] watchAndAnnounce: task %s failed: %s", taskID, errMsg)
			rt.announceToAgent(task)
			return
		}
	}
}

// announceToAgent injects the task result back into the agent's conversation session.
func (rt *ClawAgentRuntime) announceToAgent(task *WorkspaceTask) {
	if task.DeliveryStatus == DeliveryStatusNotApplicable {
		return
	}

	statusEmoji := "✅"
	statusText := "已完成"
	switch task.Status {
	case TaskStatusFailed:
		statusEmoji = "❌"
		statusText = "失败"
	case TaskStatusTimedOut:
		statusEmoji = "⏰"
		statusText = "超时"
	case TaskStatusCancelled:
		statusEmoji = "🚫"
		statusText = "已取消"
	}

	output := task.Output
	if output == "" {
		output = task.ProgressSummary
	}

	callbackMsg := fmt.Sprintf(
		"[系统通知] 委托任务已完成\n"+
			" - 状态: %s %s\n"+
			" - 结果: %s\n"+
			"请基于此结果决定下一步。",
		statusEmoji, statusText, output)

	// Resolve current active session
	activeSID, err := rt.SessionMeta.ResolveActive(rt.bgCtx, task.UserID, task.AgentSessionBaseKey)
	if err != nil {
		log.Printf("[clawagent] announceToAgent: resolve active session failed: %v", err)
		_ = rt.TaskRegistry.MarkDeliveryFailed(rt.bgCtx, task.TaskID)
		return
	}

	eventCh, err := rt.runner.Run(rt.bgCtx, task.UserID, activeSID, callbackMsg)
	if err != nil {
		// Retry with exponential backoff
		retryCount := task.AnnounceRetryCount + 1
		if retryCount >= MaxAnnounceRetry {
			log.Printf("[clawagent] announceToAgent: max retry reached for task %s", task.TaskID)
			_ = rt.TaskRegistry.MarkDeliveryFailed(rt.bgCtx, task.TaskID)
			return
		}
		_ = rt.TaskRegistry.UpdateAnnounceRetry(rt.bgCtx, task.TaskID, retryCount)
		time.AfterFunc(announceRetryDelay(retryCount), func() {
			// Re-fetch task for latest state
			updatedTask, getErr := rt.TaskRegistry.Get(rt.bgCtx, task.TaskID)
			if getErr != nil {
				return
			}
			rt.announceToAgent(updatedTask)
		})
		return
	}

	// Consume the event channel (don't need the response, just wait for completion)
	go func() {
		for range eventCh {
		}
	}()

	_ = rt.TaskRegistry.MarkDelivered(rt.bgCtx, task.TaskID)
	log.Printf("[clawagent] announceToAgent: task %s delivered to session %s", task.TaskID, activeSID)
}
