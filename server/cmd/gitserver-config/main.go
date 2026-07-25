// Command gitserver-config is a one-shot operator CLI for inspecting and
// patching the git_server config bound to a specific tenant.
//
// Why a standalone binary vs. an HTTP endpoint:
//   - admin_user/admin_password are operator-supplied secrets; the API
//     surface would need an extra auth scope + audit trail we don't have yet.
//   - The CLI runs from a dev/ops box with DB access already, side-stepping
//     the auth layer. Mirrors cs-user's dbprobe / etl pattern.
//
// Usage:
//
//	# Show resolved config for a tenant (passwords redacted by default)
//	go run ./server/cmd/gitserver-config -tenant acme -mode show
//
//	# Patch admin_user + admin_password (the most common repair — fixes
//	# ErrGiteaBasicAuthRequired after KBEnsure auto-provision races)
//	go run ./server/cmd/gitserver-config -tenant acme -mode update \
//	    -admin-user gitea-admin -admin-password '$3cr3t' -y
//
// Connection priority mirrors sqlexec: -dsn flag > DATABASE_URL > .env file
// (via internal/config.Load).
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/costrict/costrict-web/server/internal/config"
	"github.com/costrict/costrict-web/server/internal/database"
	"gorm.io/gorm"
)

// gitServerConfigJSON mirrors git_servers.config shape 1:1. Kept local (not
// imported from internal/gitserver) so this binary stays decoupled from
// internal package reshuffles — operators can run it against any version
// of the schema without rebuild churn.
type gitServerConfigJSON struct {
	AdminToken    string `json:"admin_token,omitempty"`
	AdminUser     string `json:"admin_user,omitempty"`
	AdminPassword string `json:"admin_password,omitempty"`
}

type gitServerRow struct {
	ServerID    string
	IsTemplate  bool
	Kind        string
	Endpoint    string
	DisplayName string
	Config      string
	Enabled     bool
}

func main() {
	mode := flag.String("mode", "show", "show | update")
	dsnFlag := flag.String("dsn", "", "PostgreSQL DSN (overrides .env / DATABASE_URL)")
	tenant := flag.String("tenant", "", "tenant_id whose bound git_server to inspect/patch (required)")

	// Mutation flags — empty means "leave unchanged".
	adminUser := flag.String("admin-user", "", "set config.admin_user")
	adminPassword := flag.String("admin-password", "", "set config.admin_password")
	adminToken := flag.String("admin-token", "", "set config.admin_token")
	endpoint := flag.String("endpoint", "", "set git_servers.endpoint")
	displayName := flag.String("display-name", "", "set git_servers.display_name")
	enable := flag.Bool("enable", false, "set enabled=true (overrides -disable)")
	disable := flag.Bool("disable", false, "set enabled=false")
	reveal := flag.Bool("reveal", false, "show mode only — do NOT redact secrets in output")
	yes := flag.Bool("y", false, "skip confirmation prompt in update mode")
	flag.Parse()

	if *tenant == "" {
		log.Fatal("-tenant is required")
	}

	cfg := config.Load()
	dsn := *dsnFlag
	if dsn == "" {
		dsn = cfg.DatabaseURL
	}
	if dsn == "" {
		log.Fatal("no DSN: pass -dsn, set DATABASE_URL, or configure .env")
	}

	db, err := database.Initialize(dsn)
	if err != nil {
		log.Fatalf("connect failed: %v", err)
	}

	switch *mode {
	case "show":
		if err := runShow(db, *tenant, *reveal); err != nil {
			log.Fatalf("show: %v", err)
		}
	case "update":
		if err := runUpdate(db, *tenant, updateInput{
			AdminUser:     *adminUser,
			AdminPassword: *adminPassword,
			AdminToken:    *adminToken,
			Endpoint:      *endpoint,
			DisplayName:   *displayName,
			Enable:        *enable,
			Disable:       *disable,
			Reveal:        *reveal,
			Yes:           *yes,
		}); err != nil {
			log.Fatalf("update: %v", err)
		}
	default:
		log.Fatalf("unknown -mode %q (want show|update)", *mode)
	}
}

