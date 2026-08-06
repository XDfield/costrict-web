package worker

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type fakeGitSystemHookEnsurer struct {
	mu      sync.Mutex
	calls   []gitSystemHookCall
	err     error
	failFor int
	entered chan struct{}
	release <-chan struct{}
}

type gitSystemHookCall struct {
	serverID string
	target   string
	secret   string
}

func (f *fakeGitSystemHookEnsurer) EnsureSystemCapabilityWebhook(_ context.Context, serverID, targetURL, secret string) error {
	f.mu.Lock()
	f.calls = append(f.calls, gitSystemHookCall{serverID: serverID, target: targetURL, secret: secret})
	callNumber := len(f.calls)
	f.mu.Unlock()
	if f.entered != nil {
		select {
		case f.entered <- struct{}{}:
		default:
		}
	}
	if f.release != nil {
		<-f.release
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failFor > 0 && callNumber <= f.failFor {
		return f.err
	}
	return nil
}

func (f *fakeGitSystemHookEnsurer) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

type channelGitSystemHookLocker struct {
	token    chan struct{}
	finished chan bool
}

type channelGitSystemHookLock struct {
	token    chan struct{}
	finished chan<- bool
	once     sync.Once
}

func newChannelGitSystemHookLocker() *channelGitSystemHookLocker {
	return &channelGitSystemHookLocker{token: make(chan struct{}, 1), finished: make(chan bool, 10)}
}

func (l *channelGitSystemHookLocker) TryLock(context.Context, string) (GitSystemHookLock, bool, error) {
	select {
	case l.token <- struct{}{}:
		return &channelGitSystemHookLock{token: l.token, finished: l.finished}, true, nil
	default:
		return nil, false, nil
	}
}

func (l *channelGitSystemHookLock) Finish(success bool) error {
	l.once.Do(func() {
		l.finished <- success
		<-l.token
	})
	return nil
}

type contextDeadlineGitSystemHookEnsurer struct{}

func (contextDeadlineGitSystemHookEnsurer) EnsureSystemCapabilityWebhook(ctx context.Context, _, _, _ string) error {
	<-ctx.Done()
	return ctx.Err()
}

type stopBlockingGitSystemHookEnsurer struct {
	mu      sync.Mutex
	calls   []string
	entered chan struct{}
}

func (e *stopBlockingGitSystemHookEnsurer) EnsureSystemCapabilityWebhook(ctx context.Context, serverID, _, _ string) error {
	e.mu.Lock()
	e.calls = append(e.calls, serverID)
	e.mu.Unlock()
	select {
	case e.entered <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return ctx.Err()
}

func (e *stopBlockingGitSystemHookEnsurer) calledServerIDs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.calls...)
}

type selectiveErrorGitSystemHookLocker struct {
	failServerID string
	err          error
}

func (l *selectiveErrorGitSystemHookLocker) TryLock(_ context.Context, serverID string) (GitSystemHookLock, bool, error) {
	if serverID == l.failServerID {
		return nil, false, l.err
	}
	return noOpGitSystemHookLock{}, true, nil
}

func setupGitSystemHookReconcilerDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open SQLite: %v", err)
	}
	if err := db.Exec(`CREATE TABLE git_servers (
		server_id TEXT PRIMARY KEY, kind TEXT NOT NULL, endpoint TEXT NOT NULL,
		display_name TEXT NOT NULL, config TEXT NOT NULL DEFAULT '{}',
		is_template INTEGER NOT NULL DEFAULT 0, enabled INTEGER NOT NULL DEFAULT 1,
		created_at DATETIME, updated_at DATETIME
	)`).Error; err != nil {
		t.Fatalf("create git_servers: %v", err)
	}
	return db
}

