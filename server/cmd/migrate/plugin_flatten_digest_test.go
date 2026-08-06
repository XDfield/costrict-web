package main

import (
	"reflect"
	"strings"
	"testing"
)

// pluginFlattenDigestExcluded are the only fields the plan digest may ignore.
// They are the run's OUTCOME and change while apply walks the plan, so covering
// them would make the digest unverifiable exactly when a crashed run resumes.
var pluginFlattenDigestExcluded = map[string]bool{
	"Conflict": true,
	"RowState": true,
}

// AC-FP13 / AC-FP16: the artifact's checksum has to cover what the checksum is
// sold as covering.
//
// This is written as "every field except two" rather than as a list of the
// fields that matter, because the v1 digest WAS such a list and the list was
// wrong: `contentBackend` and `sourceType` are part of the compare-and-set
// predicate, so editing them in the stored plan changed which live rows apply
// would agree to write while the digest still verified; and none of the
// provenance columns an operator signs off on in runbook §4 were covered at all.
// A list-shaped test would have gone on passing. Reflection means a new column
// fails here on the day it is added.
func TestPluginFlattenPlanDigest_CoversEveryFieldExceptTheOutcome(t *testing.T) {
	base := pluginFlattenPlanRow{
		Seq: 7, ItemID: "11111111-1111-4111-8111-000000000002", ItemType: "skill",
		ItemSlug: "child-one", RegistryID: "22222222-2222-4222-8222-000000000001",
		SourceType: "direct", ContentBackend: "db",
		CatalogEntryDir: "skills/child-one", BundledIn: "host",
		SourcePath: "skills/child-one/SKILL.md", SourceManifestSHA: "abc123",
		GitServerID: "gitea-local", GitRepoID: 42, GitRepoPath: "SKILL.md", GitEntryKey: "entry",
		ForkedFromItemID: ptr("33333333-3333-4333-8333-000000000001"),
		ParentItemID:     ptr("11111111-1111-4111-8111-000000000001"),
		ParentExists:     true, ParentItemType: "plugin", ParentSourceType: "direct",
		FavoriteCount: 3, DistributionCount: 1,
		BeforeStatus: "active", BeforeParentPluginID: ptr("11111111-1111-4111-8111-000000000001"),
		AfterStatus: "archived", AfterParentPluginID: nil,
		Classification: flattenClassDerivedCatalog, Action: flattenActionArchiveAndUnlink,
		Reason: "catalog entry skills/child-one declared bundled_in=host",
		// Outcome fields, set to non-zero so "excluded" is proven rather than
		// accidentally true because the value never moved.
		Conflict: "concurrent change", RowState: flattenRowSkipped,
	}
	want := pluginFlattenPlanDigest(pluginFlattenSchemaVersion, flattenModeMigrate, []pluginFlattenPlanRow{base})

	rowType := reflect.TypeOf(base)
	for i := 0; i < rowType.NumField(); i++ {
		field := rowType.Field(i)
		t.Run(field.Name, func(t *testing.T) {
			mutated := base
			if !mutatePluginFlattenField(reflect.ValueOf(&mutated).Elem().Field(i)) {
				t.Fatalf("no mutation rule for %s of kind %s; add one rather than skipping the field",
					field.Name, field.Type)
			}
			got := pluginFlattenPlanDigest(pluginFlattenSchemaVersion, flattenModeMigrate,
				[]pluginFlattenPlanRow{mutated})
			if pluginFlattenDigestExcluded[field.Name] {
				if got != want {
					t.Fatalf("%s is an outcome field and must NOT move the digest", field.Name)
				}
				return
			}
			if got == want {
				t.Fatalf("editing %s leaves the digest unchanged; the artifact's integrity contract does not cover it",
					field.Name)
			}
		})
	}
}

// mutatePluginFlattenField changes a field to a different value in place.
func mutatePluginFlattenField(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.String:
		v.SetString(v.String() + "-mutated")
		return true
	case reflect.Bool:
		v.SetBool(!v.Bool())
		return true
	case reflect.Int, reflect.Int64:
		v.SetInt(v.Int() + 1)
		return true
	case reflect.Ptr:
		if v.Type().Elem().Kind() != reflect.String {
			return false
		}
		next := "mutated"
		if !v.IsNil() {
			next = v.Elem().String() + "-mutated"
		}
		v.Set(reflect.ValueOf(&next))
		return true
	default:
		return false
	}
}

