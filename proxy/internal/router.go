package internal

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/costrict/costrict-web/proxy/internal/audit"
	"github.com/costrict/costrict-web/proxy/internal/filter"
	"github.com/costrict/costrict-web/proxy/internal/logger"
	"github.com/costrict/costrict-web/proxy/internal/proxy"
)

type Router struct {
	engine       *gin.Engine
	cfg          *Config
	rules        *filter.FilterRules
	auditWorker  *audit.Worker
	reverseProxy *httputil.ReverseProxy
	sseProxy     *httputil.ReverseProxy
}

func NewRouter(engine *gin.Engine, cfg *Config, rules *filter.FilterRules, auditWorker *audit.Worker) *Router {
	target, _ := url.Parse(cfg.ServerURL)

	rp := newReverseProxy(target)
	sseProxy := newReverseProxy(target)
	sseProxy.FlushInterval = -1

	return &Router{
		engine:       engine,
		cfg:          cfg,
		rules:        rules,
		auditWorker:  auditWorker,
		reverseProxy: rp,
		sseProxy:     sseProxy,
	}
}

func newReverseProxy(target *url.URL) *httputil.ReverseProxy {
	rp := httputil.NewSingleHostReverseProxy(target)
	origDirector := rp.Director
	rp.Director = func(req *http.Request) {
		origDirector(req)
		req.Host = target.Host
	}
	return rp
}

func (r *Router) Setup() {
	engine := r.engine

	engine.Use(r.routeMiddleware())
	engine.Any("/*path", func(c *gin.Context) {})
}

func (r *Router) routeMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		path := c.Request.URL.Path
		method := c.Request.Method

		if path == "/health" {
			c.JSON(http.StatusOK, gin.H{"status": "ok"})
			c.Abort()
			return
		}
		if path == "/health/ready" {
			c.JSON(http.StatusOK, gin.H{"status": "ok", "db": "ok", "upstream": "ok"})
			c.Abort()
			return
		}

		if isUpgrade(c) {
			if strings.Contains(path, "/terminal/") {
				c.JSON(http.StatusForbidden, gin.H{
					"error": "terminal disabled",
					"code":  "TERMINAL_DISABLED",
				})
				c.Abort()
				return
			}
			r.reverseProxy.ServeHTTP(c.Writer, c.Request)
			c.Abort()
			return
		}

		if isTerminalPath(path) {
			c.JSON(http.StatusForbidden, gin.H{
				"error": "terminal disabled",
				"code":  "TERMINAL_DISABLED",
			})
			c.Abort()
			return
		}

		if isDisabledPath(path, method) {
			c.JSON(http.StatusForbidden, gin.H{
				"error": "terminal disabled",
				"code":  "TERMINAL_DISABLED",
			})
			c.Abort()
			return
		}

		if isRuntimeContentPath(path) {
			logger.Info("[proxy] %s %s → 403 runtime disabled", method, path)
			c.JSON(http.StatusForbidden, gin.H{
				"error": "runtime file content disabled",
				"code":  "RUNTIME_FILE_DISABLED",
			})
			c.Abort()
			return
		}

		if isRuntimeTreePath(path) {
			logger.Info("[proxy] %s %s → 403 runtime tree disabled", method, path)
			c.JSON(http.StatusForbidden, gin.H{
				"error": "runtime file tree disabled",
				"code":  "RUNTIME_TREE_DISABLED",
			})
			c.Abort()
			return
		}

		if isRuntimeDiffListPath(path) {
			logger.Info("[proxy] %s %s → 403 runtime diff disabled", method, path)
			c.JSON(http.StatusForbidden, gin.H{
				"error": "runtime diff list disabled",
				"code":  "RUNTIME_DIFF_DISABLED",
			})
			c.Abort()
			return
		}

		if isSSERequest(c, path) {
			logger.Info("[proxy] %s %s → sse-stream", method, path)
			r.handleSSE(c)
			return
		}

		route := classifyRoute(path, method)

		switch route {
		case routeIntercept:
			logger.Info("[proxy] %s %s → intercept", method, path)
			r.handleIntercept(c)
		case routeAuditOnly:
			logger.Info("[proxy] %s %s → audit-only", method, path)
			r.handleAuditOnly(c)
		default:
			r.reverseProxy.ServeHTTP(c.Writer, c.Request)
			c.Abort()
		}
	}
}

