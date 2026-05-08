package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/costrict/costrict-web/server/internal/config"
	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/team"
	"github.com/costrict/costrict-web/server/internal/services"
	migrations "github.com/costrict/costrict-web/server/migrations"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/google/uuid"
	"github.com/pressly/goose/v3"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

var errDryRunRollback = errors.New("dry-run rollback")

const (
	publicRegistryID = "00000000-0000-0000-0000-000000000001"
	publicRepoID     = "public"
	importCreatedBy  = "system"
)

func main() {
	cfg := config.Load()

	db, err := database.Initialize(cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}

	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "help", "-h", "--help":
			printMigrateHelp()
			return
		case "user-subject-ids":
			dryRun := len(os.Args) > 2 && os.Args[2] == "--dry-run"
			if err := backfillLegacyUserReferences(db, dryRun); err != nil {
				if dryRun && errors.Is(err, errDryRunRollback) {
					log.Println("Legacy user reference dry-run completed successfully")
					return
				}
				log.Fatalf("Failed to backfill legacy user references: %v", err)
			}
			if dryRun {
				log.Println("Legacy user reference dry-run completed successfully")
			} else {
				log.Println("Legacy user reference backfill completed successfully")
			}
			return
		case "user-external-identities":
			dryRun := len(os.Args) > 2 && os.Args[2] == "--dry-run"
			if err := backfillUserExternalIdentities(db, dryRun); err != nil {
				if dryRun && errors.Is(err, errDryRunRollback) {
					log.Println("User external identity dry-run completed successfully")
					return
				}
				log.Fatalf("Failed to backfill user external identities: %v", err)
			}
			if dryRun {
				log.Println("User external identity dry-run completed successfully")
			} else {
				log.Println("User external identity backfill completed successfully")
			}
			return
		case "import-everything-ai-coding":
			sourcePath := ""
			dryRun := false
			for _, arg := range os.Args[2:] {
				if arg == "--dry-run" {
					dryRun = true
					continue
				}
				if strings.HasPrefix(arg, "--source=") {
					sourcePath = strings.TrimPrefix(arg, "--source=")
					continue
				}
				if !strings.HasPrefix(arg, "--") && sourcePath == "" {
					sourcePath = arg
				}
			}

			if sourcePath == "" {
				log.Fatalf("Missing source path. Use --source=<everything-ai-coding-path> or pass path as positional arg")
			}

			if err := importEverythingAICoding(db, sourcePath, dryRun); err != nil {
				if dryRun && errors.Is(err, errDryRunRollback) {
					log.Println("Everything-AI-Coding import dry-run completed successfully")
					return
				}
				log.Fatalf("Failed to import Everything-AI-Coding data: %v", err)
			}
			if dryRun {
				log.Println("Everything-AI-Coding import dry-run completed successfully")
			} else {
				log.Println("Everything-AI-Coding import completed successfully")
			}
			return
		case "backfill-everything-ai-coding-metadata":
			sourcePath := ""
			dryRun := false
			for _, arg := range os.Args[2:] {
				if arg == "--dry-run" {
					dryRun = true
					continue
				}
				if strings.HasPrefix(arg, "--source=") {
					sourcePath = strings.TrimPrefix(arg, "--source=")
					continue
				}
				if !strings.HasPrefix(arg, "--") && sourcePath == "" {
					sourcePath = arg
				}
			}

			if sourcePath == "" {
				log.Fatalf("Missing source path. Use --source=<everything-ai-coding-path> or pass path as positional arg")
			}

			if err := backfillCatalogMetadata(db, sourcePath, dryRun); err != nil {
				log.Fatalf("Failed to backfill catalog metadata: %v", err)
			}
			if dryRun {
				log.Println("Catalog metadata backfill dry-run completed successfully")
			} else {
				log.Println("Catalog metadata backfill completed successfully")
			}
			return
		case "backfill-capability-content-versioning":
			if err := backfillCapabilityContentVersioning(db); err != nil {
				log.Fatalf("Failed to backfill capability content versioning: %v", err)
			}
			log.Println("Capability content versioning backfill completed successfully")
			return
		case "backfill-provider-aware-external-keys":
			dryRun := len(os.Args) > 2 && os.Args[2] == "--dry-run"
			if err := backfillProviderAwareExternalKeys(db, dryRun); err != nil {
				if dryRun && errors.Is(err, errDryRunRollback) {
					log.Println("Provider-aware external key dry-run completed successfully")
					return
				}
				log.Fatalf("Failed to backfill provider-aware external keys: %v", err)
			}
			if dryRun {
				log.Println("Provider-aware external key dry-run completed successfully")
			} else {
				log.Println("Provider-aware external key backfill completed successfully")
			}
			return
		default:
			log.Printf("Unknown command: %s", os.Args[1])
			printMigrateHelp()
			os.Exit(1)
		}
	}

	if err := runPreMigrations(db); err != nil {
		log.Fatalf("Failed to run pre-migrations: %v", err)
	}

	err = db.AutoMigrate(
		&team.TeamSession{},
		&team.TeamSessionMember{},
		&team.TeamTask{},
		&team.TeamApprovalRequest{},
		&team.TeamRepoAffinity{},
		&models.UserSystemRole{},
		&models.Repository{},
			&models.RepoMember{},
			&models.RepoInvitation{},
		&models.UserSystemRole{},
		&models.Repository{},
		&models.RepoMember{},
		&models.RepoInvitation{},
		&models.Project{},
		&models.ProjectMember{},
		&models.ProjectInvitation{},
		&models.ProjectRepository{},
		&models.SyncLog{},
		&models.SyncJob{},
		&models.CapabilityRegistry{},
		&models.CapabilityItem{},
		&models.CapabilityVersion{},
		&models.CapabilityVersionAsset{},
		&models.CapabilityAsset{},
		&models.CapabilityArtifact{},
		&models.BehaviorLog{},
		&models.ItemFavorite{},
		&models.SecurityScan{},
		&models.ScanJob{},
		&models.Device{},
		&models.Workspace{},
		&models.WorkspaceDirectory{},
		&models.SystemNotificationChannel{},
		&models.UserNotificationChannel{},
		&models.UserConfig{},
		&models.NotificationLog{},
		&models.ItemCategory{},
		&models.DeviceRelease{},
		&models.DeviceCommandResult{},
	)
	if err != nil {
		log.Fatalf("Failed to auto-migrate database: %v", err)
	}

	if err := runGooseMigrations(db); err != nil {
		log.Fatalf("Failed to run goose migrations: %v", err)
	}
	if err := ensureUserIdentityColumns(db); err != nil {
		log.Fatalf("Failed to ensure user identity columns: %v", err)
	}
	if err := ensureUserAuthIdentitiesTable(db); err != nil {
		log.Fatalf("Failed to ensure user auth identities table: %v", err)
	}

	if err := backfillCapabilityContentVersioning(db); err != nil {
		log.Fatalf("Failed to backfill capability content versioning: %v", err)
	}
	if err := backfillUserExternalIdentities(db, false); err != nil {
		log.Fatalf("Failed to backfill user external identities: %v", err)
	}
	if err := backfillUserAuthIdentities(db, false); err != nil {
		log.Fatalf("Failed to backfill user auth identities: %v", err)
	}

	log.Println("All migrations completed successfully")
}

