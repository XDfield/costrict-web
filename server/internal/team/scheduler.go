package team

import (
	"math"
	"sort"
)

// ─── Priority tier constants ────────────────────────────────────────────

const (
	P1 = 1 // Has ALL target repos + no uncommitted changes + idle
	P2 = 2 // Has ALL target repos + has uncommitted changes + idle (or same-repo serialization)
	P3 = 3 // Has SOME target repos (missing need clone) + idle
	P4 = 4 // Has NO target repos (all need clone) + idle
	P5 = 5 // Any idle teammate (fallback for tasks with no repoAffinity)
)

// ─── Scheduling types ───────────────────────────────────────────────────

// MemberLoadInfo is a summary of a member's current work state.
type MemberLoadInfo struct {
	RunningCount   int
	AssignedCount  int
	CompletedCount int64
	FailedCount    int64
	ActiveRepoURLs map[string]bool // repos with currently running/assigned tasks
}

// SchedulingContext holds all data needed for one scheduling pass.
type SchedulingContext struct {
	Members        []TeamSessionMember
	RepoAffinities []TeamRepoAffinity
	RunningTasks   []TeamTask
	MemberLoad     map[string]MemberLoadInfo // memberID → load summary
}

// CandidateScore holds the computed priority and sub-scores for a member.
type CandidateScore struct {
	MemberID       string
	MachineID      string
	PriorityTier   int     // P1=1 … P5=5
	RepoCoverage   float64 // fraction of target repos this member has
	QueueLength    int     // current assigned/running task count
	SuccessRate    float64 // completed / (completed + failed), 0.5 default
	DirtyRepo      bool    // has uncommitted changes on target repos
	IsIdle         bool    // not currently running any task
	CompositeScore float64 // weighted secondary sort score
}

// ─── Core scheduling function ───────────────────────────────────────────

// ScheduleTasks assigns each unassigned task to the best candidate member
// using the P1-P5 repo affinity scheduling algorithm. Tasks that already
// have an AssignedMemberID are left untouched (respecting explicit assignment).
func ScheduleTasks(ctx SchedulingContext, tasks []TeamTask) []TeamTask {
	if len(ctx.Members) == 0 || len(tasks) == 0 {
		return tasks
	}

	// 1. Pre-compute per-member data structures
	memberRepos := buildMemberRepoMap(ctx.RepoAffinities)
	memberLoad := ctx.MemberLoad
	if memberLoad == nil {
		memberLoad = make(map[string]MemberLoadInfo)
	}
	runningByMemberRepo := buildRunningTaskRepoMap(ctx.RunningTasks, memberRepos)

	// 2. Schedule tasks with repo affinity
	for i, task := range tasks {
		if task.AssignedMemberID != nil && *task.AssignedMemberID != "" {
			continue // client explicitly assigned; respect it
		}
		if len(task.RepoAffinity) == 0 {
			continue // handled in step 3 (P5 fallback)
		}

		candidates := scoreCandidates(ctx, task, memberRepos, memberLoad, runningByMemberRepo)
		if len(candidates) == 0 {
			continue
		}

		// Sort: primary by PriorityTier ASC, secondary by CompositeScore DESC
		sort.Slice(candidates, func(a, b int) bool {
			if candidates[a].PriorityTier != candidates[b].PriorityTier {
				return candidates[a].PriorityTier < candidates[b].PriorityTier
			}
			return candidates[a].CompositeScore > candidates[b].CompositeScore
		})

		assignedID := candidates[0].MemberID
		tasks[i].AssignedMemberID = &assignedID
		tasks[i].Status = TaskStatusAssigned

		// Update in-memory load so subsequent tasks see the updated queue
		load := memberLoad[assignedID]
		load.AssignedCount++
		for _, repoURL := range task.RepoAffinity {
			if load.ActiveRepoURLs == nil {
				load.ActiveRepoURLs = make(map[string]bool)
			}
			load.ActiveRepoURLs[repoURL] = true
		}
		memberLoad[assignedID] = load

		// Update runningByMemberRepo for serialization
		for _, repoURL := range task.RepoAffinity {
			if runningByMemberRepo[assignedID] == nil {
				runningByMemberRepo[assignedID] = make(map[string]bool)
			}
			runningByMemberRepo[assignedID][repoURL] = true
		}
	}

	// 3. Handle tasks with no repoAffinity (P5 fallback)
	for i, task := range tasks {
		if (task.AssignedMemberID == nil || *task.AssignedMemberID == "") &&
			task.Status == TaskStatusPending {
			assignToLeastLoadedIdleMember(ctx, &tasks[i], memberLoad)
		}
	}

	return tasks
}

// ─── Candidate scoring ──────────────────────────────────────────────────