func (r *Router) handleSSE(c *gin.Context) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("X-Accel-Buffering", "no")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Status(http.StatusOK)

	pr, pw := io.Pipe()

	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				logger.Info("[sse] %s upstream goroutine recovered: %v", c.Request.URL.Path, rec)
			}
			pw.Close()
		}()
		r.sseProxy.ServeHTTP(&ssePipeWriter{pw}, c.Request)
	}()

	defer func() {
		if rec := recover(); rec != nil {
			logger.Info("[sse] %s connection closed", c.Request.URL.Path)
		}
	}()

	var summary filter.AuditSummary
	err := filter.FilterSSEStream(pr, c.Writer, r.rules, func(s *filter.AuditSummary) {
		if s.Filtered {
			summary.Filtered = true
		}
		summary.CodeBlocksTotal += s.CodeBlocksTotal
		summary.CodeBlocksFiltered += s.CodeBlocksFiltered
		summary.ToolsCount += s.ToolsCount
		summary.ToolActions = append(summary.ToolActions, s.ToolActions...)
	})
	if err != nil {
		logger.Info("[sse] %s filter error: %v", c.Request.URL.Path, err)
	}

	if summary.Filtered {
		entry := audit.NewAuditLog()
		entry.ApiPath = c.Request.URL.Path
		entry.Method = c.Request.Method
		entry.IsSse = true
		entry.Filtered = summary.Filtered
		entry.CodeBlocksTotal = summary.CodeBlocksTotal
		entry.CodeBlocksFiltered = summary.CodeBlocksFiltered
		entry.ToolsCount = summary.ToolsCount
		entry.Tools, entry.Files = buildAuditChildren(summary.ToolActions)
		r.fillAuditEntry(c, entry)
		r.auditWorker.Send(entry)
	}

	c.Abort()
}

type ssePipeWriter struct {
	*io.PipeWriter
}

func (w *ssePipeWriter) Header() http.Header {
	return http.Header{}
}

func (w *ssePipeWriter) WriteHeader(int) {}

func (w *ssePipeWriter) Write(b []byte) (int, error) {
	return w.PipeWriter.Write(b)
}

func (w *ssePipeWriter) Flush() {}

func (w *ssePipeWriter) CloseNotify() <-chan bool {
	ch := make(chan bool, 1)
	return ch
}

func (w *ssePipeWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return nil, nil, http.ErrNotSupported
}

func (r *Router) handleIntercept(c *gin.Context) {
	defer func() {
		if rec := recover(); rec != nil {
			logger.Info("[intercept] %s connection closed", c.Request.URL.Path)
		}
	}()

	origWriter := c.Writer
	writer := proxy.NewInterceptWriter(origWriter, r.cfg.MaxInterceptBodySize)
	c.Writer = writer

	r.reverseProxy.ServeHTTP(writer, c.Request)
	logger.Info("[intercept] %s %s upstream done, status=%d, buf=%d, overflow=%v", c.Request.Method, c.Request.URL.Path, writer.Status(), writer.Size(), writer.Overflowed())

	if !writer.Overflowed() {
		original := writer.Buffer()
		filtered, summary := filter.ProcessResponse(
			c.Request.URL.Path,
			c.Request.Method,
			writer.Status(),
			original,
			r.rules,
		)
		c.Header("Content-Length", strconv.Itoa(len(filtered)))
		origWriter.WriteHeader(writer.Status())
		_, _ = origWriter.Write(filtered)
		logger.Info("[intercept] %s %s response written, filtered=%d→%d", c.Request.Method, c.Request.URL.Path, len(original), len(filtered))

		if summary != nil {
			entry := audit.NewAuditLog()
			entry.ApiPath = c.Request.URL.Path
			entry.Method = c.Request.Method
			entry.StatusCode = writer.Status()
			entry.Filtered = summary.Filtered
			entry.ToolsCount = summary.ToolsCount
			entry.CodeBlocksTotal = summary.CodeBlocksTotal
			entry.CodeBlocksFiltered = summary.CodeBlocksFiltered
			entry.Tools, entry.Files = buildAuditChildren(summary.ToolActions)
			entry.FilesCount = len(entry.Files)
			r.fillAuditEntry(c, entry)
			r.auditWorker.Send(entry)
		}
	} else {
		entry := &audit.AuditLog{
			ApiPath: c.Request.URL.Path,
			Method:  c.Request.Method,
		}
		r.fillAuditEntry(c, entry)
		r.auditWorker.Send(entry)
	}

	c.Abort()
}

