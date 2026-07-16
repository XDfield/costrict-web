// RPCWriter implements UserWriter by calling the cs-user microservice's Phase 2
// write API over HTTP. It is the write-side counterpart to RPCClient and is
// selected by USER_SERVICE_WRITE_MODE=readonly with USER_SERVICE_BACKEND=rpc
// (single-write posture: cs-user is the sole write authority). It is also the
// Secondary inside DualWriter for the rpc+local canary.
//
// cs-user authenticates server-to-server traffic with the same X-Internal-Token
// header as the read path; the writer reuses RPCClient's configuration (base
// URL, token, timeout, ErrRPCUnavailable mapping) via an embedded *RPCClient.
//
// Write methods route through a dedicated doCapture helper (instead of
// RPCClient.do) because cs-user returns same-status-different-meaning
// responses that handlers must distinguish:
//
//   - cs-user ErrExplicitlyUnbound ("identity explicitly unbound; requires
//     force_rebind") is HTTP 409. The local writer treats this case as a no-op
//     (returns nil) at service.go:290, so the RPC writer does the same —
//     returning nil lets the OAuth bind callback proceed without surfacing a
//     spurious conflict.
//   - cs-user ErrIdentityAlreadyBound ("identity already bound to another
//     user") is HTTP 409. The local writer returns the bare token
//     "identity_already_bound" (service.go:273) and the bind callback handler
//     matches on that exact string (handlers.go:566) to redirect to the
//     merge-identity flow. The RPC writer rewrites the cs-user message to the
//     server token so the handler's substring match keeps working.
//   - cs-user surfaces other 4xx errors via `{"error": "..."}` envelopes whose
//     message strings already match what server handlers expect (e.g.
//     "identity_not_found", "cannot unbind last identity", "identity not
//     found"). mapWriteError extracts that string and returns it verbatim.
//
// 5xx responses and transport failures map to ErrRPCUnavailable, mirroring the
// read path.
package user

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/costrict/costrict-web/server/internal/config"
	"github.com/costrict/costrict-web/server/internal/logger"
	"github.com/costrict/costrict-web/server/internal/models"
)

// cs-user error body substrings that RPCWriter must normalize or downcast.
// Matched case-insensitively against the response body so minor wording
// changes (e.g. trailing punctuation) don't break the contract.
const (
	// cs-user returns this when an identity is soft-deleted-but-explicitly-
	// unbound and ForceRebind is false. Server's local writer treats this as
	// a no-op (nil) — see service.go:290.
	csUserExplicitlyUnbound = "explicitly unbound"
	// cs-user returns this when an identity already belongs to another user.
	// Server's local writer returns the bare token "identity_already_bound"
	// (service.go:273) which handlers.go:566 matches on exactly.
	csUserIdentityAlreadyBound      = "identity already bound"
	serverIdentityAlreadyBoundToken = "identity_already_bound"
)

// RPCWriter talks to cs-user's Phase 2 write API. Construct with NewRPCWriter.
// Embeds an *RPCClient to reuse its config + HTTP client; the wire format
// (auth header, timeout, ErrRPCUnavailable mapping) is identical between read
// and write paths.
type RPCWriter struct {
	client *RPCClient
}

// NewRPCWriter builds an RPCWriter from config. The underlying RPCClient is
// constructed exactly as NewRPCClient would; methods return ErrNotConfigured
// when Configured() is false (i.e. URL or token missing), so this constructor
// never fails.
func NewRPCWriter(cfg config.UserServiceConfig) *RPCWriter {
	return &RPCWriter{client: NewRPCClient(cfg)}
}

// Configured reports whether both baseURL and internalToken are set. Delegates
// to the underlying RPCClient so a single source of truth governs readiness
// for both reads and writes.
func (w *RPCWriter) Configured() bool {
	return w.client.Configured()
}

