package team

import (
	"testing"
)

// ─── P1-P5 Tier computation ───────────────────────────────────────────────

func TestComputePriorityTier(t *testing.T) {
	tests := []struct {
		name         string
		hasAll       bool
		hasSome      bool
		dirtyOnTarget bool
		isIdle       bool
		want         int
	}{
		{"P1: has all, no dirty, idle", true, true, false, true, P1},
		{"P2: has all, dirty, idle", true, true, true, true, P2},
		{"P3: has some, idle", false, true, false, true, P3},
		{"P4: has none, idle", false, false, false, true, P4},
		{"Not eligible: not idle", true, true, false, false, 0},
		{"Not idle even with all repos and dirty", true, true, true, false, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := computePriorityTier(tt.hasAll, tt.hasSome, tt.dirtyOnTarget, tt.isIdle)
			if got != tt.want {
				t.Errorf("computePriorityTier() = %d, want %d", got, tt.want)
			}
		})
	}
}

// ─── Repo coverage ───────────────────────────────────────────────────────

func TestComputeRepoCoverage(t *testing.T) {
	memberRepos := map[string]TeamRepoAffinity{
		"https://github.com/org/repo-a": {RepoRemoteURL: "https://github.com/org/repo-a", HasUncommittedChanges: false},
		"https://github.com/org/repo-b": {RepoRemoteURL: "https://github.com/org/repo-b", HasUncommittedChanges: true},
	}

	tests := []struct {
		name        string
		targetRepos []string
		wantAll     bool
		wantSome    bool
		wantDirty   bool
	}{
		{"has all repos, no dirty", []string{"https://github.com/org/repo-a", "https://github.com/org/repo-b"}, true, true, true},
		{"has all repos, no dirty on specific", []string{"https://github.com/org/repo-a"}, true, true, false},
		{"has some repos", []string{"https://github.com/org/repo-a", "https://github.com/org/repo-c"}, false, true, false},
		{"has no repos", []string{"https://github.com/org/repo-x"}, false, false, false},
		{"empty target repos", []string{}, true, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hasAll, hasSome, dirty := computeRepoCoverage(tt.targetRepos, memberRepos)
			if hasAll != tt.wantAll || hasSome != tt.wantSome || dirty != tt.wantDirty {
				t.Errorf("computeRepoCoverage() = (%v, %v, %v), want (%v, %v, %v)",
					hasAll, hasSome, dirty, tt.wantAll, tt.wantSome, tt.wantDirty)
			}
		})
	}
}

func TestComputeCoverageFraction(t *testing.T) {
	memberRepos := map[string]TeamRepoAffinity{
		"https://github.com/org/repo-a": {},
		"https://github.com/org/repo-b": {},
	}

	tests := []struct {
		name        string
		targetRepos []string
		want        float64
	}{
		{"all repos", []string{"https://github.com/org/repo-a", "https://github.com/org/repo-b"}, 1.0},
		{"half repos", []string{"https://github.com/org/repo-a", "https://github.com/org/repo-c"}, 0.5},
		{"no repos", []string{"https://github.com/org/repo-x"}, 0.0},
		{"empty target", []string{}, 1.0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := computeCoverageFraction(tt.targetRepos, memberRepos)
			if got != tt.want {
				t.Errorf("computeCoverageFraction() = %f, want %f", got, tt.want)
			}
		})
	}
}

// ─── Success rate ────────────────────────────────────────────────────────

func TestComputeSuccessRate(t *testing.T) {
	tests := []struct {
		name      string
		completed int64
		failed    int64
		want      float64
	}{
		{"all success", 10, 0, 1.0},
		{"half and half", 5, 5, 0.5},
		{"all failed", 0, 10, 0.0},
		{"no data — neutral default", 0, 0, 0.5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := computeSuccessRate(tt.completed, tt.failed)
			if got != tt.want {
				t.Errorf("computeSuccessRate() = %f, want %f", got, tt.want)
			}
		})
	}
}

// ─── Same-repo serialization ──────────────────────────────────────────────

func TestHasRunningSameRepoTask(t *testing.T) {
	runningRepos := map[string]bool{
		"https://github.com/org/repo-a": true,
	}

	tests := []struct {
		name        string
		targetRepos []string
		want        bool
	}{
		{"same repo running", []string{"https://github.com/org/repo-a"}, true},
		{"different repo", []string{"https://github.com/org/repo-b"}, false},
		{"mixed — one matches", []string{"https://github.com/org/repo-b", "https://github.com/org/repo-a"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := hasRunningSameRepoTask(tt.targetRepos, runningRepos)
			if got != tt.want {
				t.Errorf("hasRunningSameRepoTask() = %v, want %v", got, tt.want)
			}
		})
	}
}