func seedGitSystemHookServer(t *testing.T, db *gorm.DB, id, endpoint, config string, enabled bool) {
	t.Helper()
	if err := db.Exec(`INSERT INTO git_servers
		(server_id, kind, endpoint, display_name, config, is_template, enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, 0, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		id, models.GitServerKindGitea, endpoint, id, config, enabled).Error; err != nil {
		t.Fatalf("seed Git server %s: %v", id, err)
	}
}

func TestBuildGitSystemWebhookURL_PreservesDeploymentPrefix(t *testing.T) {
	got, err := buildGitSystemWebhookURL("https://cloud.example/cloud-api/", "gs-1")
	if err != nil {
		t.Fatalf("build URL: %v", err)
	}
	want := "https://cloud.example/cloud-api/api/internal/git-sync/gs-1"
	if got != want {
		t.Fatalf("URL = %q, want %q", got, want)
	}
}

func TestGitSystemHookReconciler_EmptyBaseURLIsDisabledBeforeDatabaseAccess(t *testing.T) {
	reconciler := &GitSystemHookReconciler{}
	if err := reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("disabled reconciliation returned error: %v", err)
	}
	if err := reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("repeated disabled reconciliation returned error: %v", err)
	}
}

func TestGitSystemHookReconciler_IsolatesServerErrorsAndSkipsMissingConfig(t *testing.T) {
	db := setupGitSystemHookReconcilerDB(t)
	seedGitSystemHookServer(t, db, "gs-error", "https://git-error.example", `{"admin_token":"tok-error","webhook_secret":"sec-error"}`, true)
	seedGitSystemHookServer(t, db, "gs-missing", "https://git-missing.example", `{"admin_token":"tok-missing"}`, true)
	seedGitSystemHookServer(t, db, "gs-ok", "https://git-ok.example", `{"admin_token":"tok-ok","webhook_secret":"sec-ok"}`, true)
	seedGitSystemHookServer(t, db, "gs-disabled", "https://git-disabled.example", `{"admin_token":"tok-disabled","webhook_secret":"sec-disabled"}`, false)

	failing := &fakeGitSystemHookEnsurer{err: errors.New("Gitea unavailable"), failFor: 1}
	succeeding := &fakeGitSystemHookEnsurer{}
	created := make(map[string]int)
	reconciler := &GitSystemHookReconciler{
		DB: db, WebhookBaseURL: "https://cloud.example/cloud", RequestTimeout: time.Second,
		NewClient: func(endpoint, _ string) GitSystemHookEnsurer {
			created[endpoint]++
			if endpoint == "https://git-error.example" {
				return failing
			}
			return succeeding
		},
	}
	err := reconciler.ReconcileOnce(context.Background())
	if err == nil || !errors.Is(err, failing.err) {
		t.Fatalf("reconcile error = %v, want isolated Gitea error", err)
	}
	if failing.callCount() != 1 || succeeding.callCount() != 1 {
		t.Fatalf("calls: failing=%d succeeding=%d", failing.callCount(), succeeding.callCount())
	}
	if created["https://git-missing.example"] != 0 || created["https://git-disabled.example"] != 0 {
		t.Fatalf("skipped servers unexpectedly created clients: %v", created)
	}
	if got := succeeding.calls[0]; got.serverID != "gs-ok" || got.target != "https://cloud.example/cloud/api/internal/git-sync/gs-ok" || got.secret != "sec-ok" {
		t.Fatalf("successful call = %+v", got)
	}
}

func TestGitSystemHookReconciler_LockErrorsDoNotBlockOtherServers(t *testing.T) {
	db := setupGitSystemHookReconcilerDB(t)
	seedGitSystemHookServer(t, db, "gs-lock-error", "https://git-error.example", `{"admin_token":"tok-error","webhook_secret":"sec-error"}`, true)
	seedGitSystemHookServer(t, db, "gs-ok", "https://git-ok.example", `{"admin_token":"tok-ok","webhook_secret":"sec-ok"}`, true)

	lockErr := errors.New("lock database unavailable")
	ensurer := &fakeGitSystemHookEnsurer{}
	reconciler := &GitSystemHookReconciler{
		DB: db, WebhookBaseURL: "https://cloud.example/cloud-api", RequestTimeout: time.Second,
		Locker:    &selectiveErrorGitSystemHookLocker{failServerID: "gs-lock-error", err: lockErr},
		NewClient: func(_, _ string) GitSystemHookEnsurer { return ensurer },
	}
	err := reconciler.ReconcileOnce(context.Background())
	if err == nil || !errors.Is(err, lockErr) {
		t.Fatalf("reconcile error = %v, want lock error", err)
	}
	if ensurer.callCount() != 1 || ensurer.calls[0].serverID != "gs-ok" {
		t.Fatalf("ensurer calls = %+v, want only gs-ok", ensurer.calls)
	}
}

func TestGitSystemHookReconciler_ConcurrentReconcilersOnlyOneEnsures(t *testing.T) {
	db := setupGitSystemHookReconcilerDB(t)
	seedGitSystemHookServer(t, db, "gs-1", "https://git.example", `{"admin_token":"tok","webhook_secret":"sec"}`, true)

	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	ensurer := &fakeGitSystemHookEnsurer{entered: entered, release: release}
	locker := newChannelGitSystemHookLocker()
	newReconciler := func() *GitSystemHookReconciler {
		return &GitSystemHookReconciler{
			DB: db, WebhookBaseURL: "https://cloud.example/cloud-api", RequestTimeout: time.Second,
			Locker: locker, NewClient: func(_, _ string) GitSystemHookEnsurer { return ensurer },
		}
	}

	firstDone := make(chan error, 1)
	go func() { firstDone <- newReconciler().ReconcileOnce(context.Background()) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first reconciler did not enter the protected Ensure call")
	}
	if err := newReconciler().ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	if ensurer.callCount() != 1 {
		t.Fatalf("Ensure calls = %d, want one protected create/reconcile", ensurer.callCount())
	}
	if success := <-locker.finished; !success {
		t.Fatal("successful Ensure should commit the advisory-lock transaction")
	}
}

func TestGitSystemHookReconciler_RequestTimeoutRollsBackLockTransaction(t *testing.T) {
	db := setupGitSystemHookReconcilerDB(t)
	seedGitSystemHookServer(t, db, "gs-1", "https://git.example", `{"admin_token":"tok","webhook_secret":"sec"}`, true)
	locker := newChannelGitSystemHookLocker()
	reconciler := &GitSystemHookReconciler{
		DB: db, WebhookBaseURL: "https://cloud.example/cloud-api", RequestTimeout: 10 * time.Millisecond,
		Locker: locker, NewClient: func(_, _ string) GitSystemHookEnsurer { return contextDeadlineGitSystemHookEnsurer{} },
	}
	if err := reconciler.ReconcileOnce(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("reconcile error = %v, want deadline exceeded", err)
	}
	if success := <-locker.finished; success {
		t.Fatal("timed-out Ensure should roll back the advisory-lock transaction")
	}
}

func TestGitSystemHookReconciler_StartRunsImmediatelyAndRetriesPeriodically(t *testing.T) {
	db := setupGitSystemHookReconcilerDB(t)
	seedGitSystemHookServer(t, db, "gs-retry", "https://git.example", `{"admin_token":"tok","webhook_secret":"sec"}`, true)
	ensurer := &fakeGitSystemHookEnsurer{err: errors.New("first attempt fails"), failFor: 1}
	reconciler := &GitSystemHookReconciler{
		DB: db, WebhookBaseURL: "https://cloud.example", Interval: 10 * time.Millisecond,
		RequestTimeout: time.Second,
		NewClient:      func(_, _ string) GitSystemHookEnsurer { return ensurer },
	}
	reconciler.Start()
	defer reconciler.Stop()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if ensurer.callCount() >= 2 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("calls = %d, want immediate attempt plus periodic retry", ensurer.callCount())
}

func TestGitSystemHookReconciler_StopCancelsBlockedEnsureAndSkipsRemainingServers(t *testing.T) {
	db := setupGitSystemHookReconcilerDB(t)
	seedGitSystemHookServer(t, db, "gs-a", "https://git-a.example", `{"admin_token":"tok-a","webhook_secret":"sec-a"}`, true)
	seedGitSystemHookServer(t, db, "gs-b", "https://git-b.example", `{"admin_token":"tok-b","webhook_secret":"sec-b"}`, true)
	ensurer := &stopBlockingGitSystemHookEnsurer{entered: make(chan struct{}, 1)}
	reconciler := &GitSystemHookReconciler{
		DB: db, WebhookBaseURL: "https://cloud.example/cloud-api", Interval: time.Hour, RequestTimeout: time.Hour,
		NewClient: func(_, _ string) GitSystemHookEnsurer { return ensurer },
	}
	reconciler.Start()
	select {
	case <-ensurer.entered:
	case <-time.After(time.Second):
		t.Fatal("reconciler did not enter the first Ensure call")
	}

	stopped := make(chan struct{})
	go func() {
		reconciler.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("Stop did not cancel the blocked Ensure call")
	}
	reconciler.Stop()
	if got := ensurer.calledServerIDs(); len(got) != 1 || got[0] != "gs-a" {
		t.Fatalf("Ensure calls after Stop = %v, want only gs-a", got)
	}
}