// GetOrCreateUser calls POST /api/internal/users/get-or-create with the bare
// JWTClaims as the body. cs-user's response is the upserted user; the
// post-login hook fires in the caller (UserService or DualWriter), never
// here — RPCWriter is purely a transport.
func (w *RPCWriter) GetOrCreateUser(claims *JWTClaims) (*models.User, error) {
	if claims == nil {
		return nil, errors.New("nil JWT claims")
	}
	if !w.Configured() {
		return nil, ErrNotConfigured
	}
	body, err := json.Marshal(claims)
	if err != nil {
		return nil, fmt.Errorf("user rpc writer: marshal get-or-create request: %w", err)
	}
	status, respBody, transportErr := w.doCapture(context.Background(), http.MethodPost, "/api/internal/users/get-or-create", body)
	if transportErr != nil {
		return nil, transportErr
	}
	if status >= 200 && status < 300 {
		var u models.User
		if err := json.Unmarshal(respBody, &u); err != nil {
			return nil, fmt.Errorf("user rpc writer: decode get-or-create response: %w", err)
		}
		return &u, nil
	}
	return nil, w.mapWriteError(status, respBody, "get-or-create")
}

// SyncUser calls the same endpoint as GetOrCreateUser. cs-user has a single
// upsert path; the "sync vs create" distinction lives server-side (whether to
// fire the post-login hook). SyncUser's caller (user-search backfill) routes
// through Module.Writer, which selects RPCWriter in readonly mode without
// firing the hook — so this method is literally GetOrCreateUser minus the
// caller-side hook trigger.
func (w *RPCWriter) SyncUser(claims *JWTClaims) (*models.User, error) {
	return w.GetOrCreateUser(claims)
}

// BindIdentityToUser calls POST /api/internal/users/:subject_id/bind-identity
// with the bind request envelope {claims, options}. Translates the two
// divergent cs-user 409 responses per the package doc:
//   - explicitly_unbound → nil (no-op, matches server's local writer)
//   - identity already bound → error with the server-side "identity_already_bound" token
//
// Other non-2xx responses flow through mapWriteError.
func (w *RPCWriter) BindIdentityToUser(userSubjectID string, claims *JWTClaims, opts ...BindIdentityOptions) error {
	if !w.Configured() {
		return ErrNotConfigured
	}
	var opt BindIdentityOptions
	if len(opts) > 0 {
		opt = opts[0]
	}
	body := struct {
		Claims  *JWTClaims           `json:"claims"`
		Options *BindIdentityOptions `json:"options,omitempty"`
	}{Claims: claims}
	if opt.ForceRebind || opt.UpdateLastLogin {
		optCopy := opt
		body.Options = &optCopy
	}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("user rpc writer: marshal bind request: %w", err)
	}
	path := "/api/internal/users/" + url.PathEscape(userSubjectID) + "/bind-identity"
	status, respBody, transportErr := w.doCapture(context.Background(), http.MethodPost, path, bodyBytes)
	if transportErr != nil {
		return transportErr
	}
	if status >= 200 && status < 300 {
		return nil
	}
	if status == http.StatusConflict {
		msg, _ := parseErrorBody(respBody)
		low := strings.ToLower(msg)
		switch {
		case strings.Contains(low, csUserExplicitlyUnbound):
			// Match server's local writer: no-op success when the identity
			// is explicitly unbound and ForceRebind is false.
			return nil
		case strings.Contains(low, csUserIdentityAlreadyBound):
			// Rewrite to server's local-writer token so handlers.go:566
			// redirects to the merge-identity flow on the exact match.
			return errors.New(serverIdentityAlreadyBoundToken)
		}
	}
	return w.mapWriteError(status, respBody, "bind-identity")
}

// TransferIdentityToUser calls POST /api/internal/users/transfer-identity with
// the transfer request envelope. sourceUserSubjectID is forwarded for
// forwards-compatibility; cs-user currently identifies the identity purely by
// external_key.
func (w *RPCWriter) TransferIdentityToUser(targetUserSubjectID, externalKey, sourceUserSubjectID string) error {
	if !w.Configured() {
		return ErrNotConfigured
	}
	body := struct {
		TargetUserSubjectID string `json:"target_user_subject_id"`
		ExternalKey         string `json:"external_key"`
		SourceUserSubjectID string `json:"source_user_subject_id,omitempty"`
	}{TargetUserSubjectID: targetUserSubjectID, ExternalKey: externalKey, SourceUserSubjectID: sourceUserSubjectID}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("user rpc writer: marshal transfer request: %w", err)
	}
	status, respBody, transportErr := w.doCapture(context.Background(), http.MethodPost, "/api/internal/users/transfer-identity", bodyBytes)
	if transportErr != nil {
		return transportErr
	}
	if status >= 200 && status < 300 {
		return nil
	}
	return w.mapWriteError(status, respBody, "transfer-identity")
}