func (r *Router) handleAuditOnly(c *gin.Context) {
	defer func() {
		if rec := recover(); rec != nil {
			logger.Info("[audit] %s connection closed", c.Request.URL.Path)
		}
	}()

	origWriter := c.Writer
	iw := proxy.NewInterceptWriter(origWriter, r.cfg.MaxInterceptBodySize)
	c.Writer = iw

	r.reverseProxy.ServeHTTP(iw, c.Request)
	logger.Info("[audit] %s %s upstream done, status=%d, buf=%d, overflow=%v", c.Request.Method, c.Request.URL.Path, iw.Status(), iw.Size(), iw.Overflowed())

	origWriter.WriteHeader(iw.Status())
	if !iw.Overflowed() {
		n, err := origWriter.Write(iw.Buffer())
		logger.Info("[audit] %s %s response written, bytes=%d, err=%v", c.Request.Method, c.Request.URL.Path, n, err)
	}

	entry := &audit.AuditLog{
		ApiPath: c.Request.URL.Path,
		Method:  c.Request.Method,
	}
	if len(iw.Buffer()) > 0 {
		body := iw.Buffer()
		if len(body) > 500 {
			body = body[:500]
		}
		entry.RequestSummary = string(body)
	}
	r.fillAuditEntry(c, entry)
	r.auditWorker.Send(entry)

	c.Abort()
}

func (r *Router) fillAuditEntry(c *gin.Context, entry *audit.AuditLog) {
	entry.UserID = c.GetString("user_id")
	entry.UserName = c.GetString("user_name")
	entry.UserSub = c.GetString("user_sub")
	entry.ClientIP = c.ClientIP()
	entry.ClientType = extractClientType(c.GetHeader("User-Agent"))
	entry.SessionID = c.Query("session_id")
	if entry.SessionID == "" {
		entry.SessionID = extractPathParam(c.Request.URL.Path, "sessions")
	}
	entry.ConversationID = extractPathParam(c.Request.URL.Path, "conversations")
}

type routeType int

const (
	routePassThrough routeType = iota
	routeIntercept
	routeAuditOnly
)

func classifyRoute(path, method string) routeType {
	if isSessionMessagesPath(path) {
		return routeIntercept
	}
	if isPromptPath(path) {
		return routeIntercept
	}
	if isShellPath(path) {
		return routeIntercept
	}
	if isAuditOnlyPath(path, method) {
		return routeAuditOnly
	}
	return routePassThrough
}

func isSSERequest(c *gin.Context, path string) bool {
	return strings.Contains(c.GetHeader("Accept"), "text/event-stream") ||
		strings.HasSuffix(path, "/events")
}

func isSessionMessagesPath(path string) bool {
	return strings.Contains(path, "/conversations/") &&
		(strings.HasSuffix(path, "/messages") || strings.HasSuffix(path, "/diff"))
}

func isRuntimeContentPath(path string) bool {
	return strings.Contains(path, "/runtime/files/content") ||
		strings.Contains(path, "/runtime/diff/content")
}

