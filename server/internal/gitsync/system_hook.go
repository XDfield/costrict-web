package gitsync

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

const systemHookTypeGitea = "gitea"

const managedSystemHookNamePrefix = "costrict-capability-sync-"

const (
	systemHookListPageSize = 50
	systemHookListMaxPages = 100
)

type giteaSystemHook struct {
	ID              int64             `json:"id"`
	Name            string            `json:"name"`
	Type            string            `json:"type"`
	Config          map[string]string `json:"config"`
	Events          []string          `json:"events"`
	Active          bool              `json:"active"`
	IsSystemWebhook bool              `json:"-"`
}

type giteaSystemHookRequest struct {
	Type   string            `json:"type"`
	Config map[string]string `json:"config"`
	Events []string          `json:"events"`
	Active bool              `json:"active"`
	Name   string            `json:"name"`
}

type giteaSystemHookUpdateRequest struct {
	Config map[string]string `json:"config"`
	Events []string          `json:"events"`
	Active bool              `json:"active"`
	Name   *string           `json:"name,omitempty"`
}

// EnsureSystemPushWebhook converges Gitea's system hooks to exactly one
// active push hook for gitServerID. A system hook covers existing repositories
// and repositories created or forked later, so callers do not need to mutate
// each repository lifecycle.
func (c *Client) EnsureSystemPushWebhook(ctx context.Context, gitServerID, targetURL, secret string) error {
	if c == nil {
		return ErrGiteaUnreachable
	}
	gitServerID = strings.TrimSpace(gitServerID)
	if gitServerID == "" || targetURL == "" || secret == "" {
		return fmt.Errorf("gitsync: system webhook Git server ID, target URL, and secret are required")
	}

	hooks, err := c.listSystemHooks(ctx)
	if err != nil {
		return err
	}
	managedPrefix := managedSystemHookServerPrefix(gitServerID)
	managed := make([]giteaSystemHook, 0, 1)
	for _, hook := range hooks {
		if isManagedSystemHookForServer(hook.Name, managedPrefix) {
			managed = append(managed, hook)
		}
	}
	sort.Slice(managed, func(i, j int) bool { return managed[i].ID < managed[j].ID })

	desired := giteaSystemHookRequest{
		Type: systemHookTypeGitea,
		Name: managedSystemHookName(gitServerID, secret),
		Config: map[string]string{
			"url":               targetURL,
			"content_type":      "json",
			"secret":            secret,
			"is_system_webhook": "true",
		},
		Events: []string{"push"},
		Active: true,
	}
	if len(managed) == 0 {
		return c.createSystemHook(ctx, desired)
	}

	primaryIndex := -1
	for i := range managed {
		if managed[i].Type == systemHookTypeGitea && managed[i].Name == desired.Name {
			primaryIndex = i
			break
		}
	}
	if primaryIndex < 0 {
		if err := c.createSystemHook(ctx, desired); err != nil {
			return err
		}
		for _, obsolete := range managed {
			if err := c.deleteSystemHook(ctx, obsolete.ID); err != nil {
				return fmt.Errorf("gitsync: delete obsolete system webhook %d: %w", obsolete.ID, err)
			}
		}
		return nil
	}

	primary := managed[primaryIndex]
	if !systemHookIsDesired(primary, desired) {
		if err := c.updateSystemHook(ctx, primary.ID, desired); err != nil {
			return err
		}
	}
	for i, duplicate := range managed {
		if i == primaryIndex {
			continue
		}
		if err := c.deleteSystemHook(ctx, duplicate.ID); err != nil {
			return fmt.Errorf("gitsync: delete duplicate system webhook %d: %w", duplicate.ID, err)
		}
	}
	return nil
}