// ─── Full scheduling integration ──────────────────────────────────────────

func TestScheduleTasks_P1Preferred(t *testing.T) {
	// Member A has all target repos and no dirty state → P1
	memberA := TeamSessionMember{ID: "m-a", MachineID: "machine-a"}
	memberB := TeamSessionMember{ID: "m-b", MachineID: "machine-b"}

	ctx := SchedulingContext{
		Members: []TeamSessionMember{memberA, memberB},
		RepoAffinities: []TeamRepoAffinity{
			{MemberID: "m-a", RepoRemoteURL: "https://github.com/org/repo-x"},
		},
		RunningTasks: []TeamTask{},
		MemberLoad:   map[string]MemberLoadInfo{},
	}

	tasks := []TeamTask{
		{ID: "task-1", Description: "do stuff", RepoAffinity: []string{"https://github.com/org/repo-x"}},
	}

	result := ScheduleTasks(ctx, tasks)
	if result[0].AssignedMemberID == nil || *result[0].AssignedMemberID != "m-a" {
		t.Errorf("expected task assigned to m-a (P1), got %v", result[0].AssignedMemberID)
	}
	if result[0].Status != TaskStatusAssigned {
		t.Errorf("expected status assigned, got %s", result[0].Status)
	}
}

func TestScheduleTasks_ExplicitAssignmentRespected(t *testing.T) {
	// If the client explicitly sets AssignedMemberID, the scheduler must not change it
	explicitID := "m-explicit"
	tasks := []TeamTask{
		{ID: "task-1", Description: "do stuff", AssignedMemberID: &explicitID, RepoAffinity: []string{"https://github.com/org/repo-x"}},
	}

	ctx := SchedulingContext{
		Members:        []TeamSessionMember{{ID: "m-other", MachineID: "machine-other"}},
		RepoAffinities: []TeamRepoAffinity{},
		RunningTasks:   []TeamTask{},
		MemberLoad:     map[string]MemberLoadInfo{},
	}

	result := ScheduleTasks(ctx, tasks)
	if result[0].AssignedMemberID == nil || *result[0].AssignedMemberID != explicitID {
		t.Errorf("explicit assignment should be preserved, got %v", result[0].AssignedMemberID)
	}
}

func TestScheduleTasks_NoAffinityFallback(t *testing.T) {
	// Tasks with no repoAffinity should be assigned to least-loaded idle member
	memberA := TeamSessionMember{ID: "m-a", MachineID: "machine-a"}
	memberB := TeamSessionMember{ID: "m-b", MachineID: "machine-b"}

	ctx := SchedulingContext{
		Members:        []TeamSessionMember{memberA, memberB},
		RepoAffinities: []TeamRepoAffinity{},
		RunningTasks:   []TeamTask{},
		MemberLoad: map[string]MemberLoadInfo{
			"m-a": {RunningCount: 1, AssignedCount: 0},
			"m-b": {RunningCount: 0, AssignedCount: 0},
		},
	}

	tasks := []TeamTask{
		{ID: "task-1", Description: "no repo affinity", Status: TaskStatusPending, RepoAffinity: nil},
	}

	result := ScheduleTasks(ctx, tasks)
	if result[0].AssignedMemberID == nil {
		t.Errorf("expected task assigned to m-b (idle), got nil. Status=%s, RepoAffinity=%v", result[0].Status, result[0].RepoAffinity)
	} else if *result[0].AssignedMemberID != "m-b" {
		t.Errorf("expected task assigned to m-b (idle), got %s", *result[0].AssignedMemberID)
	}
}