// UnbindIdentityByProvider calls DELETE
// /api/internal/users/:subject_id/identities/:provider. No body. cs-user's 4xx
// error messages ("cannot unbind last identity", "identity not found") already
// match what handlers.go:766 expects, so mapWriteError surfaces them verbatim.
func (w *RPCWriter) UnbindIdentityByProvider(userSubjectID, provider string) error {
	if !w.Configured() {
		return ErrNotConfigured
	}
	path := "/api/internal/users/" + url.PathEscape(userSubjectID) + "/identities/" + url.PathEscape(provider)
	status, respBody, transportErr := w.doCapture(context.Background(), http.MethodDelete, path, nil)
	if transportErr != nil {
		return transportErr
	}
	if status >= 200 && status < 300 {
		return nil
	}
	return w.mapWriteError(status, respBody, "unbind-identity")
}

// doCapture issues an authenticated request and returns (status, body,
// transportError). Unlike RPCClient.do, it does NOT decode the body into a
// model — callers handle decoding on success — and does NOT collapse non-2xx
// responses into errors. The write paths need access to the raw body to
// substring-match cs-user's error envelope, and they need the status code to
// drive per-method normalization. Transport failures, timeouts, and 5xx
// surfaces are still mapped to ErrRPCUnavailable for handler-level 503s.
//
// This helper is intentionally a method on RPCWriter (not RPCClient) so the
// read path's do() — which is strict about non-2xx → error — stays untouched.
func (w *RPCWriter) doCapture(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, w.client.httpClient.Timeout)
	defer cancel()

	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, w.client.baseURL+path, bodyReader)
	if err != nil {
		return 0, nil, fmt.Errorf("user rpc writer: build request: %w", err)
	}
	req.Header.Set("X-Internal-Token", w.client.internalToken)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := w.client.httpClient.Do(req)
	if err != nil {
		logger.Warn("[user-rpc-writer] %s %s request failed: %v", method, path, err)
		return 0, nil, fmt.Errorf("%w: %v", ErrRPCUnavailable, err)
	}
	defer resp.Body.Close()
	respBody, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		logger.Warn("[user-rpc-writer] %s %s read body failed: %v", method, path, readErr)
		return 0, nil, fmt.Errorf("%w: read body: %v", ErrRPCUnavailable, readErr)
	}
	return resp.StatusCode, respBody, nil
}

// mapWriteError is the default non-2xx response handler. It surfaces cs-user's
// JSON error string verbatim (so handlers that match on err.Error() substrings
// keep working), and maps 5xx + transport failures to ErrRPCUnavailable for
// clean 503s at the handler layer.
func (w *RPCWriter) mapWriteError(status int, respBody []byte, op string) error {
	if status >= 500 {
		logger.Warn("[user-rpc-writer] %s returned status %d: %s", op, status, truncate(string(respBody), 200))
		return fmt.Errorf("%w: status %d", ErrRPCUnavailable, status)
	}
	if msg, ok := parseErrorBody(respBody); ok {
		// cs-user's 4xx error strings are the contract — surface them
		// verbatim so handlers' substring matches keep working.
		return errors.New(msg)
	}
	logger.Warn("[user-rpc-writer] %s returned status %d: %s", op, status, truncate(string(respBody), 200))
	return fmt.Errorf("user rpc writer: %s: status %d", op, status)
}

// parseErrorBody extracts the "error" field from a cs-user JSON error envelope
// `{"error": "..."}`. Returns ("", false) if the body is missing, not JSON,
// or has an empty error field — caller then falls back to a status-only error.
func parseErrorBody(body []byte) (string, bool) {
	if len(body) == 0 {
		return "", false
	}
	var env struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return "", false
	}
	if env.Error == "" {
		return "", false
	}
	return env.Error, true
}