func printMigrateHelp() {
	fmt.Println("Usage:")
	fmt.Println("  go run ./cmd/migrate                          Run schema migrations")
	fmt.Println("  go run ./cmd/migrate backfill-capability-content-versioning")
	fmt.Println("                                                Backfill content_md5 and current_revision for capability items")
	fmt.Println("  go run ./cmd/migrate user-subject-ids [--dry-run]")
	fmt.Println("                                                Backfill legacy user IDs to subject_id")
	fmt.Println("  go run ./cmd/migrate user-external-identities [--dry-run]")
	fmt.Println("                                                Backfill users.external_key/auth_provider/provider_user_id/phone")
	fmt.Println("  go run ./cmd/migrate import-everything-ai-coding <source-path> [--dry-run]")
	fmt.Println("                                                Import MCP/command/skills/agent data")
	fmt.Println("  go run ./cmd/migrate import-everything-ai-coding --source=<source-path> [--dry-run]")
	fmt.Println("  go run ./cmd/migrate backfill-everything-ai-coding-metadata <source-path> [--dry-run]")
	fmt.Println("                                                Backfill source and experience_score from catalog/index.json")
	fmt.Println("  go run ./cmd/migrate backfill-provider-aware-external-keys [--dry-run]")
	fmt.Println("                                                Migrate external_keys from casdoor:<id> to casdoor:<provider>:<id>")
	fmt.Println("")
	fmt.Println("Examples:")
	fmt.Println("  go run ./cmd/migrate")
	fmt.Println("  go run ./cmd/migrate backfill-capability-content-versioning")
	fmt.Println("  go run ./cmd/migrate user-subject-ids --dry-run")
	fmt.Println("  go run ./cmd/migrate user-external-identities --dry-run")
	fmt.Println("  go run ./cmd/migrate import-everything-ai-coding /Users/linkai/code/.../everything-ai-coding --dry-run")
	fmt.Println("  go run ./cmd/migrate import-everything-ai-coding --source=/Users/linkai/code/.../everything-ai-coding")
	fmt.Println("  go run ./cmd/migrate backfill-everything-ai-coding-metadata /Users/linkai/code/.../everything-ai-coding --dry-run")
}

func ensureUserIdentityColumns(db *gorm.DB) error {
	stmts := []string{
		`ALTER TABLE users ADD COLUMN IF NOT EXISTS phone text`,
		`ALTER TABLE users ADD COLUMN IF NOT EXISTS auth_provider text`,
		`ALTER TABLE users ADD COLUMN IF NOT EXISTS external_key text`,
		`ALTER TABLE users ADD COLUMN IF NOT EXISTS provider_user_id text`,
		`CREATE INDEX IF NOT EXISTS idx_user_phone ON users(phone)`,
		`CREATE INDEX IF NOT EXISTS idx_user_auth_provider ON users(auth_provider)`,
		`CREATE INDEX IF NOT EXISTS idx_user_provider_user_id ON users(provider_user_id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_user_external_key ON users(external_key) WHERE external_key IS NOT NULL`,
	}
	for _, stmt := range stmts {
		if err := db.Exec(stmt).Error; err != nil {
			return fmt.Errorf("ensure users identity columns failed (%s): %w", stmt, err)
		}
	}
	return nil
}