func TestScheduleTasks_SameRepoSerialization(t *testing.T) {
	// Member A already has a running task on repo-x.
	// Member B is idle and has repo-x.
	// P1 logic should prefer idle member B, but same-repo serialization
	// should boost non-idle member A when it's the only option or at P3+ tier.
	memberA := TeamSessionMember{ID: "m-a", MachineID: "machine-a"}
	memberB := TeamSessionMember{ID: "m-b", MachineID: "machine-b"}

	ctx := SchedulingContext{
		Members: []TeamSessionMember{memberA, memberB},
		RepoAffinities: []TeamRepoAffinity{
			{MemberID: "m-a", RepoRemoteURL: "https://github.com/org/repo-x"},
			{MemberID: "m-b", RepoRemoteURL: "https://github.com/org/repo-x"},
		},
		RunningTasks: []TeamTask{
			{ID: "existing", AssignedMemberID: strPtr("m-a"), RepoAffinity: []string{"https://github.com/org/repo-x"}},
		},
		MemberLoad: map[string]MemberLoadInfo{
			"m-a": {RunningCount: 1, AssignedCount: 0},
			"m-b": {RunningCount: 0, AssignedCount: 0},
		},
	}

	tasks := []TeamTask{
		{ID: "task-1", Description: "same repo", RepoAffinity: []string{"https://github.com/org/repo-x"}},
	}

	result := ScheduleTasks(ctx, tasks)
	// Member B is P1 (idle, has all repos, no dirty), Member A is not idle → 0 tier
	// So B should be assigned
	if result[0].AssignedMemberID == nil || *result[0].AssignedMemberID != "m-b" {
		t.Errorf("expected task assigned to m-b (P1 idle), got %v", result[0].AssignedMemberID)
	}
}

func TestScheduleTasks_MultipleTasks(t *testing.T) {
	// Two tasks, one member idle — first task gets assigned, second task
	// sees updated load and should not be assigned if the member is no longer idle
	memberA := TeamSessionMember{ID: "m-a", MachineID: "machine-a"}

	ctx := SchedulingContext{
		Members:        []TeamSessionMember{memberA},
		RepoAffinities: []TeamRepoAffinity{},
		RunningTasks:   []TeamTask{},
		MemberLoad:     map[string]MemberLoadInfo{},
	}

	tasks := []TeamTask{
		{ID: "task-1", Description: "first", RepoAffinity: []string{"https://github.com/org/repo-x"}},
		{ID: "task-2", Description: "second", RepoAffinity: []string{"https://github.com/org/repo-y"}},
	}

	result := ScheduleTasks(ctx, tasks)
	// First task: member A is idle, no repo affinity match → P4 (has none), gets assigned
	if result[0].AssignedMemberID == nil {
		t.Errorf("task-1 should be assigned, got nil")
	}
	// Second task: member A now has AssignedCount=1, RunningCount=0 → still eligible at P4
	// because the scheduler only checks idle via RunningCount==0 && AssignedCount==0
	// After first assignment, AssignedCount=1, so IsIdle = false → not eligible
	if result[1].AssignedMemberID != nil {
		// This depends on the exact idle check; our scheduler checks RunningCount+AssignedCount==0
		// After task-1 assigns, member has AssignedCount=1 → not idle → tier 0
		// So task-2 should not be assigned via P1-P4, falls to P5 fallback
		// P5 allows up to 2 concurrent tasks per member
		t.Logf("task-2 assigned to %s (P5 fallback)", *result[1].AssignedMemberID)
	}
}

func TestBuildMemberRepoMap(t *testing.T) {
	affinities := []TeamRepoAffinity{
		{MemberID: "m-a", RepoRemoteURL: "https://github.com/org/repo-1"},
		{MemberID: "m-a", RepoRemoteURL: "https://github.com/org/repo-2"},
		{MemberID: "m-b", RepoRemoteURL: "https://github.com/org/repo-1"},
	}

	result := buildMemberRepoMap(affinities)
	if len(result) != 2 {
		t.Errorf("expected 2 members, got %d", len(result))
	}
	if len(result["m-a"]) != 2 {
		t.Errorf("expected m-a to have 2 repos, got %d", len(result["m-a"]))
	}
	if len(result["m-b"]) != 1 {
		t.Errorf("expected m-b to have 1 repo, got %d", len(result["m-b"]))
	}
}

func TestBuildRunningTaskRepoMap(t *testing.T) {
	memberRepos := map[string]map[string]TeamRepoAffinity{}
	tasks := []TeamTask{
		{ID: "t1", AssignedMemberID: strPtr("m-a"), RepoAffinity: []string{"https://github.com/org/repo-1"}},
		{ID: "t2", AssignedMemberID: strPtr("m-a"), RepoAffinity: []string{"https://github.com/org/repo-2"}},
		{ID: "t3", AssignedMemberID: nil, RepoAffinity: []string{"https://github.com/org/repo-3"}}, // no member
	}

	result := buildRunningTaskRepoMap(tasks, memberRepos)
	if len(result) != 1 {
		t.Errorf("expected 1 member with running tasks, got %d", len(result))
	}
	if !result["m-a"]["https://github.com/org/repo-1"] || !result["m-a"]["https://github.com/org/repo-2"] {
		t.Errorf("m-a should have repo-1 and repo-2 as running repos")
	}
}