func isRuntimeTreePath(path string) bool {
	return strings.Contains(path, "/runtime/files") && !strings.Contains(path, "/runtime/files/content") && !strings.Contains(path, "/runtime/files/meta")
}

func isRuntimeDiffListPath(path string) bool {
	return strings.Contains(path, "/runtime/diff") && !strings.Contains(path, "/runtime/diff/content")
}

func isRuntimeFilePath(path string) bool {
	return strings.Contains(path, "/runtime/files/content") ||
		strings.Contains(path, "/runtime/diff/content")
}

func isPromptPath(path string) bool {
	return strings.Contains(path, "/conversations/") && strings.HasSuffix(path, "/prompt")
}

func isShellPath(path string) bool {
	return strings.Contains(path, "/conversations/") && strings.HasSuffix(path, "/shell")
}

func isTerminalPath(path string) bool {
	idx := strings.Index(path, "/terminal")
	if idx == -1 {
		return false
	}
	rest := path[idx+len("/terminal"):]
	return rest == "" || rest[0] == '/' || rest[0] == '?'
}

func isUpgrade(c *gin.Context) bool {
	return strings.ToLower(c.GetHeader("Connection")) == "upgrade" &&
		strings.ToLower(c.GetHeader("Upgrade")) == "websocket"
}

func isDisabledPath(path, method string) bool {
	return strings.Contains(path, "/terminal/input-ws")
}

func isAuditOnlyPath(path, method string) bool {
	if strings.Contains(path, "/runtime/files/content") && method == "PUT" {
		return true
	}
	if strings.Contains(path, "/conversations/") && strings.HasSuffix(path, "/command") {
		return true
	}
	if strings.HasSuffix(path, "/permissions") {
		return true
	}
	if strings.HasSuffix(path, "/questions") {
		return true
	}
	if strings.Contains(path, "/conversations/") && strings.HasSuffix(path, "/todo") {
		return true
	}
	if strings.Contains(path, "/conversations/") && strings.HasSuffix(path, "/tasks") {
		return true
	}
	return false
}

func extractClientType(ua string) string {
	ua = strings.ToLower(ua)
	if strings.Contains(ua, "mobile") || strings.Contains(ua, "iphone") || strings.Contains(ua, "android") {
		return "mobile"
	}
	if strings.Contains(ua, "bot") || strings.Contains(ua, "crawl") || strings.Contains(ua, "spider") {
		return "bot"
	}
	return "desktop"
}

func extractPathParam(path, prefix string) string {
	parts := strings.Split(path, "/")
	for i, p := range parts {
		if p == prefix && i+1 < len(parts) {
			id := parts[i+1]
			if id != "" && !strings.Contains(id, "?") {
				return id
			}
		}
	}
	return ""
}

func buildAuditChildren(actions []filter.FilterAction) ([]audit.AuditTool, []audit.AuditFile) {
	toolSeen := make(map[string]bool)
	fileSeen := make(map[string]bool)
	var tools []audit.AuditTool
	var files []audit.AuditFile

	for _, a := range actions {
		if a.ToolName != "" && !toolSeen[a.ToolName+a.Input] {
			toolSeen[a.ToolName+a.Input] = true
			tools = append(tools, audit.AuditTool{
				ToolName: a.ToolName,
				Input:    a.Input,
				Filtered: a.Filtered,
			})
		}
		if a.Path != "" {
			accessType := fileAccessType(a.ToolName)
			key := a.Path + "|" + accessType
			if !fileSeen[key] {
				fileSeen[key] = true
				files = append(files, audit.AuditFile{
					FilePath:   a.Path,
					AccessType: accessType,
				})
			}
		}
	}
	return tools, files
}

func fileAccessType(toolName string) string {
	switch toolName {
	case "write", "edit", "write_file", "file_write", "edit_file", "file_edit", "apply_patch":
		return "write"
	default:
		return "read"
	}
}
