// Package deptsync is a thin, config-driven HTTP client for the external
// dept-sync service (real department / user tree, X-API-Key authenticated).
//
// dept-sync is an OPTIONAL dependency: when it is not configured (no base URL or
// no API key) or unreachable, the client degrades gracefully — every method
// returns a typed error (ErrNotConfigured / a request error) instead of
// panicking or blocking. Handlers translate that into a 503 so the admin UI can
// show a "department service unavailable" notice rather than crashing.
//
// The bridge between dept-sync and costrict-web is the universal id:
// dept-sync user_department.universal_id == costrict-web users.casdoor_universal_id.
package deptsync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/costrict/costrict-web/server/internal/config"
	"github.com/costrict/costrict-web/server/internal/logger"
)

// ErrNotConfigured is returned by every Client method when the dept-sync service
// has no base URL / API key configured. Handlers map it to HTTP 503.
var ErrNotConfigured = errors.New("dept-sync service is not configured")

const (
	defaultTimeout  = 10 * time.Second
	defaultCacheTTL = 60 * time.Second
	apiKeyHeader    = "X-API-Key"
)

// Dept is one node of the department tree. Children is populated only by the
// tree endpoint (include_children=true); the flat endpoints leave it nil.
type Dept struct {
	DeptID         string `json:"deptId"`
	DeptName       string `json:"deptName"`
	DeptPath       string `json:"deptPath"`
	ParentDeptID   string `json:"parentDeptId"`
	DeptLevel      int    `json:"deptLevel"`
	ChildDeptCount int    `json:"childDeptCount"`
	LeaderID       string `json:"leaderId"`
	OrderNum       int    `json:"orderNum"`
	Children       []Dept `json:"children,omitempty"`
}

// DeptUser is one member of a department as reported by dept-sync. UniversalID
// is the bridge to costrict-web users.casdoor_universal_id.
type DeptUser struct {
	UserID      string `json:"userId"`
	Username    string `json:"username"`
	UniversalID string `json:"universalId"`
	IsMain      bool   `json:"isMain"`
	Position    string `json:"position"`
}

// Client talks to dept-sync over HTTP with a short-TTL in-memory cache. The
// zero value is not usable; construct with New.
type Client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
	cacheTTL   time.Duration

	mu    sync.Mutex
	cache map[string]cacheEntry
}

type cacheEntry struct {
	value     any
	expiresAt time.Time
}