// scoreCandidates evaluates all online members against a task and returns
// their P1-P5 tier scores.
func scoreCandidates(
	ctx SchedulingContext,
	task TeamTask,
	memberRepos map[string]map[string]TeamRepoAffinity, // memberID → repoURL → affinity
	memberLoad map[string]MemberLoadInfo,
	runningByMemberRepo map[string]map[string]bool, // memberID → set of repoURLs
) []CandidateScore {
	var candidates []CandidateScore

	for _, member := range ctx.Members {
		load := memberLoad[member.ID]
		isIdle := load.RunningCount == 0 && load.AssignedCount == 0

		repos := memberRepos[member.ID] // repoURL → affinity entry
		hasAll, hasSome, dirtyOnTarget := computeRepoCoverage(task.RepoAffinity, repos)

		tier := computePriorityTier(hasAll, hasSome, dirtyOnTarget, isIdle)
		if tier == 0 {
			continue // not eligible
		}

		// Same-repo serialization: if this member is already running a task on
		// one of the target repos, boost their tier to serialize writes.
		sameRepoRunning := hasRunningSameRepoTask(task.RepoAffinity, runningByMemberRepo[member.ID])
		effectiveTier := tier
		if sameRepoRunning && tier >= P3 {
			effectiveTier = P2 // serialize: prefer the member already working on this repo
		}

		coverage := computeCoverageFraction(task.RepoAffinity, repos)
		successRate := computeSuccessRate(load.CompletedCount, load.FailedCount)
		queueLen := load.RunningCount + load.AssignedCount

		// Composite secondary score (higher is better)
		compositeScore :=
			(1.0-float64(queueLen)/10.0)*0.4 +
				successRate*0.4 +
				coverage*0.2

		candidates = append(candidates, CandidateScore{
			MemberID:       member.ID,
			MachineID:      member.MachineID,
			PriorityTier:   effectiveTier,
			RepoCoverage:   coverage,
			QueueLength:    queueLen,
			SuccessRate:    successRate,
			DirtyRepo:      dirtyOnTarget,
			IsIdle:         isIdle,
			CompositeScore: compositeScore,
		})
	}
	return candidates
}

// ─── Tier computation ───────────────────────────────────────────────────

// computePriorityTier determines the P1-P5 tier for a member.
// Returns 0 if the member is not eligible (not idle).
func computePriorityTier(hasAll, hasSome, dirtyOnTarget, isIdle bool) int {
	if !isIdle {
		// Only idle teammates are eligible for new assignment.
		// (Non-idle members can still be boosted via same-repo serialization
		// if they have running tasks on the target repo — handled in scoreCandidates.)
		return 0
	}
	if hasAll && !dirtyOnTarget {
		return P1
	}
	if hasAll && dirtyOnTarget {
		return P2
	}
	if hasSome && !hasAll {
		return P3
	}
	// hasSome == false means hasNone
	return P4
}

// ─── Helper functions ───────────────────────────────────────────────────

// computeRepoCoverage checks if the member has all, some, or none of the target repos.
// Also returns whether the member has uncommitted changes on any target repo.
func computeRepoCoverage(targetRepos []string, memberRepos map[string]TeamRepoAffinity) (hasAll, hasSome, dirtyOnTarget bool) {
	if len(targetRepos) == 0 {
		return true, true, false // no affinity requirement → trivially satisfied
	}

	matched := 0
	for _, url := range targetRepos {
		affinity, ok := memberRepos[url]
		if ok {
			matched++
			if affinity.HasUncommittedChanges {
				dirtyOnTarget = true
			}
		}
	}

	hasSome = matched > 0
	hasAll = matched == len(targetRepos)
	return
}

// computeCoverageFraction returns 0.0-1.0 for what fraction of target repos
// the member has.
func computeCoverageFraction(targetRepos []string, memberRepos map[string]TeamRepoAffinity) float64 {
	if len(targetRepos) == 0 {
		return 1.0
	}
	matched := 0
	for _, url := range targetRepos {
		if _, ok := memberRepos[url]; ok {
			matched++
		}
	}
	return float64(matched) / float64(len(targetRepos))
}

// computeSuccessRate returns completed/(completed+failed), defaulting to 0.5
// when no data is available.
func computeSuccessRate(completed, failed int64) float64 {
	total := completed + failed
	if total == 0 {
		return 0.5 // neutral default
	}
	return float64(completed) / float64(total)
}

// hasRunningSameRepoTask checks if the member is already running a task
// touching one of the target repo URLs.
func hasRunningSameRepoTask(targetRepos []string, runningRepos map[string]bool) bool {
	for _, url := range targetRepos {
		if runningRepos[url] {
			return true
		}
	}
	return false
}

// buildMemberRepoMap builds memberID → repoURL → TeamRepoAffinity from flat list.
func buildMemberRepoMap(affinities []TeamRepoAffinity) map[string]map[string]TeamRepoAffinity {
	result := make(map[string]map[string]TeamRepoAffinity)
	for _, a := range affinities {
		if result[a.MemberID] == nil {
			result[a.MemberID] = make(map[string]TeamRepoAffinity)
		}
		result[a.MemberID][a.RepoRemoteURL] = a
	}
	return result
}

// buildRunningTaskRepoMap builds memberID → set of repoURLs from currently
// running/assigned tasks.
func buildRunningTaskRepoMap(running []TeamTask, memberRepos map[string]map[string]TeamRepoAffinity) map[string]map[string]bool {
	result := make(map[string]map[string]bool)
	for _, t := range running {
		if t.AssignedMemberID == nil {
			continue
		}
		memberID := *t.AssignedMemberID
		if result[memberID] == nil {
			result[memberID] = make(map[string]bool)
		}
		for _, url := range t.RepoAffinity {
			result[memberID][url] = true
		}
	}
	return result
}

// assignToLeastLoadedIdleMember assigns a task with no repo affinity to the
// idle member with the shortest queue.
func assignToLeastLoadedIdleMember(ctx SchedulingContext, task *TeamTask, memberLoad map[string]MemberLoadInfo) {
	var bestMemberID string
	bestQueueLen := math.MaxInt

	for _, member := range ctx.Members {
		load := memberLoad[member.ID]
		queueLen := load.RunningCount + load.AssignedCount

		// Only consider members with relatively low load
		// (allow up to 2 concurrent tasks per member for no-affinity tasks)
		if queueLen >= 2 {
			continue
		}

		if queueLen < bestQueueLen {
			bestQueueLen = queueLen
			bestMemberID = member.ID
		}
	}

	if bestMemberID != "" {
		task.AssignedMemberID = &bestMemberID
		task.Status = TaskStatusAssigned
		load := memberLoad[bestMemberID]
		load.AssignedCount++
		memberLoad[bestMemberID] = load
	}
}