func ensureUserAuthIdentitiesTable(db *gorm.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS user_auth_identities (
			id BIGSERIAL PRIMARY KEY,
			user_subject_id text NOT NULL,
			provider text NOT NULL,
			issuer text,
			external_key text NOT NULL,
			external_subject text,
			external_user_id text,
			provider_user_id text,
			display_name text,
			email text,
			phone text,
			avatar_url text,
			organization text,
			is_primary boolean NOT NULL DEFAULT false,
			last_login_at timestamptz,
			created_at timestamptz,
			updated_at timestamptz,
			deleted_at timestamptz
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_user_auth_identities_external_key ON user_auth_identities(external_key)`,
		`CREATE INDEX IF NOT EXISTS idx_user_auth_identities_user_subject_id ON user_auth_identities(user_subject_id)`,
	}
	for _, stmt := range stmts {
		if err := db.Exec(stmt).Error; err != nil {
			return fmt.Errorf("ensure user_auth_identities failed (%s): %w", stmt, err)
		}
	}
	return nil
}

func backfillUserExternalIdentities(db *gorm.DB, dryRun bool) error {
	hasPhone := db.Migrator().HasColumn(&models.User{}, "phone")
	hasAuthProvider := db.Migrator().HasColumn(&models.User{}, "auth_provider")
	hasExternalKey := db.Migrator().HasColumn(&models.User{}, "external_key")
	hasProviderUserID := db.Migrator().HasColumn(&models.User{}, "provider_user_id")

	selectColumns := []string{"id", "subject_id", "username", "email", "casdoor_id", "casdoor_universal_id", "casdoor_sub"}
	if hasPhone {
		selectColumns = append(selectColumns, "phone")
	}
	if hasAuthProvider {
		selectColumns = append(selectColumns, "auth_provider")
	}
	if hasExternalKey {
		selectColumns = append(selectColumns, "external_key")
	}
	if hasProviderUserID {
		selectColumns = append(selectColumns, "provider_user_id")
	}

	type userIdentityRow struct {
		ID                uint
		SubjectID         string
		Username          string
		Email             *string
		Phone             *string
		AuthProvider      *string
		ExternalKey       *string
		ProviderUserID    *string
		CasdoorID         *string
		CasdoorUniversalID *string
		CasdoorSub        *string
	}

	var users []userIdentityRow
	if err := db.Table("users").Select(strings.Join(selectColumns, ", ")).Find(&users).Error; err != nil {
		return fmt.Errorf("load users for external identity backfill: %w", err)
	}

	updatedRows := 0
	return db.Transaction(func(tx *gorm.DB) error {
		for _, user := range users {
			updates := map[string]any{}

			if hasExternalKey && (user.ExternalKey == nil || *user.ExternalKey == "") {
				if user.CasdoorUniversalID != nil && *user.CasdoorUniversalID != "" {
					updates["external_key"] = "casdoor:" + *user.CasdoorUniversalID
				} else if user.CasdoorSub != nil && *user.CasdoorSub != "" {
					updates["external_key"] = "casdoor-sub:" + *user.CasdoorSub
				} else if user.CasdoorID != nil && *user.CasdoorID != "" {
					updates["external_key"] = "casdoor-id:" + *user.CasdoorID
				}
			}

			if hasAuthProvider && (user.AuthProvider == nil || *user.AuthProvider == "") && user.Username != "" {
				switch {
				case strings.HasPrefix(user.Username, "phone_"):
					updates["auth_provider"] = "phone"
				case user.CasdoorUniversalID != nil && *user.CasdoorUniversalID != "":
					updates["auth_provider"] = "casdoor"
				}
			}

			if hasPhone && (user.Phone == nil || *user.Phone == "") && user.Email != nil && *user.Email != "" && isLikelyPhoneValue(*user.Email) {
				updates["phone"] = *user.Email
			}

			if len(updates) == 0 {
				continue
			}
			updatedRows++
			if dryRun {
				continue
			}
			if err := tx.Table("users").Where("id = ?", user.ID).Updates(updates).Error; err != nil {
				return fmt.Errorf("update user %d external identities: %w", user.ID, err)
			}
		}

		log.Printf("user external identity summary (dry-run=%v): updated users=%d", dryRun, updatedRows)
		if dryRun {
			return errDryRunRollback
		}
		return nil
	})
}

func backfillUserAuthIdentities(db *gorm.DB, dryRun bool) error {
	type userRow struct {
		SubjectID          string
		DisplayName        *string
		Email              *string
		Phone              *string
		AvatarURL          *string
		Organization       *string
		AuthProvider       *string
		ExternalKey        *string
		ProviderUserID     *string
		CasdoorUniversalID *string
		CasdoorID          *string
		CasdoorSub         *string
	}
	var users []userRow
	if err := db.Table("users").Select("subject_id, display_name, email, phone, avatar_url, organization, auth_provider, external_key, provider_user_id, casdoor_universal_id, casdoor_id, casdoor_sub").Find(&users).Error; err != nil {
		return fmt.Errorf("load users for auth identity backfill: %w", err)
	}
	created := 0
	return db.Transaction(func(tx *gorm.DB) error {
		for _, user := range users {
			if strings.TrimSpace(user.SubjectID) == "" {
				continue
			}
			externalKey := ""
			if user.ExternalKey != nil {
				externalKey = strings.TrimSpace(*user.ExternalKey)
			}
			if externalKey == "" {
				if user.CasdoorUniversalID != nil && *user.CasdoorUniversalID != "" {
					externalKey = "casdoor:" + *user.CasdoorUniversalID
				} else if user.CasdoorSub != nil && *user.CasdoorSub != "" {
					externalKey = "casdoor-sub:" + *user.CasdoorSub
				} else if user.CasdoorID != nil && *user.CasdoorID != "" {
					externalKey = "casdoor-id:" + *user.CasdoorID
				}
			}
			if externalKey == "" {
				continue
			}
			var count int64
			if err := tx.Table("user_auth_identities").Where("external_key = ?", externalKey).Count(&count).Error; err != nil {
				return err
			}
			if count > 0 {
				continue
			}
			provider := "casdoor"
			if user.AuthProvider != nil && strings.TrimSpace(*user.AuthProvider) != "" {
				provider = strings.ToLower(strings.TrimSpace(*user.AuthProvider))
			}
			created++
			if dryRun {
				continue
			}
			if err := tx.Table("user_auth_identities").Create(map[string]any{
				"user_subject_id": user.SubjectID,
				"provider":        provider,
				"external_key":    externalKey,
				"external_subject": coalesceStringPtr(user.CasdoorUniversalID, user.CasdoorSub),
				"external_user_id": user.CasdoorID,
				"provider_user_id": user.ProviderUserID,
				"display_name":    user.DisplayName,
				"email":           user.Email,
				"phone":           user.Phone,
				"avatar_url":      user.AvatarURL,
				"organization":    user.Organization,
				"is_primary":      true,
				"created_at":      time.Now(),
				"updated_at":      time.Now(),
			}).Error; err != nil {
				return fmt.Errorf("create backfilled auth identity for %s: %w", user.SubjectID, err)
			}
		}
		log.Printf("user auth identity summary (dry-run=%v): created identities=%d", dryRun, created)
		if dryRun {
			return errDryRunRollback
		}
		return nil
	})
}

func coalesceStringPtr(values ...*string) *string {
	for _, value := range values {
		if value != nil && strings.TrimSpace(*value) != "" {
			return value
		}
	}
	return nil
}

func backfillProviderAwareExternalKeys(db *gorm.DB, dryRun bool) error {
	type identityRow struct {
		ID          uint
		ExternalKey string
		Provider    string
	}
	var identities []identityRow
	if err := db.Table("user_auth_identities").
		Select("id, external_key, provider").
		Where("external_key LIKE 'casdoor:%' AND external_key NOT LIKE 'casdoor:%:%' AND provider != '' AND provider != 'casdoor'").
		Find(&identities).Error; err != nil {
		return fmt.Errorf("load identities for provider-aware key migration: %w", err)
	}

	type userRow struct {
		ID          uint
		ExternalKey *string
		AuthProvider *string
	}
	var users []userRow
	if err := db.Table("users").
		Select("id, external_key, auth_provider").
		Where("external_key LIKE 'casdoor:%' AND external_key NOT LIKE 'casdoor:%:%'").
		Find(&users).Error; err != nil {
		return fmt.Errorf("load users for provider-aware key migration: %w", err)
	}

	updatedIdentities := 0
	updatedUsers := 0
	return db.Transaction(func(tx *gorm.DB) error {
		for _, idRow := range identities {
			provider := strings.ToLower(strings.TrimSpace(idRow.Provider))
			parts := strings.SplitN(idRow.ExternalKey, ":", 2)
			if len(parts) != 2 || provider == "" {
				continue
			}
			newKey := "casdoor:" + provider + ":" + parts[1]
			var conflict int64
			if err := tx.Table("user_auth_identities").Where("external_key = ? AND id != ?", newKey, idRow.ID).Count(&conflict).Error; err != nil {
				return err
			}
			if conflict > 0 {
				log.Printf("identity %d: skipping due to conflict with new key %q", idRow.ID, newKey)
				continue
			}
			updatedIdentities++
			if dryRun {
				continue
			}
			if err := tx.Table("user_auth_identities").Where("id = ?", idRow.ID).Update("external_key", newKey).Error; err != nil {
				return fmt.Errorf("update identity %d external_key: %w", idRow.ID, err)
			}
		}

		for _, u := range users {
			if u.ExternalKey == nil {
				continue
			}
			provider := ""
			if u.AuthProvider != nil {
				provider = strings.ToLower(strings.TrimSpace(*u.AuthProvider))
			}
			if provider == "" || provider == "casdoor" {
				continue
			}
			parts := strings.SplitN(*u.ExternalKey, ":", 2)
			if len(parts) != 2 {
				continue
			}
			newKey := "casdoor:" + provider + ":" + parts[1]
			var conflict int64
			if err := tx.Table("users").Where("external_key = ? AND id != ?", newKey, u.ID).Count(&conflict).Error; err != nil {
				return err
			}
			if conflict > 0 {
				log.Printf("user %d: skipping due to conflict with new key %q", u.ID, newKey)
				continue
			}
			updatedUsers++
			if dryRun {
				continue
			}
			if err := tx.Table("users").Where("id = ?", u.ID).Update("external_key", newKey).Error; err != nil {
				return fmt.Errorf("update user %d external_key: %w", u.ID, err)
			}
		}

		log.Printf("provider-aware external key summary (dry-run=%v): updated identities=%d, updated users=%d", dryRun, updatedIdentities, updatedUsers)
		if dryRun {
			return errDryRunRollback
		}
		return nil
	})
}

func isLikelyPhoneValue(v string) bool {
	v = strings.TrimSpace(v)
	if v == "" {
		return false
	}
	for _, ch := range v {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return len(v) >= 6 && len(v) <= 20
}

func backfillCapabilityContentVersioning(db *gorm.DB) error {
	hashSvc := services.NewContentHashService()

	return db.Transaction(func(tx *gorm.DB) error {
		var items []models.CapabilityItem
		if err := tx.Preload("Assets").Find(&items).Error; err != nil {
			return fmt.Errorf("load capability items: %w", err)
		}

		for _, item := range items {
			contentMD5 := strings.TrimSpace(item.ContentMD5)
			if contentMD5 == "" {
				var err error
				contentMD5, err = hashCurrentItemContent(hashSvc, item)
				if err != nil {
					log.Printf("Skipping capability item %s during content versioning backfill: %v", item.ID, err)
					continue
				}
			}

			currentRevision := item.CurrentRevision
			if currentRevision < 1 {
				if err := tx.Model(&models.CapabilityVersion{}).Where("item_id = ?", item.ID).Select("COALESCE(MAX(revision), 1)").Scan(&currentRevision).Error; err != nil {
					return fmt.Errorf("query current revision for item %s: %w", item.ID, err)
				}
				if currentRevision < 1 {
					currentRevision = 1
				}
			}

			if err := tx.Model(&models.CapabilityItem{}).Where("id = ?", item.ID).Updates(map[string]any{
				"content_md5":      contentMD5,
				"current_revision": currentRevision,
			}).Error; err != nil {
				return fmt.Errorf("update item %s: %w", item.ID, err)
			}
		}

		var versions []models.CapabilityVersion
		if err := tx.Find(&versions).Error; err != nil {
			return fmt.Errorf("load capability versions: %w", err)
		}
		for _, version := range versions {
			if strings.TrimSpace(version.ContentMD5) != "" {
				continue
			}

			var item models.CapabilityItem
			if err := tx.Unscoped().Select("id", "item_type").First(&item, "id = ?", version.ItemID).Error; err != nil {
				return fmt.Errorf("load item %s for version %s: %w", version.ItemID, version.ID, err)
			}

			contentMD5, err := hashSvc.HashTextContent(item.ItemType, version.Content)
			if err != nil {
				log.Printf("Skipping capability version %s during content versioning backfill: %v", version.ID, err)
				continue
			}
			if err := tx.Model(&models.CapabilityVersion{}).Where("id = ?", version.ID).Update("content_md5", contentMD5).Error; err != nil {
				return fmt.Errorf("update version %s: %w", version.ID, err)
			}
		}

		return nil
	})
}

func hashCurrentItemContent(hashSvc *services.ContentHashService, item models.CapabilityItem) (string, error) {
	if item.SourceType == "archive" {
		entries := make([]services.ArchiveManifestEntry, 0, len(item.Assets)+1)
		mainHash, err := hashSvc.HashTextContent(item.ItemType, item.Content)
		if err != nil {
			return "", err
		}
		mainPath := item.SourcePath
		if strings.TrimSpace(mainPath) == "" {
			switch item.ItemType {
			case "mcp":
				mainPath = ".mcp.json"
			default:
				mainPath = "SKILL.md"
			}
		}
		entries = append(entries, services.ArchiveManifestEntry{Path: mainPath, ContentHash: mainHash})

		for _, asset := range item.Assets {
			assetHash := strings.TrimSpace(asset.ContentSHA)
			if assetHash == "" {
				if asset.TextContent != nil {
					var err error
					assetHash, err = hashSvc.HashTextContent("text", *asset.TextContent)
					if err != nil {
						return "", err
					}
				} else {
					continue
				}
			}
			entries = append(entries, services.ArchiveManifestEntry{Path: asset.RelPath, ContentHash: assetHash})
		}

		return hashSvc.HashArchiveManifest(entries), nil
	}

	return hashSvc.HashTextContent(item.ItemType, item.Content)
}

func importEverythingAICoding(db *gorm.DB, sourcePath string, dryRun bool) error {
	if strings.TrimSpace(sourcePath) == "" {
		return fmt.Errorf("source path is required")
	}
	absSource, err := filepath.Abs(sourcePath)
	if err != nil {
		return fmt.Errorf("resolve source path: %w", err)
	}
	if stat, err := os.Stat(absSource); err != nil || !stat.IsDir() {
		if err != nil {
			return fmt.Errorf("source path check failed: %w", err)
		}
		return fmt.Errorf("source path is not a directory: %s", absSource)
	}

	catalogDownloadRoot := filepath.Join(absSource, "catalog-download")
	if _, err := os.Stat(catalogDownloadRoot); err != nil {
		return fmt.Errorf("catalog-download directory not found in %s", absSource)
	}

	// 1. Prepare temp sync directory with mapped file structure.
	tempDir, err := os.MkdirTemp("", "costrict-sync-everything-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tempDir)

	if err := mapEverythingToSyncDir(catalogDownloadRoot, tempDir); err != nil {
		return fmt.Errorf("map source to sync dir: %w", err)
	}

	// 2. Init a temporary git repo so SyncService can clone from it.
	if err := initTempGitRepo(tempDir); err != nil {
		return fmt.Errorf("init temp git repo: %w", err)
	}

	// Ensure public registry exists before syncing.
	if err := ensurePublicRegistry(db); err != nil {
		return fmt.Errorf("ensure public registry: %w", err)
	}

	// 3. Create a temporary registry pointing at the temp git repo.
	tempRegistryID := uuid.New().String()
	registry := models.CapabilityRegistry{
		ID:             tempRegistryID,
		Name:           "everything-ai-coding-import",
		Description:    "Temporary registry for importing everything-ai-coding",
		ExternalURL:    tempDir,
		ExternalBranch: "main",
		SyncConfig: mustJSON(map[string]any{
			"includePatterns": []string{
				"mcp/**/.mcp.json",
				"prompts/**/PROMPT.md",
				"rules/**/RULE.md",
				"skills/**/SKILL.md",
			},
		}),
		RepoID:      publicRepoID,
		OwnerID:     importCreatedBy,
		SyncEnabled: true,
	}
	if err := db.Create(&registry).Error; err != nil {
		return fmt.Errorf("create temp registry: %w", err)
	}

	// 4. Run sync via SyncService.
	syncSvc := &services.SyncService{
		DB:     db,
		Git:    &services.GitService{TempBaseDir: os.TempDir()},
		Parser: &services.ParserService{},
	}

	result, err := syncSvc.SyncRegistry(context.Background(), tempRegistryID, services.SyncOptions{
		TriggerType: "manual",
		TriggerUser: importCreatedBy,
		DryRun:      dryRun,
	})
	if err != nil {
		deleteTempRegistry(db, tempRegistryID)
		return fmt.Errorf("sync registry: %w", err)
	}

	// 5. Relationship mapping: migrate synced items to the public registry.
	if !dryRun {
		if err := db.Model(&models.CapabilityItem{}).Where("registry_id = ?", tempRegistryID).Update("registry_id", publicRegistryID).Error; err != nil {
			return fmt.Errorf("migrate items to public registry: %w", err)
		}
		if err := deleteTempRegistry(db, tempRegistryID); err != nil {
			return fmt.Errorf("delete temp registry: %w", err)
		}
	}

	log.Printf("everything-ai-coding sync summary: added=%d updated=%d skipped=%d deleted=%d failed=%d",
		result.Added, result.Updated, result.Skipped, result.Deleted, result.Failed)
	if len(result.Errors) > 0 {
		for _, e := range result.Errors {
			log.Printf("  sync error: %s", e)
		}
	}

	// 6. Backfill source and experience_score from catalog/index.json.
	if !dryRun {
		if err := backfillCatalogMetadata(db, absSource, false); err != nil {
			log.Printf("Warning: failed to backfill catalog metadata: %v", err)
		}
	}

	return nil
}

func backfillCatalogMetadata(db *gorm.DB, absSource string, dryRun bool) error {
	catalogPath := filepath.Join(absSource, "catalog", "index.json")
	data, err := os.ReadFile(catalogPath)
	if err != nil {
		return fmt.Errorf("read catalog/index.json: %w", err)
	}

	var catalogItems []struct {
		ID     string `json:"id"`
		Source string `json:"source"`
		Stars  int    `json:"stars"`
	}
	if err := json.Unmarshal(data, &catalogItems); err != nil {
		return fmt.Errorf("parse catalog/index.json: %w", err)
	}

	catalogMap := make(map[string]struct {
		Source string
		Stars  int
	}, len(catalogItems))
	for _, item := range catalogItems {
		catalogMap[item.ID] = struct {
			Source string
			Stars  int
		}{
			Source: item.Source,
			Stars:  item.Stars,
		}
	}

	var items []models.CapabilityItem
	if err := db.Where("registry_id = ?", publicRegistryID).Find(&items).Error; err != nil {
		return fmt.Errorf("load items for backfill: %w", err)
	}

	updated := 0
	skipped := 0
	for _, item := range items {
		parts := strings.Split(filepath.ToSlash(item.SourcePath), "/")
		if len(parts) < 2 {
			skipped++
			continue
		}
		dirName := parts[1]
		meta, ok := catalogMap[dirName]
		if !ok {
			skipped++
			continue
		}
		updates := map[string]any{}
		if meta.Source != "" {
			updates["source"] = meta.Source
		}
		if meta.Stars > 0 {
			updates["experience_score"] = meta.Stars
		}
		if len(updates) == 0 {
			skipped++
			continue
		}
		if dryRun {
			updated++
			continue
		}
		if err := db.Model(&models.CapabilityItem{}).Where("id = ?", item.ID).Updates(updates).Error; err != nil {
			log.Printf("Warning: failed to update item %s: %v", item.ID, err)
			skipped++
			continue
		}
		updated++
	}

	log.Printf("Catalog metadata backfill (dryRun=%v): updated %d items, skipped %d", dryRun, updated, skipped)
	return nil
}

func mapEverythingToSyncDir(sourceRoot, targetRoot string) error {
	return filepath.WalkDir(sourceRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		relPath, err := filepath.Rel(sourceRoot, path)
		if err != nil {
			return err
		}
		relPath = filepath.ToSlash(relPath)

		targetPath := filepath.Join(targetRoot, relPath)
		if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
			return err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(targetPath, content, 0644)
	})
}

func initTempGitRepo(dir string) error {
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		return fmt.Errorf("git init: %w", err)
	}
	w, err := repo.Worktree()
	if err != nil {
		return err
	}
	if _, err := w.Add("."); err != nil {
		return fmt.Errorf("git add: %w", err)
	}
	if _, err := w.Commit("Initial import", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "import",
			Email: "import@costrict.local",
			When:  time.Now(),
		},
	}); err != nil {
		return fmt.Errorf("git commit: %w", err)
	}
	return nil
}

func deleteTempRegistry(db *gorm.DB, registryID string) error {
	db.Where("registry_id = ?", registryID).Delete(&models.SyncLog{})
	db.Where("registry_id = ?", registryID).Delete(&models.SyncJob{})
	return db.Delete(&models.CapabilityRegistry{ID: registryID}).Error
}

func ensurePublicRegistry(db *gorm.DB) error {
	var exists int
	if err := db.Raw(`SELECT 1 FROM capability_registries WHERE id = ?`, publicRegistryID).Scan(&exists).Error; err != nil {
		return fmt.Errorf("check public registry: %w", err)
	}
	if exists == 1 {
		return nil
	}

	now := db.NowFunc()
	reg := models.CapabilityRegistry{
		ID:          publicRegistryID,
		Name:        "public",
		Description: "Default public registry",
		SourceType:  "internal",
		RepoID:      publicRepoID,
		OwnerID:     importCreatedBy,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := db.Create(&reg).Error; err != nil {
		return fmt.Errorf("create public registry: %w", err)
	}
	return nil
}

func mustJSON(v any) datatypes.JSON {
	b, err := json.Marshal(v)
	if err != nil {
		return datatypes.JSON([]byte("{}"))
	}
	if len(b) == 0 {
		return datatypes.JSON([]byte("{}"))
	}
	return datatypes.JSON(b)
}

func runGooseMigrations(db *gorm.DB) error {
	sqlDB, err := db.DB()
	if err != nil {
		return fmt.Errorf("failed to get underlying sql.DB: %w", err)
	}

	goose.SetBaseFS(migrations.FS)

	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("failed to set goose dialect: %w", err)
	}

	if err := goose.Up(sqlDB, ".", goose.WithAllowMissing()); err != nil {
		return fmt.Errorf("goose migration failed: %w", err)
	}

	log.Println("Goose migrations completed successfully")
	return nil
}

func runPreMigrations(db *gorm.DB) error {
	bootstrapStmts := []string{
		`CREATE TABLE IF NOT EXISTS sync_logs (
			id uuid DEFAULT gen_random_uuid() PRIMARY KEY,
			registry_id uuid NOT NULL,
			trigger_type text NOT NULL,
			trigger_user text,
			status text NOT NULL DEFAULT 'running',
			commit_sha text,
			previous_sha text,
			total_items bigint DEFAULT 0,
			added_items bigint DEFAULT 0,
			updated_items bigint DEFAULT 0,
			deleted_items bigint DEFAULT 0,
			skipped_items bigint DEFAULT 0,
			failed_items bigint DEFAULT 0,
			error_message text,
			duration_ms bigint,
			started_at timestamptz NOT NULL DEFAULT now(),
			finished_at timestamptz,
			created_at timestamptz
		)`,
		`CREATE TABLE IF NOT EXISTS capability_registries (
			id uuid DEFAULT gen_random_uuid() PRIMARY KEY,
			name text NOT NULL,
			description text,
			source_type text NOT NULL DEFAULT 'internal',
			external_url text,
			external_branch text DEFAULT 'main',
			sync_enabled boolean DEFAULT false,
			sync_interval bigint DEFAULT 3600,
			last_synced_at timestamptz,
			last_sync_sha text,
			sync_status text DEFAULT 'idle',
			sync_config JSONB DEFAULT '{}',
			last_sync_log_id uuid,
			visibility text DEFAULT 'repo',
			repo_id text,
			owner_id text NOT NULL,
			created_at timestamptz,
			updated_at timestamptz
		)`,
	}
	for _, stmt := range bootstrapStmts {
		if err := db.Exec(stmt).Error; err != nil {
			return fmt.Errorf("bootstrap failed: %w", err)
		}
	}

	preMigrations := []struct {
		check string
		stmts []string
	}{
		{
			check: `SELECT 1 FROM information_schema.columns WHERE table_name='capability_versions' AND column_name='version'`,
			stmts: []string{
				`ALTER TABLE capability_versions ADD COLUMN IF NOT EXISTS revision bigint`,
				`ALTER TABLE capability_versions ALTER COLUMN revision SET NOT NULL`,
				`ALTER TABLE capability_versions ALTER COLUMN revision SET DEFAULT 1`,
			},
		},
		{
			check: `SELECT 1 FROM information_schema.columns WHERE table_name='capability_items' AND column_name='visibility'`,
			stmts: []string{
				`ALTER TABLE capability_items DROP COLUMN IF EXISTS visibility`,
			},
		},
		// Drop visibility column from capability_registries — visibility is now
		// derived from the parent repository's visibility field.
		{
			check: `SELECT 1 FROM information_schema.columns WHERE table_name='capability_registries' AND column_name='visibility'`,
			stmts: []string{
				`ALTER TABLE capability_registries DROP COLUMN IF EXISTS visibility`,
			},
		},
		{
			check: `SELECT 1 FROM information_schema.columns WHERE table_name='security_scans' AND column_name='revision_id'`,
			stmts: []string{
				`ALTER TABLE security_scans DROP COLUMN IF EXISTS revision_id`,
			},
		},
		{
			check: `SELECT 1 FROM pg_indexes WHERE indexname = 'idx_item_slug'`,
			stmts: []string{
				`DROP INDEX IF EXISTS idx_item_slug`,
			},
		},
		{
			check: `SELECT 1 FROM pg_indexes WHERE indexname = 'idx_item_slug_global'`,
			stmts: []string{
				`DROP INDEX IF EXISTS idx_item_slug_global`,
			},
		},
		{
			check: `SELECT 1 FROM information_schema.columns WHERE table_name='capability_items' AND column_name='id' AND NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='capability_items' AND column_name='repo_id')`,
			stmts: []string{
				`ALTER TABLE capability_items ADD COLUMN repo_id text NOT NULL DEFAULT 'public'`,
			},
		},
	}

	for _, m := range preMigrations {
		var exists int
		if err := db.Raw(m.check).Scan(&exists).Error; err != nil {
			return fmt.Errorf("pre-migration check failed (%s): %w", m.check, err)
		}
		if exists != 1 {
			continue
		}
		if m.check == `SELECT 1 FROM information_schema.columns WHERE table_name='capability_versions' AND column_name='version'` {
			if err := normalizeLegacyCapabilityVersions(db); err != nil {
				return fmt.Errorf("normalize legacy capability versions: %w", err)
			}
		}
		for _, stmt := range m.stmts {
			if err := db.Exec(stmt).Error; err != nil {
				return fmt.Errorf("pre-migration failed (%s): %w", stmt, err)
			}
		}
	}

	if err := backfillCapabilityItemRepoIDs(db); err != nil {
		return fmt.Errorf("failed to backfill capability_items.repo_id before migrations: %w", err)
	}

	if err := deduplicateSlugs(db); err != nil {
		return fmt.Errorf("failed to deduplicate slugs before composite unique index: %w", err)
	}
	return nil
}

func normalizeLegacyCapabilityVersions(db *gorm.DB) error {
	type legacyVersionRow struct {
		ID     string
		ItemID string
	}

	return db.Transaction(func(tx *gorm.DB) error {
		var rows []legacyVersionRow
		if err := tx.Table("capability_versions").
			Select("id, item_id").
			Order("item_id ASC, created_at ASC, id ASC").
			Find(&rows).Error; err != nil {
			return fmt.Errorf("load legacy capability versions: %w", err)
		}

		keepIDs := make([]string, 0, len(rows))
		deleteIDs := make([]string, 0)
		seenItems := make(map[string]struct{}, len(rows))
		for _, row := range rows {
			if _, ok := seenItems[row.ItemID]; ok {
				deleteIDs = append(deleteIDs, row.ID)
				continue
			}
			seenItems[row.ItemID] = struct{}{}
			keepIDs = append(keepIDs, row.ID)
		}

		if len(deleteIDs) > 0 {
			if err := tx.Table("capability_versions").Where("id IN ?", deleteIDs).Delete(nil).Error; err != nil {
				return fmt.Errorf("delete duplicate legacy capability versions: %w", err)
			}
		}

		if len(keepIDs) > 0 {
			if err := tx.Table("capability_versions").Where("id IN ?", keepIDs).Update("revision", 1).Error; err != nil {
				return fmt.Errorf("initialize legacy capability version revisions: %w", err)
			}
		}

		if err := tx.Table("capability_items").Where("current_revision < 1 OR current_revision IS NULL").Update("current_revision", 1).Error; err != nil {
			return fmt.Errorf("initialize capability item current revisions: %w", err)
		}

		return nil
	})
}

func backfillCapabilityItemRepoIDs(db *gorm.DB) error {
	var tableExists int
	if err := db.Raw(`SELECT 1 FROM information_schema.tables WHERE table_schema = CURRENT_SCHEMA() AND table_name = 'capability_items'`).Scan(&tableExists).Error; err != nil {
		return fmt.Errorf("checking capability_items existence: %w", err)
	}
	if tableExists != 1 {
		return nil
	}

	var needsBackfill int
	if err := db.Raw(`SELECT 1 FROM capability_items WHERE repo_id = 'public' LIMIT 1`).Scan(&needsBackfill).Error; err != nil {
		return fmt.Errorf("checking capability_items repo_id backfill: %w", err)
	}
	if needsBackfill != 1 {
		return nil
	}

	if err := db.Exec(`UPDATE capability_items SET repo_id = COALESCE(
		(SELECT COALESCE(NULLIF(cr.repo_id,''), 'public')
		 FROM capability_registries cr
		 WHERE cr.id = capability_items.registry_id),
		'public'
	) WHERE repo_id = 'public'`).Error; err != nil {
		return fmt.Errorf("backfilling capability_items.repo_id: %w", err)
	}

	return nil
}

func backfillLegacyUserReferences(db *gorm.DB, dryRun bool) error {
	type userMapping struct {
		SubjectID          string
		CasdoorUniversalID *string
		CasdoorID          *string
		CasdoorSub         *string
	}

	var users []userMapping
	if err := db.Table("users").Select("subject_id, casdoor_universal_id, casdoor_id, casdoor_sub").Find(&users).Error; err != nil {
		return fmt.Errorf("load user mappings: %w", err)
	}

	mapping := make(map[string]string, len(users)*4)
	for _, user := range users {
		if user.SubjectID == "" {
			continue
		}
		mapping[user.SubjectID] = user.SubjectID
		if user.CasdoorUniversalID != nil && *user.CasdoorUniversalID != "" {
			mapping[*user.CasdoorUniversalID] = user.SubjectID
		}
		if user.CasdoorID != nil && *user.CasdoorID != "" {
			mapping[*user.CasdoorID] = user.SubjectID
		}
		if user.CasdoorSub != nil && *user.CasdoorSub != "" {
			mapping[*user.CasdoorSub] = user.SubjectID
		}
	}

	updates := []struct {
		table  string
		column string
	}{
		{"system_notification_channels", "created_by"},
		{"user_notification_channels", "user_id"},
		{"user_configs", "user_id"},
		{"notification_logs", "user_id"},
		{"devices", "user_id"},
		{"repositories", "owner_id"},
		{"repo_members", "user_id"},
		{"repo_invitations", "inviter_id"},
		{"repo_invitations", "invitee_id"},
		{"projects", "creator_id"},
		{"project_members", "user_id"},
		{"project_invitations", "inviter_id"},
		{"project_invitations", "invitee_id"},
		{"project_repositories", "bound_by_user_id"},
		{"user_system_roles", "user_id"},
		{"user_system_roles", "granted_by"},
		{"capability_registries", "owner_id"},
		{"sync_jobs", "trigger_user"},
		{"sync_logs", "trigger_user"},
		{"capability_items", "created_by"},
		{"capability_items", "updated_by"},
		{"item_categories", "created_by"},
		{"item_favorites", "user_id"},
		{"capability_versions", "created_by"},
		{"capability_artifacts", "uploaded_by"},
		{"scan_jobs", "trigger_user"},
		{"workspaces", "user_id"},
		{"behavior_logs", "user_id"},
	}

	type tableStat struct {
		UpdatedRows int
		Unresolved  map[string]int
	}
	stats := make(map[string]*tableStat, len(updates))
	getStat := func(table, column string) *tableStat {
		key := table + "." + column
		if stats[key] == nil {
			stats[key] = &tableStat{Unresolved: map[string]int{}}
		}
		return stats[key]
	}

	return db.Transaction(func(tx *gorm.DB) error {
		for _, target := range updates {
			rows, err := tx.Table(target.table).Select(target.column).Where(target.column + " IS NOT NULL AND " + target.column + " <> ''").Rows()
			if err != nil {
				return fmt.Errorf("scan %s.%s: %w", target.table, target.column, err)
			}
			values := map[string]struct{}{}
			for rows.Next() {
				var value string
				if err := rows.Scan(&value); err != nil {
					rows.Close()
					return fmt.Errorf("read %s.%s: %w", target.table, target.column, err)
				}
				if value != "" {
					values[value] = struct{}{}
				}
			}
			rows.Close()

			for legacyValue := range values {
				subjectID, ok := mapping[legacyValue]
				if !ok {
					getStat(target.table, target.column).Unresolved[legacyValue]++
					continue
				}
				if subjectID == legacyValue {
					continue
				}

				var affected int64
				if dryRun {
					if err := tx.Table(target.table).Where(target.column+" = ?", legacyValue).Count(&affected).Error; err != nil {
						return fmt.Errorf("count %s.%s for %s: %w", target.table, target.column, legacyValue, err)
					}
				} else {
					result := tx.Exec(
						fmt.Sprintf("UPDATE %s SET %s = ? WHERE %s = ?", target.table, target.column, target.column),
						subjectID,
						legacyValue,
					)
					if result.Error != nil {
						return fmt.Errorf("update %s.%s from %s to %s: %w", target.table, target.column, legacyValue, subjectID, result.Error)
					}
					affected = result.RowsAffected
				}
				getStat(target.table, target.column).UpdatedRows += int(affected)
			}
		}

		log.Printf("user-subject-ids summary (dry-run=%v)", dryRun)
		for _, target := range updates {
			key := target.table + "." + target.column
			stat := stats[key]
			if stat == nil {
				continue
			}
			if stat.UpdatedRows > 0 {
				log.Printf("  [%s] updated rows: %d", key, stat.UpdatedRows)
			}
			if len(stat.Unresolved) > 0 {
				log.Printf("  [%s] unresolved identifiers:", key)
				for legacyValue, seenCount := range stat.Unresolved {
					log.Printf("    - %s (seen %d times)", legacyValue, seenCount)
				}
			}
		}

		if dryRun {
			return errDryRunRollback
		}
		return nil
	})
}

func deduplicateSlugs(db *gorm.DB) error {
	var tableExists int
	if err := db.Raw(`SELECT 1 WHERE to_regclass('public.capability_items') IS NOT NULL`).Scan(&tableExists).Error; err != nil {
		return fmt.Errorf("checking capability_items existence: %w", err)
	}
	if tableExists != 1 {
		return nil
	}

	var idxExists int
	db.Raw(`SELECT 1 FROM pg_indexes WHERE indexname = 'idx_item_repo_type_slug'`).Scan(&idxExists)
	if idxExists == 1 {
		return nil
	}

	type row struct {
		ID       string
		RepoID   string `gorm:"column:repo_id"`
		ItemType string `gorm:"column:item_type"`
		Slug     string
	}

	var rows []row
	err := db.Raw(`
		SELECT id, repo_id, item_type, slug
		FROM capability_items
		WHERE (repo_id, item_type, slug) IN (
			SELECT repo_id, item_type, slug
			FROM capability_items
			GROUP BY repo_id, item_type, slug HAVING COUNT(*) > 1
		)
		ORDER BY repo_id, item_type, slug, created_at ASC, id ASC`,
	).Scan(&rows).Error
	if err != nil {
		return fmt.Errorf("querying duplicate slugs: %w", err)
	}
	if len(rows) == 0 {
		return nil
	}

	type groupKey struct{ RepoID, ItemType, Slug string }
	type group struct {
		ids []string
	}
	groups := make(map[groupKey]*group)
	var keys []groupKey
	for _, r := range rows {
		k := groupKey{r.RepoID, r.ItemType, r.Slug}
		g, ok := groups[k]
		if !ok {
			groups[k] = &group{ids: []string{r.ID}}
			keys = append(keys, k)
		} else {
			g.ids = append(g.ids, r.ID)
		}
	}

	return db.Transaction(func(tx *gorm.DB) error {
		for _, k := range keys {
			g := groups[k]
			for i := 1; i < len(g.ids); i++ {
				candidate := ""
				for n := i + 1; ; n++ {
					candidate = fmt.Sprintf("%s-%d", k.Slug, n)
					var count int64
					if err := tx.Raw(
						`SELECT COUNT(*) FROM capability_items WHERE repo_id = ? AND item_type = ? AND slug = ?`,
						k.RepoID, k.ItemType, candidate,
					).Scan(&count).Error; err != nil {
						return fmt.Errorf("checking slug %q: %w", candidate, err)
					}
					if count == 0 {
						break
					}
				}
				log.Printf("[deduplicateSlugs] renaming item %s slug %q -> %q (repo=%s, type=%s)",
					g.ids[i], k.Slug, candidate, k.RepoID, k.ItemType)
				if err := tx.Exec(
					`UPDATE capability_items SET slug = ? WHERE id = ?`, candidate, g.ids[i],
				).Error; err != nil {
					return fmt.Errorf("renaming slug for item %s: %w", g.ids[i], err)
				}
			}
		}
		return nil
	})
}
