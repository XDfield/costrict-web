// Command user-provision-backfill is a one-shot operator CLI that reconciles
// existing cs-user users against user_git_binding for one tenant. Used to
// migrate users created before USER_CREATED_EVENT_PROCESSING_ENABLED was
// turned on — the "存量用户开户" path.
//
// Why a standalone binary vs. the POST /api/admin/users/provision/backfill
// HTTP endpoint (which already exists in handlers/backfill.go):
//
//   - The HTTP endpoint lives behind the platform-admin auth layer and is
//     good for ad-hoc operator triggers from the admin console. The CLI
//     targets the cut-over scenario where an SRE has DB + cs-user internal
//     token access on a jumpbox and wants a logged, repeatable, scriptable
//     run without minting a platform-admin JWT.
//   - The CLI shares the exact same code path
//     (gitsync.UserProvisionService.BackfillMissingBindings), so behaviour
//     is identical — just a different entry point for a different context.
//
// Connection priority mirrors gitserver-config / sqlexec:
//
//	-dsn flag  >  DATABASE_URL  >  .env (via internal/config.Load)
//
// cs-user connection comes from USER_SERVICE_URL + USER_SERVICE_INTERNAL_TOKEN
// (loaded by internal/config.Load). Both must be set.
//
// Usage:
//
//	# Dry-run preview (no writes): show who would be provisioned
//	go run ./server/cmd/user-provision-backfill -tenant acme -dry-run
//
//	# Real backfill with confirmation prompt
//	go run ./server/cmd/user-provision-backfill -tenant acme
//
//	# Scriptable / unattended
//	go run ./server/cmd/user-provision-backfill -tenant acme -max-users 1000 -y
//
// Exit codes:
//
//	0 — backfill completed (failures may still be reported per-user)
//	1 — fatal misconfiguration / DB or RPC unreachable
//	2 — operator aborted at the confirmation prompt
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/costrict/costrict-web/server/internal/config"
	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/gitserver"
	"github.com/costrict/costrict-web/server/internal/gitsync"
	"github.com/costrict/costrict-web/server/internal/tenant"
	userpkg "github.com/costrict/costrict-web/server/internal/user"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

const (
	defaultPageSize = 100
	defaultMaxUsers = 500
	hardMaxPageSize = 500
	hardMaxUsers    = 2000
)

func main() {
	tenantID := flag.String("tenant", "", "tenant_id whose users to reconcile (required)")
	tenantSlug := flag.String("tenant-slug", "", "X-Tenant-Id slug forwarded to cs-user ListUsers (defaults to -tenant)")
	dsnFlag := flag.String("dsn", "", "PostgreSQL DSN (overrides .env / DATABASE_URL)")
	pageSize := flag.Int("page-size", defaultPageSize, "cs-user ListUsers page size (hard cap 500)")
	maxUsers := flag.Int("max-users", defaultMaxUsers, "ceiling on users processed in this run (hard cap 2000)")
	updateDisplayName := flag.Bool("update-display-name", false, "push cs-user display_name to existing Gitea users (one extra PATCH /admin/users/:username per synced user)")
	dryRun := flag.Bool("dry-run", false, "preview only — classify users without calling ProvisionUser")
	yes := flag.Bool("y", false, "skip confirmation prompt")
	flag.Parse()

	if *tenantID == "" {
		log.Fatal("-tenant is required")
	}
	if *tenantSlug == "" {
		*tenantSlug = *tenantID
	}
	*pageSize = clamp(*pageSize, 1, hardMaxPageSize, defaultPageSize)
	*maxUsers = clamp(*maxUsers, 1, hardMaxUsers, defaultMaxUsers)

	cfg := config.Load()
	dsn := *dsnFlag
	if dsn == "" {
		dsn = cfg.DatabaseURL
	}
	if dsn == "" {
		log.Fatal("no DSN: pass -dsn, set DATABASE_URL, or configure .env")
	}
	if cfg.UserService.BaseURL == "" || cfg.UserService.InternalToken == "" {
		log.Fatal("cs-user RPC not configured: set USER_SERVICE_URL and USER_SERVICE_INTERNAL_TOKEN")
	}

	db, err := database.Initialize(dsn)
	if err != nil {
		log.Fatalf("connect failed: %v", err)
	}

	rpc := userpkg.NewRPCClient(cfg.UserService)
	if !rpc.Configured() {
		log.Fatal("user rpc client not configured")
	}

	// tenant.WithSlug injects the X-Tenant-Id header the RPC client forwards
	// to cs-user, scoping ListUsers to this tenant.
	ctx := tenant.WithSlug(context.Background(), *tenantSlug)

	users, err := collectUsers(ctx, rpc, *pageSize, *maxUsers)
	if err != nil {
		log.Fatalf("list users: %v", err)
	}
	fmt.Printf("fetched %d users from cs-user (tenant-slug=%q)\n", len(users), *tenantSlug)

	if *dryRun {
		preview(db, *tenantID, users, *updateDisplayName)
		return
	}

	// Operator confirmation — backfill makes real Git API calls.
	fmt.Printf("\nAbout to backfill %d users into tenant %q (page_size=%d, max_users=%d, update_display_name=%t).\n",
		len(users), *tenantID, *pageSize, *maxUsers, *updateDisplayName)
	if !*yes {
		fmt.Print("Proceed? [y/N] ")
		var resp string
		fmt.Fscanln(os.Stdin, &resp)
		if resp != "y" && resp != "Y" && resp != "yes" {
			fmt.Println("aborted by operator")
			os.Exit(2)
		}
	}

	resolver := gitserver.NewDBResolver(db)
	provSvc := gitsync.NewUserProvisionService(db, resolver, zap.NewNop())

	start := time.Now()
	result := provSvc.BackfillMissingBindings(ctx, *tenantID, toBackfillUsers(users), gitsync.BackfillOptions{
		UpdateDisplayName: *updateDisplayName,
	})
	elapsed := time.Since(start).Round(time.Millisecond)

	fmt.Println("\n=== backfill result ===")
	fmt.Printf("tenant              : %s\n", *tenantID)
	fmt.Printf("total               : %d\n", result.Total)
	fmt.Printf("already_bound       : %d\n", result.AlreadyBound)
	fmt.Printf("provisioned         : %d\n", result.Provisioned)
	fmt.Printf("display_name_updated: %d\n", result.DisplayNameUpdated)
	fmt.Printf("skipped             : %d (no ShortID)\n", result.Skipped)
	fmt.Printf("failed              : %d\n", result.Failed)
	fmt.Printf("elapsed             : %s\n", elapsed)
	if len(result.Failures) > 0 {
		fmt.Println("\nfailures:")
		for _, f := range result.Failures {
			fmt.Printf("  - %s : %s\n", f.SubjectID, f.Error)
		}
		fmt.Printf("\nRe-run after fixing root cause; already-synced users will be skipped (idempotent).\n")
	}
}