func (c *Client) listSystemHooks(ctx context.Context) ([]giteaSystemHook, error) {
	all := make([]giteaSystemHook, 0)
	seen := make(map[int64]struct{})
	for page := 1; page <= systemHookListMaxPages; page++ {
		path := fmt.Sprintf("/api/v1/admin/hooks?type=system&page=%d&limit=%d", page, systemHookListPageSize)
		resp, err := c.doJSON(ctx, http.MethodGet, path, nil, http.StatusOK)
		if err != nil {
			return nil, fmt.Errorf("gitsync: list system webhooks page %d: %w", page, err)
		}
		var hooks []giteaSystemHook
		decodeErr := json.NewDecoder(resp.Body).Decode(&hooks)
		_ = resp.Body.Close()
		if decodeErr != nil {
			return nil, fmt.Errorf("%w: decode system webhooks page %d: %v", ErrGiteaUnreachable, page, decodeErr)
		}
		totalHeader := strings.TrimSpace(resp.Header.Get("X-Total-Count"))
		if totalHeader == "" {
			// Gitea 1.24.x ignores pagination and returns the complete list.
			return hooks, nil
		}
		total, err := strconv.Atoi(totalHeader)
		if err != nil || total < 0 {
			return nil, fmt.Errorf("gitsync: invalid X-Total-Count %q in system webhook listing", totalHeader)
		}
		for _, hook := range hooks {
			if _, ok := seen[hook.ID]; ok {
				continue
			}
			seen[hook.ID] = struct{}{}
			all = append(all, hook)
		}
		if len(all) > total {
			return nil, fmt.Errorf("gitsync: system webhook listing inconsistent: collected %d unique hooks but X-Total-Count is %d", len(all), total)
		}
		if len(all) == total {
			return all, nil
		}
		if len(hooks) == 0 {
			return nil, fmt.Errorf("gitsync: system webhook listing incomplete: collected %d of %d", len(all), total)
		}
	}
	return nil, fmt.Errorf("gitsync: system webhook listing reached safety limit of %d pages; collected incomplete list", systemHookListMaxPages)
}

func (c *Client) createSystemHook(ctx context.Context, desired giteaSystemHookRequest) error {
	resp, err := c.doJSON(ctx, http.MethodPost, "/api/v1/admin/hooks", desired, http.StatusCreated)
	if err != nil {
		return fmt.Errorf("gitsync: create system webhook: %w", err)
	}
	_ = resp.Body.Close()
	return nil
}

func (c *Client) updateSystemHook(ctx context.Context, hookID int64, desired giteaSystemHookRequest) error {
	path := "/api/v1/admin/hooks/" + strconv.FormatInt(hookID, 10)
	name := desired.Name
	update := giteaSystemHookUpdateRequest{
		Config: map[string]string{
			"url":          desired.Config["url"],
			"content_type": desired.Config["content_type"],
		},
		Events: desired.Events,
		Active: desired.Active,
		Name:   &name,
	}
	resp, err := c.doJSON(ctx, http.MethodPatch, path, update, http.StatusOK)
	if err != nil {
		return fmt.Errorf("gitsync: update system webhook %d: %w", hookID, err)
	}
	_ = resp.Body.Close()
	return nil
}

func (c *Client) deleteSystemHook(ctx context.Context, hookID int64) error {
	path := "/api/v1/admin/hooks/" + strconv.FormatInt(hookID, 10)
	resp, err := c.doJSON(ctx, http.MethodDelete, path, nil, http.StatusNoContent)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	return nil
}

func systemHookIsDesired(current giteaSystemHook, desired giteaSystemHookRequest) bool {
	return current.Name == desired.Name &&
		current.Type == desired.Type &&
		current.Active == desired.Active &&
		current.Config["url"] == desired.Config["url"] &&
		current.Config["content_type"] == desired.Config["content_type"] &&
		len(current.Events) == 1 && current.Events[0] == "push"
}

func managedSystemHookServerPrefix(gitServerID string) string {
	gitServerID = strings.TrimSpace(gitServerID)
	return fmt.Sprintf("%s%d-%s-", managedSystemHookNamePrefix, len(gitServerID), gitServerID)
}

func managedSystemHookName(gitServerID, secret string) string {
	digest := sha256.Sum256([]byte(secret))
	return fmt.Sprintf("%s%x", managedSystemHookServerPrefix(gitServerID), digest[:16])
}

func isManagedSystemHookForServer(name, serverPrefix string) bool {
	fingerprint := strings.TrimPrefix(name, serverPrefix)
	if fingerprint == name || len(fingerprint) != 32 {
		return false
	}
	for _, r := range fingerprint {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}
