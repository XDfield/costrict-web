package team

// ─── Leader candidate scoring ─────────────────────────────────────────────

// LeaderCapability represents a candidate's self-reported capabilities
// sent during leader election.
type LeaderCapability struct {
	MachineID            string
	RepoURLs             []string
	HeartbeatSuccessRate float64 // 0.0–1.0
	CPUIdlePercent       float64 // 0–100
	MemoryFreeMB         float64
	RTTMs                float64 // round-trip latency in ms
}

// LeaderScore is the computed scoring breakdown for a leader candidate.
type LeaderScore struct {
	MachineID          string  `json:"machineId"`
	TotalScore         float64 `json:"totalScore"`
	RepoCoverageScore  float64 `json:"repoCoverageScore"`
	HeartbeatScore     float64 `json:"heartbeatScore"`
	PerformanceScore   float64 `json:"performanceScore"`
	LatencyScore       float64 `json:"latencyScore"`
}

// Weight constants for leader scoring.
const (
	LeaderWeightRepo      = 0.4
	LeaderWeightHeartbeat = 0.3
	LeaderWeightPerf      = 0.2
	LeaderWeightLatency   = 0.1
)

// ScoreLeaderCandidate evaluates a leader candidate and returns the weighted
// score breakdown. sessionTargetRepos is the union of all repo URLs referenced
// by tasks in the session; if empty the repo score defaults to neutral (0.5).
func ScoreLeaderCandidate(cap LeaderCapability, sessionTargetRepos []string) LeaderScore {
	repoScore := computeRepoCoverageScore(cap.RepoURLs, sessionTargetRepos)
	heartbeatScore := cap.HeartbeatSuccessRate
	if heartbeatScore == 0 {
		heartbeatScore = 0.5 // neutral default when not provided
	}
	perfScore := computePerformanceScore(cap.CPUIdlePercent, cap.MemoryFreeMB)
	latencyScore := computeLatencyScore(cap.RTTMs)

	total := repoScore*LeaderWeightRepo +
		heartbeatScore*LeaderWeightHeartbeat +
		perfScore*LeaderWeightPerf +
		latencyScore*LeaderWeightLatency

	return LeaderScore{
		MachineID:          cap.MachineID,
		TotalScore:         total,
		RepoCoverageScore:  repoScore,
		HeartbeatScore:     heartbeatScore,
		PerformanceScore:   perfScore,
		LatencyScore:       latencyScore,
	}
}

// computeRepoCoverageScore returns the fraction of session target repos that
// the candidate has locally. Returns 0.5 (neutral) when there are no target repos.
func computeRepoCoverageScore(candidateRepos, targetRepos []string) float64 {
	if len(targetRepos) == 0 {
		return 0.5 // no repo requirement → neutral
	}
	candidateSet := make(map[string]struct{}, len(candidateRepos))
	for _, url := range candidateRepos {
		candidateSet[url] = struct{}{}
	}
	matched := 0
	for _, url := range targetRepos {
		if _, ok := candidateSet[url]; ok {
			matched++
		}
	}
	return float64(matched) / float64(len(targetRepos))
}

// computePerformanceScore combines CPU idle % and free memory into a 0–1 score.
// cpuIdle is 0–100, memFreeMB is absolute. The memory component caps at 8 GB.
func computePerformanceScore(cpuIdle, memFreeMB float64) float64 {
	cpuComponent := cpuIdle / 100.0
	if cpuComponent > 1.0 {
		cpuComponent = 1.0
	}
	memComponent := memFreeMB / 8192.0
	if memComponent > 1.0 {
		memComponent = 1.0
	}
	return (cpuComponent + memComponent) / 2.0
}

// computeLatencyScore converts RTT in ms to a 0–1 score where lower latency
// is better. Uses max(0, 1 - rtt/500) so 0 ms → 1.0 and 500+ ms → 0.0.
func computeLatencyScore(rttMs float64) float64 {
	score := 1.0 - rttMs/500.0
	if score < 0 {
		return 0
	}
	if score > 1 {
		return 1
	}
	return score
}
