package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/costrict/costrict-web/server/internal/gitsync"
	"github.com/costrict/costrict-web/server/internal/logger"
	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/gorm"
)

const defaultGitSystemHookReconcileInterval = 5 * time.Minute

type GitSystemHookEnsurer interface {
	EnsureSystemCapabilityWebhook(ctx context.Context, gitServerID, targetURL, secret string) error
}

type GitSystemHookReconciler struct {
	DB             *gorm.DB
	WebhookBaseURL string
	Interval       time.Duration
	RequestTimeout time.Duration
	NewClient      func(endpoint, adminToken string) GitSystemHookEnsurer
	Locker         GitSystemHookLocker

	lifecycleMu     sync.Mutex
	cancel          context.CancelFunc
	running         bool
	wg              sync.WaitGroup
	disabledLogOnce sync.Once
}

func (r *GitSystemHookReconciler) Start() {
	if r == nil || r.DB == nil {
		return
	}
	r.lifecycleMu.Lock()
	defer r.lifecycleMu.Unlock()
	if r.running {
		return
	}
	if r.Interval <= 0 {
		r.Interval = defaultGitSystemHookReconcileInterval
	}
	if r.RequestTimeout <= 0 {
		r.RequestTimeout = 15 * time.Second
	}
	if r.NewClient == nil {
		r.NewClient = func(endpoint, adminToken string) GitSystemHookEnsurer {
			client := gitsync.NewClient(endpoint, adminToken)
			if client == nil {
				return nil
			}
			return client
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.running = true
	r.wg.Add(1)
	go r.run(ctx)
}

func (r *GitSystemHookReconciler) Stop() {
	if r == nil {
		return
	}
	r.lifecycleMu.Lock()
	defer r.lifecycleMu.Unlock()
	if r.cancel == nil {
		return
	}
	r.cancel()
	r.wg.Wait()
	r.cancel = nil
	r.running = false
}

func (r *GitSystemHookReconciler) run(ctx context.Context) {
	defer r.wg.Done()
	r.reconcileAndLog(ctx)
	ticker := time.NewTicker(r.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.reconcileAndLog(ctx)
		}
	}
}

func (r *GitSystemHookReconciler) reconcileAndLog(ctx context.Context) {
	if err := r.ReconcileOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("Git system webhook reconciliation completed with errors: %v", err)
	}
}