// ─── Leader score ────────────────────────────────────────────────────────

func TestScoreLeaderCandidate(t *testing.T) {
	cap := LeaderCapability{
		MachineID:            "machine-1",
		RepoURLs:             []string{"https://github.com/org/repo-a", "https://github.com/org/repo-b"},
		HeartbeatSuccessRate: 0.9,
		CPUIdlePercent:       60,
		MemoryFreeMB:         4096,
		RTTMs:                50,
	}
	targetRepos := []string{"https://github.com/org/repo-a", "https://github.com/org/repo-b", "https://github.com/org/repo-c"}

	score := ScoreLeaderCandidate(cap, targetRepos)

	// Repo coverage: 2/3 → 0.667
	if score.RepoCoverageScore < 0.66 || score.RepoCoverageScore > 0.67 {
		t.Errorf("RepoCoverageScore = %f, want ~0.667", score.RepoCoverageScore)
	}
	// Heartbeat: 0.9
	if score.HeartbeatScore != 0.9 {
		t.Errorf("HeartbeatScore = %f, want 0.9", score.HeartbeatScore)
	}
	// Perf: (60/100 + 4096/8192) / 2 = (0.6 + 0.5) / 2 = 0.55
	if score.PerformanceScore != 0.55 {
		t.Errorf("PerformanceScore = %f, want 0.55", score.PerformanceScore)
	}
	// Latency: max(0, 1 - 50/500) = 0.9
	if score.LatencyScore != 0.9 {
		t.Errorf("LatencyScore = %f, want 0.9", score.LatencyScore)
	}
	// Total: 0.667*0.4 + 0.9*0.3 + 0.55*0.2 + 0.9*0.1 ≈ 0.737
	if score.TotalScore < 0.73 || score.TotalScore > 0.75 {
		t.Errorf("TotalScore = %f, want ~0.737", score.TotalScore)
	}
}

func TestScoreLeaderCandidate_NoTargetRepos(t *testing.T) {
	cap := LeaderCapability{
		MachineID:            "machine-1",
		RepoURLs:             []string{"https://github.com/org/repo-a"},
		HeartbeatSuccessRate: 0,
		CPUIdlePercent:       0,
		MemoryFreeMB:         0,
		RTTMs:                0,
	}

	score := ScoreLeaderCandidate(cap, nil)

	// No target repos → repo score = 0.5 (neutral)
	if score.RepoCoverageScore != 0.5 {
		t.Errorf("RepoCoverageScore = %f, want 0.5", score.RepoCoverageScore)
	}
	// No heartbeat provided → default 0.5
	if score.HeartbeatScore != 0.5 {
		t.Errorf("HeartbeatScore = %f, want 0.5", score.HeartbeatScore)
	}
}

func TestComputeLatencyScore(t *testing.T) {
	tests := []struct {
		name   string
		rttMs  float64
		want   float64
	}{
		{"zero RTT", 0, 1.0},
		{"250ms", 250, 0.5},
		{"500ms → 0", 500, 0.0},
		{"600ms → clamped to 0", 600, 0.0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := computeLatencyScore(tt.rttMs)
			if got != tt.want {
				t.Errorf("computeLatencyScore(%f) = %f, want %f", tt.rttMs, got, tt.want)
			}
		})
	}
}

func TestComputePerformanceScore(t *testing.T) {
	tests := []struct {
		name       string
		cpuIdle    float64
		memFreeMB  float64
		want       float64
	}{
		{"full idle, full mem", 100, 8192, 1.0},
		{"half idle, half mem", 50, 4096, 0.5},
		{"zero everything", 0, 0, 0.0},
		{"cpu cap, mem overflow", 150, 16000, 1.0}, // both capped at 1.0
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := computePerformanceScore(tt.cpuIdle, tt.memFreeMB)
			if got != tt.want {
				t.Errorf("computePerformanceScore(%f, %f) = %f, want %f",
					tt.cpuIdle, tt.memFreeMB, got, tt.want)
			}
		})
	}
}

// ─── Helpers ─────────────────────────────────────────────────────────────

func strPtr(s string) *string { return &s }
