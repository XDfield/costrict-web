// Command cs-user-dbprobe is a one-shot diagnostic for the --init migration
// validation flow. Not part of the build graph — run via `go run` from
// dev machines only. Removes the need for psql when validating that
// server.users.subject_id == cs-user.users.subject_id post-migration.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

type row struct {
	SubjectID string
	Username  string
	Email     *string
	ExtKey    *string
}

func main() {
	mode := flag.String("mode", "inventory", "inventory | compare")
	// No hardcoded default DSN. A previous version baked the production
	// password AND a production IP (172.29.254.54) into the binary — same
	// CVSS 9.2 class as the server's DATABASE_URL default. Operators MUST
	// supply -server-dsn / -target-dsn (or the env vars below); dbprobe
	// refuses to run otherwise.
	serverDSN := flag.String("server-dsn", "",
		"server PG DSN (required; or set CS_USER_DBPROBE_SERVER_DSN)")
	targetDSN := flag.String("target-dsn", "",
		"cs-user PG DSN (required; or set CS_USER_DBPROBE_TARGET_DSN)")
	limit := flag.Int("limit", 50, "max rows per side for inventory / compare")
	flag.Parse()

	if strings.TrimSpace(*serverDSN) == "" {
		*serverDSN = strings.TrimSpace(os.Getenv("CS_USER_DBPROBE_SERVER_DSN"))
	}
	if strings.TrimSpace(*targetDSN) == "" {
		*targetDSN = strings.TrimSpace(os.Getenv("CS_USER_DBPROBE_TARGET_DSN"))
	}
	if *serverDSN == "" || *targetDSN == "" {
		fmt.Fprintln(os.Stderr, "server-dsn and target-dsn are required (set via -server-dsn/-target-dsn flags or CS_USER_DBPROBE_SERVER_DSN/CS_USER_DBPROBE_TARGET_DSN env vars); refusing to run with no DSN — hardcoded default removed for security (CVSS 9.2)")
		os.Exit(1)
	}

	gormCfg := &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)}

	switch *mode {
	case "inventory":
		if err := inventory(*serverDSN, "server.users", *targetDSN, "cs-user.users", *limit, gormCfg); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "compare":
		if err := compare(*serverDSN, *targetDSN, *limit, gormCfg); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintln(os.Stderr, "unknown mode:", *mode)
		os.Exit(2)
	}
}

func openDB(dsn string, cfg *gorm.Config) (*gorm.DB, error) {
	db, err := gorm.Open(postgres.Open(dsn), cfg)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	return db, nil
}

func inventory(serverDSN, serverLabel, targetDSN, targetLabel string, limit int, cfg *gorm.Config) error {
	srv, err := openDB(serverDSN, cfg)
	if err != nil {
		return fmt.Errorf("server: %w", err)
	}
	tgt, err := openDB(targetDSN, cfg)
	if err != nil {
		return fmt.Errorf("target: %w", err)
	}

	var srvCount, tgtCount int64
	srv.Unscoped().Table("users").Count(&srvCount)
	tgt.Unscoped().Table("users").Count(&tgtCount)
	fmt.Printf("%-15s %d rows (incl soft-deleted)\n", serverLabel+":", srvCount)
	fmt.Printf("%-15s %d rows (incl soft-deleted)\n", targetLabel+":", tgtCount)

	fmt.Println("\n--- sample: server.users ---")
	printUsers(srv, limit)
	fmt.Println("\n--- sample: cs-user.users ---")
	printUsers(tgt, limit)

	fmt.Println("\n--- auth_identities counts ---")
	var sAuth, tAuth int64
	srv.Unscoped().Table("user_auth_identities").Count(&sAuth)
	tgt.Unscoped().Table("user_auth_identities").Count(&tAuth)
	fmt.Printf("%-15s %d\n", "server.auth_identities:", sAuth)
	fmt.Printf("%-15s %d\n", "cs-user.auth_identities:", tAuth)

	fmt.Println("\n--- sample: server.user_auth_identities ---")
	printAuthIdentities(srv, limit)
	fmt.Println("\n--- sample: cs-user.user_auth_identities ---")
	printAuthIdentities(tgt, limit)

	return nil
}

