package team

import (
	"errors"
	"strings"
	"time"

	"gorm.io/gorm"
)

// Store handles all database operations for the team module.
type Store struct {
	db *gorm.DB
}

func NewStore(db *gorm.DB) *Store {
	return &Store{db: db}
}

// ─── Session ───────────────────────────────────────────────────────────────

func (s *Store) CreateSession(sess *TeamSession) error {
	return s.db.Create(sess).Error
}

func (s *Store) GetSession(id string) (*TeamSession, error) {
	var sess TeamSession
	err := s.db.First(&sess, "id = ?", id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &sess, err
}

func (s *Store) UpdateSession(id string, updates map[string]any) error {
	return s.db.Model(&TeamSession{}).Where("id = ?", id).Updates(updates).Error
}

func (s *Store) DeleteSession(id string) error {
	return s.db.Delete(&TeamSession{}, "id = ?", id).Error
}

func (s *Store) ListSessionsByCreator(creatorID string) ([]TeamSession, error) {
	var sessions []TeamSession
	err := s.db.Where("creator_id = ?", creatorID).Order("created_at DESC").Find(&sessions).Error
	return sessions, err
}

// ─── Member ────────────────────────────────────────────────────────────────

func (s *Store) CreateMember(m *TeamSessionMember) error {
	// Backward-compatible insert: older databases may not yet have newer
	// capability columns on team_session_members.
	return s.db.Select(
		"ID",
		"SessionID",
		"UserID",
		"MachineID",
		"MachineName",
		"Role",
		"Status",
		"ConnectedAt",
		"LastHeartbeat",
	).Create(m).Error
}

func (s *Store) GetMember(id string) (*TeamSessionMember, error) {
	var m TeamSessionMember
	err := s.db.First(&m, "id = ?", id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &m, err
}

func (s *Store) GetMemberByMachine(sessionID, machineID string) (*TeamSessionMember, error) {
	var m TeamSessionMember
	err := s.db.Where("session_id = ? AND machine_id = ?", sessionID, machineID).First(&m).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &m, err
}

// GetMemberByMachineUnscoped returns a member by session+machine, including soft-deleted rows.
func (s *Store) GetMemberByMachineUnscoped(sessionID, machineID string) (*TeamSessionMember, error) {
	var m TeamSessionMember
	err := s.db.Unscoped().Where("session_id = ? AND machine_id = ?", sessionID, machineID).First(&m).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &m, err
}

// GetMemberByMachineAnySessionUnscoped returns a member by machine across any session,
// including soft-deleted rows. Used for legacy schemas that enforced global machine uniqueness.
func (s *Store) GetMemberByMachineAnySessionUnscoped(machineID string) (*TeamSessionMember, error) {
	var m TeamSessionMember
	err := s.db.Unscoped().Where("machine_id = ?", machineID).Order("updated_at DESC").First(&m).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &m, err
}

func (s *Store) ListMembers(sessionID string) ([]TeamSessionMember, error) {
	var members []TeamSessionMember
	err := s.db.Where("session_id = ?", sessionID).Order("created_at ASC").Find(&members).Error
	return members, err
}

func (s *Store) UpdateMember(id string, updates map[string]any) error {
	return s.db.Model(&TeamSessionMember{}).Where("id = ?", id).Updates(updates).Error
}

func (s *Store) UpdateMemberHeartbeat(id string) error {
	return s.db.Model(&TeamSessionMember{}).Where("id = ?", id).
		Update("last_heartbeat", time.Now()).Error
}

func (s *Store) DeleteMember(id string) error {
	return s.db.Delete(&TeamSessionMember{}, "id = ?", id).Error
}

// ─── Task ──────────────────────────────────────────────────────────────────

func (s *Store) CreateTask(t *TeamTask) error {
	return s.db.Create(t).Error
}

func (s *Store) CreateTasks(tasks []TeamTask) error {
	return s.db.Create(&tasks).Error
}

func (s *Store) GetTask(id string) (*TeamTask, error) {
	var t TeamTask
	err := s.db.First(&t, "id = ?", id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &t, err
}

func (s *Store) ListTasks(sessionID string) ([]TeamTask, error) {
	var tasks []TeamTask
	err := s.db.Where("session_id = ?", sessionID).Order("priority DESC, created_at ASC").Find(&tasks).Error
	return tasks, err
}

func (s *Store) ListPendingTasks(sessionID string) ([]TeamTask, error) {
	var tasks []TeamTask
	err := s.db.Where("session_id = ? AND status = ?", sessionID, TaskStatusPending).
		Order("priority DESC, created_at ASC").Find(&tasks).Error
	return tasks, err
}

func (s *Store) UpdateTask(id string, updates map[string]any) error {
	return s.db.Model(&TeamTask{}).Where("id = ?", id).Updates(updates).Error
}

// RetryTask increments retry_count and resets the task to pending/assigned so it
// can be dispatched again. Returns the updated task, or nil if the task has
// exhausted its retries or does not exist.
func (s *Store) RetryTask(taskID string) (*TeamTask, error) {
	var task TeamTask
	if err := s.db.First(&task, "id = ?", taskID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	if task.RetryCount >= task.MaxRetries {
		return nil, nil
	}
	updates := map[string]any{
		"retry_count":   task.RetryCount + 1,
		"status":        TaskStatusAssigned,
		"error_message": "",
		"claimed_at":    nil,
		"started_at":    nil,
	}
	if err := s.db.Model(&TeamTask{}).Where("id = ?", taskID).Updates(updates).Error; err != nil {
		return nil, err
	}
	task.RetryCount++
	task.Status = TaskStatusAssigned
	return &task, nil
}

// ClaimTask atomically transitions a task from pending/assigned → claimed.
func (s *Store) ClaimTask(taskID, memberID string) error {
	now := time.Now()
	result := s.db.Model(&TeamTask{}).
		Where("id = ? AND status IN ?", taskID, []string{TaskStatusPending, TaskStatusAssigned}).
		Updates(map[string]any{
			"assigned_member_id": memberID,
			"status":             TaskStatusClaimed,
			"claimed_at":         now,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return errors.New("task not available for claiming")
	}
	return nil
}

// UnlockDependentTasks checks all tasks in the session that list completedTaskID
// in their Dependencies array. For each, if ALL its dependencies are now completed,
// the task is promoted to 'pending' and returned so the caller can notify the
// assigned machine.
func (s *Store) UnlockDependentTasks(sessionID, completedTaskID string) ([]TeamTask, error) {
	// Step 1: tasks that mention completedTaskID in their dependencies array
	var candidates []TeamTask
	if err := s.db.
		Where("session_id = ? AND ? = ANY(dependencies)", sessionID, completedTaskID).
		Find(&candidates).Error; err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, nil
	}

	// Step 2: for each candidate, verify ALL dependencies are completed
	var unlocked []TeamTask
	for _, task := range candidates {
		if task.Status != TaskStatusPending && task.Status != TaskStatusAssigned {
			continue // only unlock tasks that are still waiting
		}
		if len(task.Dependencies) == 0 {
			continue
		}

		var blockedCount int64
		if err := s.db.Model(&TeamTask{}).
			Where("id = ANY(?) AND status != ?", task.Dependencies, TaskStatusCompleted).
			Count(&blockedCount).Error; err != nil {
			return nil, err
		}
		if blockedCount > 0 {
			continue // still has incomplete dependencies
		}

		// All dependencies done — promote to pending
		if err := s.db.Model(&TeamTask{}).
			Where("id = ?", task.ID).
			Update("status", TaskStatusPending).Error; err != nil {
			return nil, err
		}
		task.Status = TaskStatusPending
		unlocked = append(unlocked, task)
	}
	return unlocked, nil
}

// ─── Approval ──────────────────────────────────────────────────────────────

func (s *Store) CreateApproval(a *TeamApprovalRequest) error {
	return s.db.Create(a).Error
}

func (s *Store) GetApproval(id string) (*TeamApprovalRequest, error) {
	var a TeamApprovalRequest
	err := s.db.First(&a, "id = ?", id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &a, err
}

func (s *Store) ListPendingApprovals(sessionID string) ([]TeamApprovalRequest, error) {
	var approvals []TeamApprovalRequest
	err := s.db.Where("session_id = ? AND status = ?", sessionID, "pending").
		Order("created_at ASC").Find(&approvals).Error
	return approvals, err
}

func (s *Store) UpdateApproval(id string, updates map[string]any) error {
	return s.db.Model(&TeamApprovalRequest{}).Where("id = ?", id).Updates(updates).Error
}

// ─── Repo Affinity ─────────────────────────────────────────────────────────

func (s *Store) UpsertRepoAffinity(r *TeamRepoAffinity) error {
	return s.db.
		Where("session_id = ? AND member_id = ? AND repo_remote_url = ?",
			r.SessionID, r.MemberID, r.RepoRemoteURL).
		Assign(TeamRepoAffinity{
			RepoLocalPath:         r.RepoLocalPath,
			CurrentBranch:         r.CurrentBranch,
			HasUncommittedChanges: r.HasUncommittedChanges,
			LastSyncedAt:          r.LastSyncedAt,
		}).
		FirstOrCreate(r).Error
}

func (s *Store) ListReposByURL(sessionID, repoRemoteURL string) ([]TeamRepoAffinity, error) {
	var affinities []TeamRepoAffinity
	err := s.db.Where("session_id = ? AND repo_remote_url = ?", sessionID, repoRemoteURL).
		Order("last_synced_at DESC").Find(&affinities).Error
	return affinities, err
}

func (s *Store) ListReposByMember(sessionID, memberID string) ([]TeamRepoAffinity, error) {
	var affinities []TeamRepoAffinity
	err := s.db.Where("session_id = ? AND member_id = ?", sessionID, memberID).Find(&affinities).Error
	return affinities, err
}

// ─── Progress ──────────────────────────────────────────────────────────────

// TeammateProgress holds per-machine task counters for the progress view.
type TeammateProgress struct {
	MemberID      string  `json:"memberId"`
	MachineName   string  `json:"machineName"`
	CurrentTaskID *string `json:"currentTaskId,omitempty"`
	Completed     int64   `json:"completed"`
	Failed        int64   `json:"failed"`
	Running       int64   `json:"running"`
}

// SessionProgress aggregates task counts for a session, with per-member detail.
type SessionProgress struct {
	TotalTasks     int64              `json:"totalTasks"`
	CompletedTasks int64              `json:"completedTasks"`
	FailedTasks    int64              `json:"failedTasks"`
	RunningTasks   int64              `json:"runningTasks"`
	PendingTasks   int64              `json:"pendingTasks"`
	Teammates      []TeammateProgress `json:"teammates"`
}

func (s *Store) GetProgress(sessionID string) (*SessionProgress, error) {
	// ── Aggregate counts ──────────────────────────────────────────────────
	type statusCount struct {
		Status string
		Count  int64
	}
	var rows []statusCount
	if err := s.db.Model(&TeamTask{}).
		Select("status, count(*) as count").
		Where("session_id = ?", sessionID).
		Group("status").
		Scan(&rows).Error; err != nil {
		return nil, err
	}
	p := &SessionProgress{
		Teammates: []TeammateProgress{},
	}
	for _, r := range rows {
		p.TotalTasks += r.Count
		switch r.Status {
		case TaskStatusCompleted:
			p.CompletedTasks = r.Count
		case TaskStatusFailed:
			p.FailedTasks = r.Count
		case TaskStatusRunning:
			p.RunningTasks = r.Count
		case TaskStatusPending, TaskStatusAssigned:
			p.PendingTasks += r.Count
		}
	}

	// ── Per-teammate breakdown ─────────────────────────────────────────────
	type memberStatusCount struct {
		MemberID string
		Status   string
		Count    int64
	}
	var memberRows []memberStatusCount
	if err := s.db.Model(&TeamTask{}).
		Select("assigned_member_id as member_id, status, count(*) as count").
		Where("session_id = ? AND assigned_member_id IS NOT NULL", sessionID).
		Group("assigned_member_id, status").
		Scan(&memberRows).Error; err != nil {
		return nil, err
	}

	// Build per-member map
	type tmpProgress struct {
		Completed int64
		Failed    int64
		Running   int64
	}
	memberMap := make(map[string]*tmpProgress)
	for _, r := range memberRows {
		if memberMap[r.MemberID] == nil {
			memberMap[r.MemberID] = &tmpProgress{}
		}
		switch r.Status {
		case TaskStatusCompleted:
			memberMap[r.MemberID].Completed = r.Count
		case TaskStatusFailed:
			memberMap[r.MemberID].Failed = r.Count
		case TaskStatusRunning:
			memberMap[r.MemberID].Running = r.Count
		}
	}

	// Fetch member names and running task IDs
	if len(memberMap) > 0 {
		memberIDs := make([]string, 0, len(memberMap))
		for id := range memberMap {
			memberIDs = append(memberIDs, id)
		}
		var members []TeamSessionMember
		s.db.Where("id IN ?", memberIDs).Find(&members)
		memberNames := make(map[string]string, len(members))
		for _, m := range members {
			memberNames[m.ID] = m.MachineName
		}

		// Running task ID per member
		var runningTasks []TeamTask
		s.db.Select("id, assigned_member_id").
			Where("session_id = ? AND status = ? AND assigned_member_id IS NOT NULL",
				sessionID, TaskStatusRunning).
			Find(&runningTasks)
		runningTaskMap := make(map[string]string)
		for _, t := range runningTasks {
			if t.AssignedMemberID != nil {
				runningTaskMap[*t.AssignedMemberID] = t.ID
			}
		}

		for memberID, tmp := range memberMap {
			tp := TeammateProgress{
				MemberID:    memberID,
				MachineName: memberNames[memberID],
				Completed:   tmp.Completed,
				Failed:      tmp.Failed,
				Running:     tmp.Running,
			}
			if taskID, ok := runningTaskMap[memberID]; ok {
				tp.CurrentTaskID = &taskID
			}
			p.Teammates = append(p.Teammates, tp)
		}
	}

	return p, nil
}

// ─── Scheduling support ──────────────────────────────────────────────────

// ListAllRepoAffinities returns every repo affinity entry for a session.
func (s *Store) ListAllRepoAffinities(sessionID string) ([]TeamRepoAffinity, error) {
	var affinities []TeamRepoAffinity
	err := s.db.Where("session_id = ?", sessionID).Find(&affinities).Error
	return affinities, err
}

// GetMemberLoadInfo returns per-member load summary for scheduling.
func (s *Store) GetMemberLoadInfo(sessionID string) (map[string]MemberLoadInfo, error) {
	type memberStatusCount struct {
		MemberID string
		Status   string
		Count    int64
	}
	var rows []memberStatusCount
	if err := s.db.Model(&TeamTask{}).
		Select("assigned_member_id as member_id, status, count(*) as count").
		Where("session_id = ? AND assigned_member_id IS NOT NULL", sessionID).
		Group("assigned_member_id, status").
		Scan(&rows).Error; err != nil {
		return nil, err
	}

	result := make(map[string]MemberLoadInfo)
	for _, r := range rows {
		load := result[r.MemberID]
		switch r.Status {
		case TaskStatusRunning, TaskStatusClaimed:
			load.RunningCount += int(r.Count)
		case TaskStatusAssigned:
			load.AssignedCount += int(r.Count)
		case TaskStatusCompleted:
			load.CompletedCount += r.Count
		case TaskStatusFailed:
			load.FailedCount += r.Count
		}
		result[r.MemberID] = load
	}

	// Build ActiveRepoURLs per member from running/assigned tasks
	var activeTasks []TeamTask
	s.db.Select("id, assigned_member_id, repo_affinity").
		Where("session_id = ? AND status IN ? AND assigned_member_id IS NOT NULL",
			sessionID, []string{TaskStatusRunning, TaskStatusAssigned, TaskStatusClaimed}).
		Find(&activeTasks)
	for _, t := range activeTasks {
		if t.AssignedMemberID == nil {
			continue
		}
		load := result[*t.AssignedMemberID]
		if load.ActiveRepoURLs == nil {
			load.ActiveRepoURLs = make(map[string]bool)
		}
		for _, url := range t.RepoAffinity {
			load.ActiveRepoURLs[url] = true
		}
		result[*t.AssignedMemberID] = load
	}

	return result, nil
}

// GetRunningAssignedTasks returns all tasks that are currently running,
// assigned, or claimed.
func (s *Store) GetRunningAssignedTasks(sessionID string) ([]TeamTask, error) {
	var tasks []TeamTask
	err := s.db.Where("session_id = ? AND status IN ?",
		sessionID, []string{TaskStatusRunning, TaskStatusAssigned, TaskStatusClaimed}).
		Find(&tasks).Error
	return tasks, err
}

// UpdateMemberCapabilities persists a leader candidate's capability data.
func (s *Store) UpdateMemberCapabilities(memberID string, caps LeaderCapability) error {
	updates := map[string]any{
		"cpu_idle_percent":       caps.CPUIdlePercent,
		"memory_free_mb":        caps.MemoryFreeMB,
		"rtt_ms":                caps.RTTMs,
		"heartbeat_success_rate": caps.HeartbeatSuccessRate,
	}
	if len(caps.RepoURLs) > 0 {
		updates["reported_repo_urls"] = caps.RepoURLs
	}
	err := s.db.Model(&TeamSessionMember{}).Where("id = ?", memberID).Updates(updates).Error
	if err == nil {
		return nil
	}
	// Compatibility fallback for databases that haven't applied the migration yet.
	// Missing capability columns should not break join/election flow.
	if strings.Contains(err.Error(), "SQLSTATE 42703") || strings.Contains(err.Error(), "does not exist") {
		return nil
	}
	return err
}

// GetSessionTargetRepos returns the distinct set of repo URLs referenced by
// all tasks in the session (used for leader scoring context).
func (s *Store) GetSessionTargetRepos(sessionID string) ([]string, error) {
	var tasks []TeamTask
	if err := s.db.Select("repo_affinity").
		Where("session_id = ? AND repo_affinity IS NOT NULL", sessionID).
		Find(&tasks).Error; err != nil {
		return nil, err
	}
	seen := make(map[string]struct{})
	for _, t := range tasks {
		for _, url := range t.RepoAffinity {
			seen[url] = struct{}{}
		}
	}
	result := make([]string, 0, len(seen))
	for url := range seen {
		result = append(result, url)
	}
	return result, nil
}