// ReconcileOnce converges all enabled Gitea servers independently. Missing
// opt-in configuration is a gray-rollout skip; external failures are returned
// after the remaining servers have still been processed.
func (r *GitSystemHookReconciler) ReconcileOnce(ctx context.Context) error {
	if r == nil {
		return errors.New("Git system webhook reconciler is not configured")
	}
	if strings.TrimSpace(r.WebhookBaseURL) == "" {
		r.disabledLogOnce.Do(func() {
			logger.Warn("Git system webhook reconciliation disabled: GIT_SYSTEM_WEBHOOK_BASE_URL is empty")
		})
		return nil
	}
	if r.DB == nil {
		return errors.New("Git system webhook reconciler is not configured")
	}

	var servers []models.GitServer
	if err := r.DB.WithContext(ctx).
		Where("enabled = ? AND kind = ?", true, models.GitServerKindGitea).
		Order("server_id ASC").Find(&servers).Error; err != nil {
		return fmt.Errorf("query enabled Git servers: %w", err)
	}

	var reconcileErrors []error
	locker := r.Locker
	if locker == nil {
		locker = newGitSystemHookLocker(r.DB)
	}
	for i := range servers {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(reconcileErrors, err)...)
		}
		server := &servers[i]
		cfg, err := parseGitSystemHookConfig(server.Config)
		if err != nil {
			logger.Warn("Git system webhook skipped serverID=%s reason=invalid-config err=%v", server.ServerID, err)
			continue
		}
		missing := missingGitSystemHookConfig(server, cfg)
		if len(missing) > 0 {
			logger.Warn("Git system webhook skipped serverID=%s reason=missing-config fields=%s", server.ServerID, strings.Join(missing, ","))
			continue
		}
		targetURL, err := buildGitSystemWebhookURL(r.WebhookBaseURL, server.ServerID)
		if err != nil {
			logger.Warn("Git system webhook skipped serverID=%s reason=invalid-GIT_SYSTEM_WEBHOOK_BASE_URL err=%v", server.ServerID, err)
			continue
		}

		factory := r.NewClient
		if factory == nil {
			factory = func(endpoint, adminToken string) GitSystemHookEnsurer {
				client := gitsync.NewClient(endpoint, adminToken)
				if client == nil {
					return nil
				}
				return client
			}
		}
		client := factory(server.Endpoint, cfg.AdminToken)
		if client == nil {
			logger.Warn("Git system webhook skipped serverID=%s reason=client-unavailable", server.ServerID)
			continue
		}
		lock, acquired, lockErr := locker.TryLock(ctx, server.ServerID)
		if lockErr != nil {
			logger.Error("Git system webhook lock failed serverID=%s err=%v", server.ServerID, lockErr)
			reconcileErrors = append(reconcileErrors, fmt.Errorf("server %s advisory lock: %w", server.ServerID, lockErr))
			continue
		}
		if !acquired {
			logger.Info("Git system webhook reconciliation skipped serverID=%s reason=lock-held", server.ServerID)
			continue
		}
		timeout := r.RequestTimeout
		if timeout <= 0 {
			timeout = 15 * time.Second
		}
		err = func() (retErr error) {
			defer func() {
				panicValue := recover()
				if finishErr := lock.Finish(panicValue == nil && retErr == nil); finishErr != nil {
					retErr = errors.Join(retErr, fmt.Errorf("finish advisory lock transaction: %w", finishErr))
				}
				if panicValue != nil {
					panic(panicValue)
				}
			}()
			requestCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			return client.EnsureSystemCapabilityWebhook(requestCtx, server.ServerID, targetURL, cfg.WebhookSecret)
		}()
		if err != nil {
			logger.Error("Git system webhook reconcile failed serverID=%s err=%v", server.ServerID, err)
			reconcileErrors = append(reconcileErrors, fmt.Errorf("server %s: %w", server.ServerID, err))
			continue
		}
		logger.Info("Git system webhook reconciled serverID=%s target=%s", server.ServerID, targetURL)
	}
	return errors.Join(reconcileErrors...)
}

type gitSystemHookConfig struct {
	AdminToken    string `json:"admin_token"`
	WebhookSecret string `json:"webhook_secret"`
}

func parseGitSystemHookConfig(raw string) (gitSystemHookConfig, error) {
	var cfg gitSystemHookConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func missingGitSystemHookConfig(server *models.GitServer, cfg gitSystemHookConfig) []string {
	missing := make([]string, 0, 3)
	if server == nil || strings.TrimSpace(server.Endpoint) == "" {
		missing = append(missing, "endpoint")
	}
	if strings.TrimSpace(cfg.AdminToken) == "" {
		missing = append(missing, "admin_token")
	}
	if strings.TrimSpace(cfg.WebhookSecret) == "" {
		missing = append(missing, "webhook_secret")
	}
	return missing
}

func buildGitSystemWebhookURL(baseURL, serverID string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return "", fmt.Errorf("parse base URL: %w", err)
	}
	if parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("base URL must be an absolute HTTP(S) URL without userinfo, query, or fragment")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", errors.New("base URL scheme must be http or https")
	}
	serverID = strings.TrimSpace(serverID)
	if serverID == "" || strings.Contains(serverID, "/") {
		return "", errors.New("Git server ID is not a valid URL path segment")
	}
	target, err := url.JoinPath(parsed.String(), "api", "internal", "git-sync", serverID)
	if err != nil {
		return "", fmt.Errorf("join webhook URL: %w", err)
	}
	return target, nil
}
