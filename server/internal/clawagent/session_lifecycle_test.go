package clawagent

import (
	"testing"
	"time"
)

func TestDailyResetAt(t *testing.T) {
	// March 17, 2026 at 10:00 AM
	now := time.Date(2026, 3, 17, 10, 0, 0, 0, time.UTC)

	reset := dailyResetAt(now, 4)
	expected := time.Date(2026, 3, 17, 4, 0, 0, 0, time.UTC)
	if !reset.Equal(expected) {
		t.Errorf("dailyResetAt(10:00, 4) = %v, want %v", reset, expected)
	}

	// Before reset hour (3 AM)
	beforeReset := time.Date(2026, 3, 17, 3, 0, 0, 0, time.UTC)
	reset = dailyResetAt(beforeReset, 4)
	expected = time.Date(2026, 3, 16, 4, 0, 0, 0, time.UTC)
	if !reset.Equal(expected) {
		t.Errorf("dailyResetAt(3:00, 4) = %v, want %v (previous day)", reset, expected)
	}

	// Exactly at reset hour
	atReset := time.Date(2026, 3, 17, 4, 0, 0, 0, time.UTC)
	reset = dailyResetAt(atReset, 4)
	expected = time.Date(2026, 3, 17, 4, 0, 0, 0, time.UTC)
	if !reset.Equal(expected) {
		t.Errorf("dailyResetAt(4:00, 4) = %v, want %v", reset, expected)
	}
}

func TestIsStale_Direct(t *testing.T) {
	cfg := testClawAgentCfg()

	// Session last active at 10:00, current time 10:30 (same day, after 4AM reset)
	meta := &SessionMeta{
		ResetType:     "direct",
		LastMessageAt: time.Date(2026, 3, 17, 10, 0, 0, 0, time.UTC),
	}
	now := time.Date(2026, 3, 17, 10, 30, 0, 0, time.UTC)

	rt := &ClawAgentRuntime{agentCfg: cfg}
	stale := rt.isStale(meta, now)
	if stale {
		t.Error("direct session should not be stale within same day after reset hour")
	}

	// Session from previous day
	meta.LastMessageAt = time.Date(2026, 3, 16, 14, 0, 0, 0, time.UTC)
	now = time.Date(2026, 3, 17, 10, 0, 0, 0, time.UTC)
	stale = rt.isStale(meta, now)
	if !stale {
		t.Error("direct session from previous day should be stale")
	}
}

func TestIsStale_Group(t *testing.T) {
	cfg := testClawAgentCfg()
	rt := &ClawAgentRuntime{agentCfg: cfg}

	meta := &SessionMeta{
		ResetType:     "group",
		LastMessageAt: time.Now().Add(-20 * time.Minute),
	}

	// Within idle threshold (20min < 30min threshold)
	stale := rt.isStale(meta, time.Now())
	if stale {
		t.Error("group session within idle threshold should not be stale")
	}

	// Beyond idle threshold
	meta.LastMessageAt = time.Now().Add(-35 * time.Minute)
	stale = rt.isStale(meta, time.Now())
	if !stale {
		t.Error("group session beyond idle threshold should be stale")
	}
}

func TestIsStale_Event(t *testing.T) {
	cfg := testClawAgentCfg()
	rt := &ClawAgentRuntime{agentCfg: cfg}

	meta := &SessionMeta{
		ResetType:     "event",
		LastMessageAt: time.Now().Add(-40 * time.Minute),
	}

	// Events have 60min idle threshold
	stale := rt.isStale(meta, time.Now())
	if stale {
		t.Error("event session within 60min should not be stale")
	}

	meta.LastMessageAt = time.Now().Add(-70 * time.Minute)
	stale = rt.isStale(meta, time.Now())
	if !stale {
		t.Error("event session beyond 60min should be stale")
	}
}

func TestIsStale_Task(t *testing.T) {
	cfg := testClawAgentCfg()
	rt := &ClawAgentRuntime{agentCfg: cfg}

	meta := &SessionMeta{
		ResetType:     "task",
		LastMessageAt: time.Now().Add(-90 * time.Minute),
	}

	// Tasks have 120min idle threshold
	stale := rt.isStale(meta, time.Now())
	if stale {
		t.Error("task session within 120min should not be stale")
	}

	meta.LastMessageAt = time.Now().Add(-130 * time.Minute)
	stale = rt.isStale(meta, time.Now())
	if !stale {
		t.Error("task session beyond 120min should be stale")
	}
}

func TestIsStale_UnknownType(t *testing.T) {
	cfg := testClawAgentCfg()
	rt := &ClawAgentRuntime{agentCfg: cfg}

	meta := &SessionMeta{
		ResetType:     "unknown",
		LastMessageAt: time.Now().Add(-10 * time.Hour),
	}

	stale := rt.isStale(meta, time.Now())
	if stale {
		t.Error("unknown reset type should not be stale")
	}
}

func TestIsStale_ExactBoundary(t *testing.T) {
	cfg := testClawAgentCfg()
	rt := &ClawAgentRuntime{agentCfg: cfg}

	// Exactly at threshold
	meta := &SessionMeta{
		ResetType:     "group",
		LastMessageAt: time.Now().Add(-30 * time.Minute),
	}

	stale := rt.isStale(meta, time.Now())
	if stale {
		t.Error("session at exact idle threshold should not be stale yet")
	}
}

func testClawAgentCfg() ClawAgentConfig {
	return ClawAgentConfig{
		EncryptionKey:           "test-key-32-bytes-long-for-testing!",
		DefaultProvider:         "openai",
		DefaultModelName:        "gpt-4",
		SessionDailyResetHour:   4,
		SessionGroupIdleMinutes: 30,
		SessionEventIdleMinutes: 60,
		SessionTaskIdleMinutes:  120,
		SessionPruneAfterDays:   30,
		SessionMaxPerUser:       200,
		SessionMaxTokens:        8000,
		CompactionKeepRecent:    10,
	}
}