// New builds a Client from config. It never fails; a missing base URL / API key
// simply produces a client whose Configured() reports false and whose methods
// return ErrNotConfigured.
func New(cfg config.DeptSyncConfig) *Client {
	timeout := time.Duration(cfg.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	ttl := time.Duration(cfg.CacheTTLSec) * time.Second
	if ttl <= 0 {
		ttl = defaultCacheTTL
	}
	return &Client{
		baseURL:    strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/"),
		apiKey:     strings.TrimSpace(cfg.APIKey),
		httpClient: &http.Client{Timeout: timeout},
		cacheTTL:   ttl,
		cache:      make(map[string]cacheEntry),
	}
}

// Configured reports whether dept-sync has both a base URL and an API key. When
// false, every fetch method short-circuits with ErrNotConfigured.
func (c *Client) Configured() bool {
	return c != nil && c.baseURL != "" && c.apiKey != ""
}

// envelope is the unified dept-sync response wrapper {code,message,data}. data is
// kept raw so callers can decode it as either a bare array or a {list,total}
// object (list endpoints differ).
type envelope struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// listWrapper handles the "{list,total}" shape some list endpoints return inside
// data. T is decoded element-by-element.
type listWrapper struct {
	List  json.RawMessage `json:"list"`
	Total int             `json:"total"`
}

// GetTree returns the full department tree (nested children). Cached for cacheTTL.
func (c *Client) GetTree() ([]Dept, error) {
	if !c.Configured() {
		return nil, ErrNotConfigured
	}
	const key = "tree"
	if v, ok := c.cacheGet(key); ok {
		return v.([]Dept), nil
	}
	raw, err := c.get("/api/department/tree?include_children=true")
	if err != nil {
		return nil, err
	}
	depts, err := decodeDeptList(raw)
	if err != nil {
		return nil, err
	}
	c.cacheSet(key, depts)
	return depts, nil
}

// GetDeptUsers returns the members of one department. Cached per department id.
func (c *Client) GetDeptUsers(deptID string) ([]DeptUser, error) {
	if !c.Configured() {
		return nil, ErrNotConfigured
	}
	key := "deptUsers:" + deptID
	if v, ok := c.cacheGet(key); ok {
		return v.([]DeptUser), nil
	}
	raw, err := c.get("/api/department/" + urlPathEscape(deptID) + "/users")
	if err != nil {
		return nil, err
	}
	users, err := decodeDeptUserList(raw)
	if err != nil {
		return nil, err
	}
	c.cacheSet(key, users)
	return users, nil
}

// GetUserDepartments returns the departments a user belongs to (one user may be
// in several). Cached per user id.
func (c *Client) GetUserDepartments(userID string) ([]Dept, error) {
	if !c.Configured() {
		return nil, ErrNotConfigured
	}
	key := "userDepts:" + userID
	if v, ok := c.cacheGet(key); ok {
		return v.([]Dept), nil
	}
	raw, err := c.get("/api/user/" + urlPathEscape(userID) + "/departments")
	if err != nil {
		return nil, err
	}
	depts, err := decodeDeptList(raw)
	if err != nil {
		return nil, err
	}
	c.cacheSet(key, depts)
	return depts, nil
}

// GetDepartmentPath resolves a single department's materialized path (dept_path)
// by its dept_id. It searches the cached department tree (one GetTree fetch,
// then memoized for cacheTTL), so resolving paths for several grants does not
// hammer dept-sync. Returns an error when dept-sync is not configured/unreachable
// or the department id is not found in the tree.
func (c *Client) GetDepartmentPath(deptID string) (string, error) {
	if !c.Configured() {
		return "", ErrNotConfigured
	}
	tree, err := c.GetTree()
	if err != nil {
		return "", err
	}
	if path, ok := findDeptPath(tree, deptID); ok {
		return path, nil
	}
	return "", fmt.Errorf("dept-sync: department %q not found", deptID)
}

// findDeptPath walks the nested department tree depth-first looking for deptID,
// returning its dept_path.
func findDeptPath(nodes []Dept, deptID string) (string, bool) {
	for _, n := range nodes {
		if n.DeptID == deptID {
			return n.DeptPath, true
		}
		if len(n.Children) > 0 {
			if path, ok := findDeptPath(n.Children, deptID); ok {
				return path, true
			}
		}
	}
	return "", false
}

// get performs an authenticated GET and unwraps the {code,message,data}
// envelope, returning the raw data payload for the caller to decode.
func (c *Client) get(path string) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(context.Background(), c.httpClient.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, fmt.Errorf("dept-sync: build request: %w", err)
	}
	req.Header.Set(apiKeyHeader, c.apiKey)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		logger.Warn("[deptsync] request to %s failed: %v", path, err)
		return nil, fmt.Errorf("dept-sync: request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("dept-sync: read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		logger.Warn("[deptsync] %s returned status %d: %s", path, resp.StatusCode, truncate(string(body), 200))
		return nil, fmt.Errorf("dept-sync: upstream returned status %d", resp.StatusCode)
	}

	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("dept-sync: decode envelope: %w", err)
	}
	// Treat a non-zero application code as an upstream error. dept-sync uses 0 or
	// 200 for success depending on the endpoint; both are accepted.
	if env.Code != 0 && env.Code != http.StatusOK {
		return nil, fmt.Errorf("dept-sync: upstream code %d: %s", env.Code, env.Message)
	}
	if len(env.Data) == 0 {
		return json.RawMessage("null"), nil
	}
	return env.Data, nil
}

// decodeDeptList decodes a department list from data that may be either a bare
// array or a {list,total} wrapper.
func decodeDeptList(raw json.RawMessage) ([]Dept, error) {
	inner := unwrapList(raw)
	if isNull(inner) {
		return []Dept{}, nil
	}
	var depts []Dept
	if err := json.Unmarshal(inner, &depts); err != nil {
		return nil, fmt.Errorf("dept-sync: decode departments: %w", err)
	}
	return depts, nil
}

// decodeDeptUserList decodes a dept-user list from data that may be either a bare
// array or a {list,total} wrapper.
func decodeDeptUserList(raw json.RawMessage) ([]DeptUser, error) {
	inner := unwrapList(raw)
	if isNull(inner) {
		return []DeptUser{}, nil
	}
	var users []DeptUser
	if err := json.Unmarshal(inner, &users); err != nil {
		return nil, fmt.Errorf("dept-sync: decode dept users: %w", err)
	}
	return users, nil
}

// unwrapList returns the inner array payload, transparently unwrapping a
// {list,total} object when present. A bare array is returned unchanged.
func unwrapList(raw json.RawMessage) json.RawMessage {
	trimmed := strings.TrimSpace(string(raw))
	if strings.HasPrefix(trimmed, "{") {
		var lw listWrapper
		if err := json.Unmarshal(raw, &lw); err == nil && len(lw.List) > 0 {
			return lw.List
		}
	}
	return raw
}

func isNull(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s == "" || s == "null"
}

func (c *Client) cacheGet(key string) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.cache[key]
	if !ok || time.Now().After(entry.expiresAt) {
		if ok {
			delete(c.cache, key)
		}
		return nil, false
	}
	return entry.value, true
}

func (c *Client) cacheSet(key string, value any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cache[key] = cacheEntry{value: value, expiresAt: time.Now().Add(c.cacheTTL)}
}

// InvalidateCache clears all cached dept-sync responses. Exposed so a future
// "refresh" admin action or sync trigger can drop stale data immediately.
func (c *Client) InvalidateCache() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cache = make(map[string]cacheEntry)
}

// urlPathEscape escapes a path segment while keeping it readable; dept ids /
// user ids are simple tokens but may contain special characters.
func urlPathEscape(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "?", "%3F"), "#", "%23")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