// collectUsers pages through cs-user ListUsers until a page returns short of
// pageSize (end of list) or the max_users ceiling is hit.
func collectUsers(ctx context.Context, rpc *userpkg.RPCClient, pageSize, maxUsers int) ([]userpkg.AdminUser, error) {
	var out []userpkg.AdminUser
	page := 1
	for len(out) < maxUsers {
		result, err := rpc.ListUsers(ctx, userpkg.AdminUserListParams{Page: page, PageSize: pageSize})
		if err != nil {
			return nil, fmt.Errorf("page %d: %w", page, err)
		}
		if len(result.Users) == 0 {
			break
		}
		out = append(out, result.Users...)
		if len(result.Users) < pageSize {
			break
		}
		page++
	}
	if len(out) > maxUsers {
		out = out[:maxUsers]
	}
	return out, nil
}

// preview classifies the fetched users against current bindings and prints
// a summary without invoking ProvisionUser. Mirrors BackfillMissingBindings'
// partitioning so what dry-run reports matches what the real run will do.
func preview(db *gorm.DB, tenantID string, users []userpkg.AdminUser, updateDisplayName bool) {
	var rows []struct {
		UserSubjectID string
		SyncStatus    string
	}
	res := db.Table("user_git_binding").
		Select("user_subject_id, sync_status").
		Where("tenant_id = ?", tenantID).
		Find(&rows)
	if res.Error != nil {
		log.Fatalf("preview: query bindings: %v", res.Error)
	}
	bound := make(map[string]string, len(rows))
	for _, r := range rows {
		bound[r.UserSubjectID] = r.SyncStatus
	}

	var synced, pending, provision, skipped, displayNameCandidates int
	var provisionExamples []string
	for _, u := range users {
		switch {
		case u.SubjectID == "" || u.ShortID == "":
			skipped++
		case bound[u.SubjectID] == "synced":
			synced++
			if updateDisplayName && u.DisplayName != nil && strings.TrimSpace(*u.DisplayName) != "" {
				displayNameCandidates++
			}
		case bound[u.SubjectID] == "pending", bound[u.SubjectID] == "error":
			pending++
			provision++
		default:
			provision++
			if len(provisionExamples) < 5 {
				provisionExamples = append(provisionExamples, u.SubjectID+" (→ "+u.ShortID+")")
			}
		}
	}

	fmt.Println("\n=== dry-run preview ===")
	fmt.Printf("total                       : %d\n", len(users))
	fmt.Printf("already synced              : %d\n", synced)
	fmt.Printf("pending/error               : %d (will be retried)\n", pending)
	fmt.Printf("would provision             : %d\n", provision)
	fmt.Printf("skipped                     : %d (no ShortID)\n", skipped)
	if updateDisplayName {
		fmt.Printf("display_name update targets : %d (synced users with non-empty cs-user display_name)\n", displayNameCandidates)
	} else {
		fmt.Printf("display_name update targets : 0 (-update-display-name=false)\n")
	}
	if len(provisionExamples) > 0 {
		fmt.Println("\nfirst would-provision users:")
		for _, ex := range provisionExamples {
			fmt.Println("  - " + ex)
		}
	}
	fmt.Println("\nNo writes performed. Re-run without -dry-run to apply.")
}

// toBackfillUsers maps cs-user's AdminUser projection into the
// gitsync.BackfillUser input shape BackfillMissingBindings expects.
func toBackfillUsers(users []userpkg.AdminUser) []gitsync.BackfillUser {
	out := make([]gitsync.BackfillUser, 0, len(users))
	for _, u := range users {
		out = append(out, gitsync.BackfillUser{
			SubjectID:   u.SubjectID,
			ShortID:     u.ShortID,
			Username:    u.Username,
			DisplayName: u.DisplayName,
			Email:       u.Email,
		})
	}
	return out
}

// clamp bounds v to [lo, hi]; zero/out-of-range yields def.
func clamp(v, lo, hi, def int) int {
	if v < lo {
		return def
	}
	if v > hi {
		return hi
	}
	return v
}