// runShow resolves the tenant's bound git_server row and prints its config.
// Secrets are masked unless reveal=true.
func runShow(db *gorm.DB, tenantID string, reveal bool) error {
	row, err := loadBoundServer(db, tenantID)
	if err != nil {
		return err
	}
	printRow(row, reveal)
	return nil
}

type updateInput struct {
	AdminUser     string
	AdminPassword string
	AdminToken    string
	Endpoint      string
	DisplayName   string
	Enable        bool
	Disable       bool
	Reveal        bool
	Yes           bool
}

// runUpdate applies the supplied mutations to the tenant's bound git_server
// row. Empty mutation flags are ignored (preserve existing value). Enable
// and Disable are mutually exclusive.
func runUpdate(db *gorm.DB, tenantID string, in updateInput) error {
	if in.Enable && in.Disable {
		return fmt.Errorf("-enable and -disable are mutually exclusive")
	}
	if !in.Enable && !in.Disable &&
		in.AdminUser == "" && in.AdminPassword == "" && in.AdminToken == "" &&
		in.Endpoint == "" && in.DisplayName == "" {
		return fmt.Errorf("no mutation flags supplied (need at least one of -admin-user/-admin-password/-admin-token/-endpoint/-display-name/-enable/-disable)")
	}

	before, err := loadBoundServer(db, tenantID)
	if err != nil {
		return err
	}

	// Build after-state via raw SQL — single UPDATE so the change is atomic
	// against the row (no race with concurrent operator edits).
	parsed := gitServerConfigJSON{}
	if before.Config != "" {
		if err := json.Unmarshal([]byte(before.Config), &parsed); err != nil {
			return fmt.Errorf("parse existing config JSON (%s): %w", before.ServerID, err)
		}
	}
	if in.AdminUser != "" {
		parsed.AdminUser = in.AdminUser
	}
	if in.AdminPassword != "" {
		parsed.AdminPassword = in.AdminPassword
	}
	if in.AdminToken != "" {
		parsed.AdminToken = in.AdminToken
	}
	// Reject clearing admin_token — resolver rejects empty token at read time
	// (internal/gitserver/resolver.go ErrConfigMalformed branch) and that
	// would break the whole tenant. Operator must supply a non-empty value
	// or leave the field alone.
	if parsed.AdminToken == "" {
		return fmt.Errorf("refusing to write empty admin_token (resolver would reject this server); supply -admin-token")
	}
	newConfig, err := json.Marshal(parsed)
	if err != nil {
		return fmt.Errorf("marshal new config: %w", err)
	}

	newEndpoint := before.Endpoint
	if in.Endpoint != "" {
		newEndpoint = in.Endpoint
	}
	newDisplayName := before.DisplayName
	if in.DisplayName != "" {
		newDisplayName = in.DisplayName
	}
	newEnabled := before.Enabled
	if in.Enable {
		newEnabled = true
	}
	if in.Disable {
		newEnabled = false
	}

	// Diff summary for operator confirmation.
	fmt.Println("=== proposed change ===")
	fmt.Printf("server_id   : %s\n", before.ServerID)
	fmt.Printf("is_template : %v\n", before.IsTemplate)
	if newEndpoint != before.Endpoint {
		fmt.Printf("endpoint    : %q  →  %q\n", before.Endpoint, newEndpoint)
	}
	if newDisplayName != before.DisplayName {
		fmt.Printf("display_name: %q  →  %q\n", before.DisplayName, newDisplayName)
	}
	if newEnabled != before.Enabled {
		fmt.Printf("enabled     : %v  →  %v\n", before.Enabled, newEnabled)
	}
	beforeParsed := gitServerConfigJSON{}
	if before.Config != "" {
		_ = json.Unmarshal([]byte(before.Config), &beforeParsed)
	}
	mask := func(s string) string {
		if in.Reveal || s == "" {
			return s
		}
		return "<redacted>"
	}
	if beforeParsed.AdminUser != parsed.AdminUser {
		fmt.Printf("admin_user  : %q  →  %q\n", mask(beforeParsed.AdminUser), mask(parsed.AdminUser))
	}
	if beforeParsed.AdminPassword != parsed.AdminPassword {
		fmt.Printf("admin_pass  : <changed>\n")
	}
	if beforeParsed.AdminToken != parsed.AdminToken {
		fmt.Printf("admin_token : <changed>\n")
	}
	fmt.Println()

	if !in.Yes {
		fmt.Print("Apply this change? [y/N] ")
		var resp string
		fmt.Fscanln(os.Stdin, &resp)
		if resp != "y" && resp != "Y" && resp != "yes" {
			return fmt.Errorf("aborted by operator")
		}
	}

	res := db.Table("git_servers").
		Where("server_id = ?", before.ServerID).
		Updates(map[string]interface{}{
			"config":       string(newConfig),
			"endpoint":     newEndpoint,
			"display_name": newDisplayName,
			"enabled":      newEnabled,
			"updated_at":   gorm.Expr("NOW()"),
		})
	if res.Error != nil {
		return fmt.Errorf("update git_servers: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("update matched 0 rows — server_id %q vanished mid-flight", before.ServerID)
	}

	// Read-back + pretty print so the operator sees the post-write state.
	after, err := loadBoundServer(db, tenantID)
	if err != nil {
		return fmt.Errorf("post-update readback failed: %w", err)
	}
	fmt.Println("=== applied ===")
	printRow(after, in.Reveal)
	return nil
}

// loadBoundServer resolves tenant_git_server_binding → git_servers. Mirrors
// internal/gitserver.DBResolver.Resolve but without the abstraction layer —
// operators get raw error context for FK violations / drift.
func loadBoundServer(db *gorm.DB, tenantID string) (*gitServerRow, error) {
	var serverID string
	err := db.Table("tenant_git_server_binding").
		Select("git_server_id").
		Where("tenant_id = ?", tenantID).
		Row().Scan(&serverID)
	if err != nil {
		return nil, fmt.Errorf("no tenant_git_server_binding row for tenant %q (err=%w); bind one first via PUT /api/internal/tenants/:id/git-server", tenantID, err)
	}

	var r gitServerRow
	if err := db.Table("git_servers").
		Select("server_id, is_template, kind, endpoint, display_name, config, enabled").
		Where("server_id = ?", serverID).
		Row().Scan(&r.ServerID, &r.IsTemplate, &r.Kind, &r.Endpoint, &r.DisplayName, &r.Config, &r.Enabled); err != nil {
		return nil, fmt.Errorf("git_servers row %q not found (FK violation?): %w", serverID, err)
	}
	return &r, nil
}

// printRow pretty-prints the resolved row. Secrets are masked unless
// reveal=true — operators almost never need plaintext on screen; the value
// goes straight into the DB and the Gitea client uses it from there.
func printRow(r *gitServerRow, reveal bool) {
	parsed := gitServerConfigJSON{}
	if r.Config != "" {
		_ = json.Unmarshal([]byte(r.Config), &parsed)
	}
	mask := func(s string) string {
		if reveal || s == "" {
			return s
		}
		if len(s) <= 4 {
			return "<redacted>"
		}
		return s[:2] + "…" + s[len(s)-2:] + " (len=" + fmt.Sprint(len(s)) + ")"
	}
	fmt.Printf("server_id    : %s\n", r.ServerID)
	fmt.Printf("is_template  : %v\n", r.IsTemplate)
	fmt.Printf("kind         : %s\n", r.Kind)
	fmt.Printf("endpoint     : %s\n", r.Endpoint)
	fmt.Printf("display_name : %s\n", r.DisplayName)
	fmt.Printf("enabled      : %v\n", r.Enabled)
	fmt.Printf("admin_token  : %s\n", mask(parsed.AdminToken))
	fmt.Printf("admin_user   : %s\n", mask(parsed.AdminUser))
	fmt.Printf("admin_pass   : %s\n", mask(parsed.AdminPassword))
	if parsed.AdminUser == "" || parsed.AdminPassword == "" {
		fmt.Println("\n⚠  admin_user/admin_password missing — bot provisioning will fail")
		fmt.Println("   with ErrGiteaBasicAuthRequired on the next CreateTeam call.")
		fmt.Println("   Repair with: -mode update -admin-user <u> -admin-password <p>")
	}
}
