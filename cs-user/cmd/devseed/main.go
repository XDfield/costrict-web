// Command devseed applies cs-user/scripts/dev-seed.sql against the cs_user
// Postgres database. Intended for local E2E setup — see
// docs/repo-management/E2E_TESTING.md.
//
// Env vars (mirror cs-user's own .env):
//
//	CS_USER_POSTGRES_HOST (default 127.0.0.1)
//	CS_USER_POSTGRES_PORT (default 5432)
//	CS_USER_POSTGRES_DATABASE (default cs_user)
//	CS_USER_POSTGRES_USER (default costrict)
//	CS_USER_POSTGRES_PASSWORD (required)
//	CS_USER_POSTGRES_SSLMODE (default disable)
//	DEV_SEED_GITEA_TOKEN (required — injected into git_servers.config.admin_token)
//	DEV_SEED_GITEA_ENDPOINT (default http://127.0.0.1:3001)
//	DEV_SEED_GITEA_ADMIN_USER (required — injected into git_servers.config.admin_user;
//	    needed because Gitea's /users/{name}/tokens endpoint sits behind
//	    reqBasicOrRevProxyAuth and rejects admin PAT)
//	DEV_SEED_GITEA_ADMIN_PASSWORD (required — injected into git_servers.config.admin_password)
//	DEV_SEED_SQL_PATH (default scripts/dev-seed.sql, relative to cs-user/)
//
// Exit code 0 on success, 1 on any failure.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "devseed: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	host := envOr("CS_USER_POSTGRES_HOST", "127.0.0.1")
	port := envOr("CS_USER_POSTGRES_PORT", "5432")
	dbName := envOr("CS_USER_POSTGRES_DATABASE", "cs_user")
	user := envOr("CS_USER_POSTGRES_USER", "costrict")
	pass := os.Getenv("CS_USER_POSTGRES_PASSWORD")
	if pass == "" {
		return fmt.Errorf("CS_USER_POSTGRES_PASSWORD env var required")
	}
	sslmode := envOr("CS_USER_POSTGRES_SSLMODE", "disable")

	token := os.Getenv("DEV_SEED_GITEA_TOKEN")
	if token == "" {
		return fmt.Errorf("DEV_SEED_GITEA_TOKEN env var required (admin PAT from local Gitea)")
	}
	adminUser := os.Getenv("DEV_SEED_GITEA_ADMIN_USER")
	if adminUser == "" {
		return fmt.Errorf("DEV_SEED_GITEA_ADMIN_USER env var required (Basic-auth for /users/{name}/tokens)")
	}
	adminPass := os.Getenv("DEV_SEED_GITEA_ADMIN_PASSWORD")
	if adminPass == "" {
		return fmt.Errorf("DEV_SEED_GITEA_ADMIN_PASSWORD env var required (Basic-auth for /users/{name}/tokens)")
	}
	endpoint := envOr("DEV_SEED_GITEA_ENDPOINT", "http://127.0.0.1:3001")
	sqlPath := envOr("DEV_SEED_SQL_PATH", "scripts/dev-seed.sql")

	raw, err := os.ReadFile(sqlPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", sqlPath, err)
	}
	sql := string(raw)
	sql = strings.ReplaceAll(sql, "<REPLACE_WITH_LOCAL_PAT>", token)
	sql = strings.ReplaceAll(sql, "<REPLACE_WITH_GITEA_ADMIN_USER>", adminUser)
	sql = strings.ReplaceAll(sql, "<REPLACE_WITH_GITEA_ADMIN_PASSWORD>", adminPass)
	sql = strings.ReplaceAll(sql, "http://127.0.0.1:3000", endpoint)

	dsn := fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=%s",
		user, pass, host, port, dbName, sslmode)

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer pool.Close()

	// dev-seed.sql wraps everything in a single BEGIN/COMMIT, so one Exec is
	// atomic. Split on semicolons is unsafe (function bodies, etc.) — just
	// send the whole thing as one query; pgx + Postgres handle multi-statement
	// fine when there's no bind parameters.
	if _, err := pool.Exec(ctx, sql); err != nil {
		return fmt.Errorf("exec seed: %w", err)
	}

	// Sanity probe.
	var (
		tenantID    string
		gitServerID *string
		gitKind     string
		gitEndpoint string
		gitEnabled  bool
		userSubject string
		empNo       string
	)
	if err := pool.QueryRow(ctx,
		`SELECT t.tenant_id, t.git_server_id FROM tenants t WHERE t.tenant_id='tenant-e2e'`,
	).Scan(&tenantID, &gitServerID); err != nil {
		return fmt.Errorf("verify tenant: %w", err)
	}
	if gitServerID == nil || *gitServerID == "" {
		return fmt.Errorf("tenant-e2e.git_server_id is NULL — seed did not bind")
	}
	if err := pool.QueryRow(ctx,
		`SELECT kind, endpoint, enabled FROM git_servers WHERE server_id=$1`, *gitServerID,
	).Scan(&gitKind, &gitEndpoint, &gitEnabled); err != nil {
		return fmt.Errorf("verify git_server: %w", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT subject_id FROM users WHERE subject_id='usr_e2e_1'`,
	).Scan(&userSubject); err != nil {
		return fmt.Errorf("verify user: %w", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT employee_number FROM employment_identities WHERE user_subject_id='usr_e2e_1' AND employee_number='E001'`,
	).Scan(&empNo); err != nil {
		return fmt.Errorf("verify employment_identity: %w", err)
	}

	fmt.Printf("✓ tenant %s bound to git_server=%s (kind=%s endpoint=%s enabled=%v)\n",
		tenantID, *gitServerID, gitKind, gitEndpoint, gitEnabled)
	fmt.Printf("✓ user %s with employee_number=%s\n", userSubject, empNo)
	fmt.Printf("✓ seed applied (gitea endpoint=%s, token redacted)\n", endpoint)
	return nil
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}
