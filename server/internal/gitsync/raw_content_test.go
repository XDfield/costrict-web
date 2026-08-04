package gitsync

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestClient_ReadRawFile_ReturnsBytesVerbatim(t *testing.T) {
	const body = "---\nname: demo\n---\n\n# Body\n\x00binary-ish"
	var gotPath, gotRef, gotAuth string
	srv := newDispatchServer(t, dispatch{
		"GET /api/v1/repos/alice/skills/*": func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			gotRef = r.URL.Query().Get("ref")
			gotAuth = r.Header.Get("Authorization")
			_, _ = w.Write([]byte(body))
		},
	})
	c := newClientWithHTTPC(srv.URL, "tok", srv.Client())

	raw, err := c.ReadRawFile(context.Background(), "alice", "skills", "main", "skills/demo/skill.md")
	if err != nil {
		t.Fatalf("ReadRawFile: %v", err)
	}
	if string(raw) != body {
		t.Fatalf("payload was not returned verbatim: %q", raw)
	}
	// Path separators must survive: the file is nested, and a whole-path escape
	// would collapse them into %2F.
	if gotPath != "/api/v1/repos/alice/skills/raw/skills/demo/skill.md" {
		t.Fatalf("unexpected request path %q", gotPath)
	}
	if gotRef != "main" {
		t.Fatalf("ref not sent: %q", gotRef)
	}
	if gotAuth != "token tok" {
		t.Fatalf("request was not authenticated: %q", gotAuth)
	}
}

// A missing file must be an error, not (nil, nil): the caller serves this to
// end users, where "gone" and "empty" mean different things.
func TestClient_ReadRawFile_MissingFileIsAnError(t *testing.T) {
	srv := newDispatchServer(t, dispatch{
		"GET /api/v1/repos/alice/skills/*": func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
		},
	})
	c := newClientWithHTTPC(srv.URL, "tok", srv.Client())

	raw, err := c.ReadRawFile(context.Background(), "alice", "skills", "main", "skill.md")
	if !errors.Is(err, ErrGiteaNotFound) {
		t.Fatalf("expected a not-found error, got %v", err)
	}
	if raw != nil {
		t.Fatalf("expected no payload on 404, got %q", raw)
	}
}

func TestClient_ReadRawFile_RejectsOversizedFile(t *testing.T) {
	srv := newDispatchServer(t, dispatch{
		"GET /api/v1/repos/alice/skills/*": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write(make([]byte, maxRawFileBytes+1))
		},
	})
	c := newClientWithHTTPC(srv.URL, "tok", srv.Client())

	if _, err := c.ReadRawFile(context.Background(), "alice", "skills", "main", "huge.bin"); err == nil ||
		!strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("expected an oversize error, got %v", err)
	}
}

func TestClient_ReadRawFile_RequiresEveryCoordinate(t *testing.T) {
	c := newClientWithHTTPC("http://x", "tok", nil)
	for _, args := range [][4]string{
		{"", "skills", "main", "skill.md"},
		{"alice", "", "main", "skill.md"},
		{"alice", "skills", "", "skill.md"},
		{"alice", "skills", "main", ""},
	} {
		if _, err := c.ReadRawFile(context.Background(), args[0], args[1], args[2], args[3]); err == nil {
			t.Fatalf("expected an error for %v", args)
		}
	}
}