// The digest must also be stable under row order: apply and rollback sort by
// item id internally, and a plan whose rows came back in a different order is
// the same plan.
func TestPluginFlattenPlanDigest_IsIndependentOfRowOrder(t *testing.T) {
	a := pluginFlattenPlanRow{ItemID: "a", BeforeStatus: "active", AfterStatus: "archived",
		Classification: flattenClassDerivedCatalog, Action: flattenActionArchiveAndUnlink, Reason: "x"}
	b := a
	b.ItemID = "b"
	forward := pluginFlattenPlanDigest(pluginFlattenSchemaVersion, flattenModeMigrate, []pluginFlattenPlanRow{a, b})
	reverse := pluginFlattenPlanDigest(pluginFlattenSchemaVersion, flattenModeMigrate, []pluginFlattenPlanRow{b, a})
	if forward != reverse {
		t.Fatalf("digest depends on row order: %s vs %s", forward, reverse)
	}
}

// A v1 plan/artifact must be refused, not reinterpreted: the same bytes hash
// differently under v2, so silently accepting one would mean comparing a v1
// digest against a v2 recomputation and rejecting it with a confusing message —
// or worse, accepting it if the two ever collided in shape.
func TestPluginFlattenPlanDigest_SchemaVersionIsPartOfTheHash(t *testing.T) {
	row := pluginFlattenPlanRow{ItemID: "a", BeforeStatus: "active", AfterStatus: "archived",
		Classification: flattenClassDerivedCatalog, Action: flattenActionArchiveAndUnlink, Reason: "x"}
	if pluginFlattenPlanDigest(1, flattenModeMigrate, []pluginFlattenPlanRow{row}) ==
		pluginFlattenPlanDigest(2, flattenModeMigrate, []pluginFlattenPlanRow{row}) {
		t.Fatal("schema version does not participate in the digest")
	}
	// Mode too: a migrate plan and its inverse must not share a digest.
	if pluginFlattenPlanDigest(2, flattenModeMigrate, []pluginFlattenPlanRow{row}) ==
		pluginFlattenPlanDigest(2, flattenModeRollback, []pluginFlattenPlanRow{row}) {
		t.Fatal("mode does not participate in the digest")
	}
}

// A tool where one flag decides how much data moves per transaction must not
// silently truncate a typo into a valid number.
func TestParseFlattenInt_RejectsTrailingGarbage(t *testing.T) {
	for _, bad := range []string{"200x", "5abc", "", "1 2", "0x10", "--3", "1.5"} {
		if got, err := parseFlattenInt(bad); err == nil {
			t.Errorf("parseFlattenInt(%q) = %d, want an error", bad, got)
		}
	}
	for _, tc := range []struct {
		in   string
		want int
	}{{"200", 200}, {" 500 ", 500}, {"-1", -1}, {"0", 0}} {
		got, err := parseFlattenInt(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("parseFlattenInt(%q) = %d, %v; want %d, nil", tc.in, got, err, tc.want)
		}
	}
}

// The tool tables have exactly one definition (the goose migration), and this is
// the parser that lets the command run it. It only has to understand what that
// file contains — `--` comments and single-quoted literals, both of which hold
// semicolons in the real file — but it does have to understand those.
func TestSplitSQLStatements_IgnoresSemicolonsInCommentsAndLiterals(t *testing.T) {
	got := splitSQLStatements(`
-- a comment; with a semicolon
CREATE TABLE t (a INT);
COMMENT ON TABLE t IS 'planned -> applying; then applied';
SELECT 'it''s escaped; still one statement';
`)
	if len(got) != 3 {
		t.Fatalf("split into %d statements, want 3: %#v", len(got), got)
	}
	if !strings.HasPrefix(got[0], "CREATE TABLE") {
		t.Errorf("first statement kept the leading comment: %q", got[0])
	}
	if !strings.Contains(got[1], "applying; then applied") {
		t.Errorf("literal was cut at its semicolon: %q", got[1])
	}
	if !strings.Contains(got[2], "it''s escaped; still one statement") {
		t.Errorf("escaped quote confused the splitter: %q", got[2])
	}
}

// The Up block of the real migration must be extractable and must contain the
// two tables. If this fails, `migrate flatten-plugins` cannot bootstrap its own
// tables and every subcommand stops at the first statement.
func TestPluginFlattenMigrationStatements_ParsesTheRealFile(t *testing.T) {
	statements, err := pluginFlattenMigrationStatements()
	if err != nil {
		t.Fatalf("parse %s: %v", pluginFlattenMigrationFile, err)
	}
	joined := strings.Join(statements, "\n")
	for _, want := range []string{
		"CREATE TABLE IF NOT EXISTS plugin_flatten_migration_runs",
		"CREATE TABLE IF NOT EXISTS plugin_flatten_migration_rows",
		"idx_plugin_flatten_runs_source",
		"idx_plugin_flatten_rows_item",
		"already_at_target",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("extracted Up block does not contain %q", want)
		}
	}
	for _, unwanted := range []string{"+goose", "DROP TABLE IF EXISTS plugin_flatten_migration_rows"} {
		if strings.Contains(joined, unwanted) {
			t.Errorf("extracted Up block leaked %q (the Down block or a goose marker)", unwanted)
		}
	}
}
