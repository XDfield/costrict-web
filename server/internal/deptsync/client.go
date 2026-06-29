// Package deptsync is a thin, config-driven HTTP client for the external
// dept-sync service (real department / user tree, X-Query-Key authenticated).
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
	"net/url"
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
	// dept-sync mounts its data endpoints under /costrict-dept-info/api/v1 and
	// authenticates them with the X-Query-Key header (query_key table). Both are
	// overridable via config (DEPT_SYNC_PATH_PREFIX / DEPT_SYNC_AUTH_HEADER) so a
	// future service version can be targeted without a code change.
	defaultPathPrefix = "/costrict-dept-info/api/v1"
	defaultAuthHeader = "X-Query-Key"
)

// Dept is one node of the department tree. It is the response contract returned
// verbatim to the admin frontend (camelCase AdminDept), so its tags stay camelCase;
// dept-sync's snake_case payload is decoded via deptNode and mapped in. Children is
// populated only by the tree endpoint; the flat endpoints leave it nil.
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

// deptNode mirrors dept-sync's snake_case department node (the /department/tree and
// /user/{id}/departments endpoints share this shape) for decoding only; toDept maps
// it to the camelCase Dept contract. Keeping decode and frontend-output shapes
// separate is what lets the upstream contract change without touching the frontend.
type deptNode struct {
	DeptID         string     `json:"dept_id"`
	DeptName       string     `json:"dept_name"`
	DeptPath       string     `json:"dept_path"`
	ParentDeptID   string     `json:"parent_dept_id"`
	DeptLevel      int        `json:"dept_level"`
	OrderNum       int        `json:"order_num"`
	LeaderID       string     `json:"leader_id"`
	ChildDeptCount int        `json:"child_dept_count"`
	Children       []deptNode `json:"children"`
}

func (n deptNode) toDept() Dept {
	d := Dept{
		DeptID:         n.DeptID,
		DeptName:       n.DeptName,
		DeptPath:       n.DeptPath,
		ParentDeptID:   n.ParentDeptID,
		DeptLevel:      n.DeptLevel,
		ChildDeptCount: n.ChildDeptCount,
		LeaderID:       n.LeaderID,
		OrderNum:       n.OrderNum,
	}
	if len(n.Children) > 0 {
		d.Children = make([]Dept, len(n.Children))
		for i, c := range n.Children {
			d.Children[i] = c.toDept()
		}
	}
	return d
}

// DeptUser is one member of a department as reported by dept-sync. UniversalID
// is the bridge to costrict-web users.casdoor_universal_id. Field tags match
// dept-sync's snake_case data API; IsMain is an int flag (1 = primary membership).
type DeptUser struct {
	UserID      string `json:"user_id"`
	Username    string `json:"username"`
	UniversalID string `json:"universal_id"`
	DeptID      string `json:"dept_id"`
	DeptName    string `json:"dept_name"`
	Position    string `json:"position"`
	IsMain      int    `json:"is_main"`
	Status      int    `json:"status"`
}

// Client talks to dept-sync over HTTP with a short-TTL in-memory cache. The
// zero value is not usable; construct with New.
type Client struct {
	baseURL    string
	apiKey     string
	pathPrefix string
	authHeader string
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
	// Normalize the prefix to exactly one leading slash and no trailing slash, so a
	// misconfigured override (e.g. "costrict-dept-info/api/v1" without a leading
	// slash) still yields a valid URL rather than "http://hostcostrict-...".
	prefix := strings.Trim(strings.TrimSpace(cfg.PathPrefix), "/")
	if prefix == "" {
		prefix = defaultPathPrefix
	} else {
		prefix = "/" + prefix
	}
	authHeader := strings.TrimSpace(cfg.AuthHeader)
	if authHeader == "" {
		authHeader = defaultAuthHeader
	}
	return &Client{
		baseURL:    strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/"),
		apiKey:     strings.TrimSpace(cfg.APIKey),
		pathPrefix: prefix,
		authHeader: authHeader,
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

// envelope is the unified dept-sync response wrapper {code,success,message,data}.
// dept-sync data endpoints return code as a string and a boolean success flag, so
// Code is kept raw (tolerates string or number) and Success is a pointer (nil when
// the endpoint omits it — then success is judged solely by HTTP status). data is
// kept raw so callers can decode it as either a bare array or a {list,total} object.
type envelope struct {
	Code    json.RawMessage `json:"code"`
	Success *bool           `json:"success"`
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
	// dept-sync's tree endpoint returns the full nested tree when no dept_id is given;
	// it reads only dept_id, not include_children (that flag is for the users endpoint).
	raw, err := c.get("/department/tree")
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
	raw, err := c.get("/department/" + url.PathEscape(deptID) + "/users")
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
	// authz passes a universal_id, so query by universal; dept-sync's user-departments
	// endpoint defaults to type=user_id (工号) and would otherwise not match.
	raw, err := c.get("/user/" + url.PathEscape(userID) + "/departments?type=universal")
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

// get performs an authenticated GET (base URL + configured path prefix), sends the
// configured auth header, and unwraps the {code,success,data} envelope, returning
// the raw data payload for the caller to decode.
func (c *Client) get(path string) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(context.Background(), c.httpClient.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+c.pathPrefix+path, nil)
	if err != nil {
		return nil, fmt.Errorf("dept-sync: build request: %w", err)
	}
	req.Header.Set(c.authHeader, c.apiKey)
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
	// Only an explicit success:false is treated as an application error. The string
	// "code" is informational (it varies across endpoints) and is not used as a
	// gate, avoiding brittle code-value comparisons; transport/auth failures already
	// surface as a non-200 status above.
	if env.Success != nil && !*env.Success {
		return nil, fmt.Errorf("dept-sync: upstream reported failure (code=%s message=%s)",
			strings.TrimSpace(string(env.Code)), env.Message)
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
	var nodes []deptNode
	if err := json.Unmarshal(inner, &nodes); err != nil {
		return nil, fmt.Errorf("dept-sync: decode departments: %w", err)
	}
	depts := make([]Dept, len(nodes))
	for i, n := range nodes {
		depts[i] = n.toDept()
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

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