type authRow struct {
	UserSubjectID string
	Provider      string
	ExternalKey   *string
}

func printAuthIdentities(db *gorm.DB, limit int) {
	var rows []authRow
	err := db.Unscoped().Table("user_auth_identities").
		Select("user_subject_id, provider, external_key").
		Order("created_at DESC").
		Limit(limit).
		Scan(&rows).Error
	if err != nil {
		fmt.Println("  (query failed:", err, ")")
		return
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "  USER_SUBJECT_ID\tPROVIDER\tEXTERNAL_KEY")
	for _, r := range rows {
		ext := "<null>"
		if r.ExternalKey != nil {
			ext = *r.ExternalKey
		}
		fmt.Fprintf(w, "  %s\t%s\t%s\n", r.UserSubjectID, r.Provider, ext)
	}
	w.Flush()
}

func printUsers(db *gorm.DB, limit int) {
	var rows []row
	err := db.Unscoped().Table("users").
		Select("subject_id, username, email, external_key").
		Order("created_at DESC").
		Limit(limit).
		Scan(&rows).Error
	if err != nil {
		fmt.Println("  (query failed:", err, ")")
		return
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "  SUBJECT_ID\tUSERNAME\tEMAIL\tEXTERNAL_KEY")
	for _, r := range rows {
		email, ext := "<null>", "<null>"
		if r.Email != nil {
			email = *r.Email
		}
		if r.ExtKey != nil {
			ext = *r.ExtKey
		}
		fmt.Fprintf(w, "  %s\t%s\t%s\t%s\n", r.SubjectID, r.Username, email, ext)
	}
	w.Flush()
}

// compare joins server and cs-user by user_auth_identities.external_key
// (the durable identity handle; users.external_key is null in this codebase).
// Reports subject_id divergence — post-migration every server user_auth_identity
// should have a cs-user counterpart whose user_subject_id matches.
func compare(serverDSN, targetDSN string, limit int, cfg *gorm.Config) error {
	srv, err := openDB(serverDSN, cfg)
	if err != nil {
		return fmt.Errorf("server: %w", err)
	}
	tgt, err := openDB(targetDSN, cfg)
	if err != nil {
		return fmt.Errorf("target: %w", err)
	}

	type authRow struct {
		UserSubjectID string
		Provider      string
		ExternalKey   string
	}
	var srvRows []authRow
	if err := srv.Unscoped().Table("user_auth_identities").
		Select("user_subject_id, provider, external_key").
		Where("external_key IS NOT NULL AND external_key <> ''").
		Order("external_key").
		Find(&srvRows).Error; err != nil {
		return fmt.Errorf("scan server.user_auth_identities: %w", err)
	}

	var matchCount, mismatchCount, missingCount int
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "  EXTERNAL_KEY\tSERVER_SUBJECT_ID\tCSUSER_SUBJECT_ID\tSTATUS")

	for _, s := range srvRows {
		var t authRow
		err := tgt.Unscoped().Table("user_auth_identities").
			Select("user_subject_id, provider, external_key").
			Where("external_key = ?", s.ExternalKey).
			Take(&t).Error
		if err != nil {
			fmt.Fprintf(w, "  %s\t%s\t<missing>\tMISSING_IN_CSUSER\n", s.ExternalKey, s.UserSubjectID)
			missingCount++
			continue
		}
		if t.UserSubjectID == s.UserSubjectID {
			matchCount++
		} else {
			fmt.Fprintf(w, "  %s\t%s\t%s\tSUBJECT_ID_MISMATCH\n", s.ExternalKey, s.UserSubjectID, t.UserSubjectID)
			mismatchCount++
		}
	}
	w.Flush()

	fmt.Printf("\nsummary: matched=%d mismatched=%d missing=%d (total server auth_identities=%d)\n",
		matchCount, mismatchCount, missingCount, len(srvRows))
	if mismatchCount == 0 && missingCount == 0 {
		fmt.Println("VERDICT: ✅ every server user has a cs-user counterpart with matching subject_id")
		return nil
	}
	fmt.Println("VERDICT: ❌ divergence remains — see rows above")
	os.Exit(1)
	return nil
}
