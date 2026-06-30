package services

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/deptsync"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/notification/sender"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type fakeNotifier struct {
	calls []struct {
		userID    string
		eventType string
		msg       sender.NotificationMessage
	}
}

func (f *fakeNotifier) TriggerMessage(userID, eventType string, msg sender.NotificationMessage) {
	f.calls = append(f.calls, struct {
		userID    string
		eventType string
		msg       sender.NotificationMessage
	}{userID, eventType, msg})
}

func setupDistributionServiceDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	stmts := []string{
		`CREATE TABLE item_distributions (
			id TEXT PRIMARY KEY,
			item_id TEXT NOT NULL,
			distributor_id TEXT NOT NULL,
			permission_mode TEXT DEFAULT 'readonly',
			status TEXT DEFAULT 'active',
			scope_type TEXT DEFAULT 'user',
			target_id TEXT NOT NULL,
			message TEXT,
			revoked_at DATETIME,
			expires_at DATETIME,
			created_at DATETIME,
			updated_at DATETIME
		)`,
		`CREATE TABLE item_distribution_receipts (
			id TEXT PRIMARY KEY,
			distribution_id TEXT NOT NULL,
			user_id TEXT NOT NULL,
			receipt_status TEXT DEFAULT 'unread',
			forked_item_id TEXT,
			created_at DATETIME,
			updated_at DATETIME
		)`,
		`CREATE TABLE capability_items (
			id TEXT PRIMARY KEY,
			name TEXT
		)`,
	}
	for _, stmt := range stmts {
		if err := db.Exec(stmt).Error; err != nil {
			t.Fatalf("create table: %v", err)
		}
	}
	return db
}

func seedDistributions(t *testing.T, db *gorm.DB) {
	t.Helper()
	now := time.Now()
	items := [][2]string{
		{"item-1", "Alpha Plugin"},
		{"item-2", "Beta Skill"},
	}
	for _, it := range items {
		if err := db.Exec(`INSERT INTO capability_items (id, name) VALUES (?, ?)`, it[0], it[1]).Error; err != nil {
			t.Fatalf("seed item: %v", err)
		}
	}
	dists := []models.ItemDistribution{
		{ID: "d1", ItemID: "item-1", DistributorID: "admin-a", Status: "active", ScopeType: "user", TargetID: "u1", CreatedAt: now.Add(-3 * time.Hour)},
		{ID: "d2", ItemID: "item-1", DistributorID: "admin-b", Status: "paused", ScopeType: "user", TargetID: "u2", CreatedAt: now.Add(-2 * time.Hour)},
		{ID: "d3", ItemID: "item-2", DistributorID: "admin-a", Status: "active", ScopeType: "department", TargetID: "dept-x", CreatedAt: now.Add(-1 * time.Hour)},
		{ID: "d4", ItemID: "item-2", DistributorID: "admin-c", Status: "revoked", ScopeType: "user", TargetID: "u3", CreatedAt: now},
	}
	for _, d := range dists {
		if err := db.Create(&d).Error; err != nil {
			t.Fatalf("seed distribution: %v", err)
		}
	}
}

func TestListAllDistributions_AcrossDistributors(t *testing.T) {
	db := setupDistributionServiceDB(t)
	seedDistributions(t, db)
	svc := NewDistributionService(db, nil, nil)

	list, total, err := svc.ListAllDistributions(context.Background(), DistributionListFilter{})
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if total != 4 {
		t.Fatalf("expected total 4, got %d", total)
	}
	if len(list) != 4 {
		t.Fatalf("expected 4 rows, got %d", len(list))
	}
	// Default order: created_at DESC -> d4, d3, d2, d1
	if list[0].ID != "d4" {
		t.Fatalf("expected newest first (d4), got %s", list[0].ID)
	}
	// Item preloaded
	if list[0].Item == nil || list[0].Item.Name != "Beta Skill" {
		t.Fatalf("expected Item preloaded with name Beta Skill, got %+v", list[0].Item)
	}
}

func TestListAllDistributions_FiltersAndPagination(t *testing.T) {
	db := setupDistributionServiceDB(t)
	seedDistributions(t, db)
	svc := NewDistributionService(db, nil, nil)
	ctx := context.Background()

	// status filter
	list, total, err := svc.ListAllDistributions(ctx, DistributionListFilter{Status: "active"})
	if err != nil {
		t.Fatalf("status filter: %v", err)
	}
	if total != 2 || len(list) != 2 {
		t.Fatalf("expected 2 active, got total=%d len=%d", total, len(list))
	}

	// scope filter
	list, total, err = svc.ListAllDistributions(ctx, DistributionListFilter{ScopeType: "department"})
	if err != nil {
		t.Fatalf("scope filter: %v", err)
	}
	if total != 1 || list[0].ID != "d3" {
		t.Fatalf("expected only d3 for department scope, got total=%d", total)
	}

	// search by item name
	list, total, err = svc.ListAllDistributions(ctx, DistributionListFilter{Search: "Alpha"})
	if err != nil {
		t.Fatalf("search item name: %v", err)
	}
	if total != 2 {
		t.Fatalf("expected 2 distributions for item Alpha Plugin, got %d", total)
	}

	// search by distributor id
	list, total, err = svc.ListAllDistributions(ctx, DistributionListFilter{Search: "admin-c"})
	if err != nil {
		t.Fatalf("search distributor: %v", err)
	}
	if total != 1 || list[0].ID != "d4" {
		t.Fatalf("expected d4 for distributor admin-c, got total=%d", total)
	}

	// pagination: page size 2
	list, total, err = svc.ListAllDistributions(ctx, DistributionListFilter{Page: 1, PageSize: 2})
	if err != nil {
		t.Fatalf("pagination page1: %v", err)
	}
	if total != 4 || len(list) != 2 {
		t.Fatalf("expected total=4 len=2 on page1, got total=%d len=%d", total, len(list))
	}
	page2, _, err := svc.ListAllDistributions(ctx, DistributionListFilter{Page: 2, PageSize: 2})
	if err != nil {
		t.Fatalf("pagination page2: %v", err)
	}
	if len(page2) != 2 || page2[0].ID == list[0].ID {
		t.Fatalf("expected distinct page2 rows, got %v vs %v", page2[0].ID, list[0].ID)
	}
}

func setupRevokeFavoriteDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	stmts := []string{
		`CREATE TABLE item_distributions (
			id TEXT PRIMARY KEY, item_id TEXT NOT NULL, distributor_id TEXT NOT NULL,
			permission_mode TEXT DEFAULT 'readonly', status TEXT DEFAULT 'active',
			scope_type TEXT DEFAULT 'user', target_id TEXT NOT NULL, message TEXT,
			revoked_at DATETIME, expires_at DATETIME, created_at DATETIME, updated_at DATETIME)`,
		`CREATE TABLE item_distribution_receipts (
			id TEXT PRIMARY KEY, distribution_id TEXT NOT NULL, user_id TEXT NOT NULL,
			receipt_status TEXT DEFAULT 'unread', forked_item_id TEXT, created_at DATETIME, updated_at DATETIME)`,
		`CREATE TABLE capability_items (id TEXT PRIMARY KEY, name TEXT, favorite_count INTEGER DEFAULT 0)`,
		`CREATE TABLE item_favorites (id TEXT PRIMARY KEY, item_id TEXT NOT NULL, user_id TEXT NOT NULL, created_at DATETIME)`,
	}
	for _, stmt := range stmts {
		if err := db.Exec(stmt).Error; err != nil {
			t.Fatalf("create table: %v", err)
		}
	}
	return db
}

func countFavorites(t *testing.T, db *gorm.DB, itemID, userID string) int64 {
	t.Helper()
	var n int64
	if err := db.Model(&models.ItemFavorite{}).Where("item_id = ? AND user_id = ?", itemID, userID).Count(&n).Error; err != nil {
		t.Fatalf("count favorites: %v", err)
	}
	return n
}

func TestRevokeReadonlyDistribution_RemovesRecipientFavorite(t *testing.T) {
	db := setupRevokeFavoriteDB(t)
	svc := NewDistributionService(db, NewBehaviorService(db), nil)
	ctx := context.Background()

	if err := db.Exec(`INSERT INTO capability_items (id, name, favorite_count) VALUES ('item-1','Code Reviewer',1)`).Error; err != nil {
		t.Fatalf("seed item: %v", err)
	}
	if err := db.Create(&models.ItemDistribution{ID: "d1", ItemID: "item-1", DistributorID: "admin-a", PermissionMode: "readonly", Status: "active", ScopeType: "user", TargetID: "u1"}).Error; err != nil {
		t.Fatalf("seed dist: %v", err)
	}
	if err := db.Create(&models.ItemDistributionReceipt{ID: "r1", DistributionID: "d1", UserID: "u1", ReceiptStatus: "read"}).Error; err != nil {
		t.Fatalf("seed receipt: %v", err)
	}
	if err := db.Create(&models.ItemFavorite{ID: "f1", ItemID: "item-1", UserID: "u1"}).Error; err != nil {
		t.Fatalf("seed favorite: %v", err)
	}
	if got := countFavorites(t, db, "item-1", "u1"); got != 1 {
		t.Fatalf("precondition: expected 1 favorite, got %d", got)
	}

	if err := svc.RevokeDistribution(ctx, "d1", "admin-a", true); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	// Revoking a READONLY distribution must remove the recipient's favorite.
	// Before the tx fix the readonly guard ran in a separate transaction, saw the
	// distribution as still 'active', and blocked the removal — so the skill
	// stayed favorited on the cloud and /hub never unloaded it.
	if got := countFavorites(t, db, "item-1", "u1"); got != 0 {
		t.Fatalf("expected favorite removed after revoking readonly distribution, got %d", got)
	}
}

func TestRevokeReadonlyDistribution_KeepsFavoriteWhenAnotherActiveReadonlyExists(t *testing.T) {
	db := setupRevokeFavoriteDB(t)
	svc := NewDistributionService(db, NewBehaviorService(db), nil)
	ctx := context.Background()

	if err := db.Exec(`INSERT INTO capability_items (id, name, favorite_count) VALUES ('item-1','Code Reviewer',1)`).Error; err != nil {
		t.Fatalf("seed item: %v", err)
	}
	for _, d := range []models.ItemDistribution{
		{ID: "d1", ItemID: "item-1", DistributorID: "admin-a", PermissionMode: "readonly", Status: "active", ScopeType: "user", TargetID: "u1"},
		{ID: "d2", ItemID: "item-1", DistributorID: "admin-b", PermissionMode: "readonly", Status: "active", ScopeType: "user", TargetID: "u1"},
	} {
		if err := db.Create(&d).Error; err != nil {
			t.Fatalf("seed dist: %v", err)
		}
	}
	if err := db.Create(&models.ItemDistributionReceipt{ID: "r1", DistributionID: "d1", UserID: "u1", ReceiptStatus: "read"}).Error; err != nil {
		t.Fatalf("seed receipt: %v", err)
	}
	if err := db.Create(&models.ItemDistributionReceipt{ID: "r2", DistributionID: "d2", UserID: "u1", ReceiptStatus: "read"}).Error; err != nil {
		t.Fatalf("seed receipt: %v", err)
	}
	if err := db.Create(&models.ItemFavorite{ID: "f1", ItemID: "item-1", UserID: "u1"}).Error; err != nil {
		t.Fatalf("seed favorite: %v", err)
	}

	if err := svc.RevokeDistribution(ctx, "d1", "admin-a", true); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	// d2 (still active + readonly) keeps requiring the item, so the favorite stays.
	if got := countFavorites(t, db, "item-1", "u1"); got != 1 {
		t.Fatalf("expected favorite kept (another active readonly distribution exists), got %d", got)
	}
}

func TestDistributeItem_NotificationCarriesMessage(t *testing.T) {
	db := setupDistributionServiceDB(t)
	svc := NewDistributionService(db, nil, nil)
	notifier := &fakeNotifier{}
	svc.SetNotificationService(notifier)
	ctx := context.Background()

	item := &models.CapabilityItem{ID: "item-1", Name: "Alpha Plugin"}

	// With a message (附言), the recipient notification body must carry it.
	_, err := svc.DistributeItem(ctx, item, "admin-a", DistributeItemRequest{
		Targets:        []DistributionTarget{{ScopeType: "user", TargetID: "u1"}},
		PermissionMode: "readonly",
		Message:        "请尽快试用",
	})
	if err != nil {
		t.Fatalf("distribute: %v", err)
	}
	if len(notifier.calls) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(notifier.calls))
	}
	got := notifier.calls[0]
	if got.userID != "u1" {
		t.Fatalf("expected notify u1, got %s", got.userID)
	}
	if !strings.Contains(got.msg.Body, "附言：请尽快试用") {
		t.Fatalf("expected body to carry 附言, got %q", got.msg.Body)
	}
	if got.msg.Metadata["message"] != "请尽快试用" {
		t.Fatalf("expected metadata.message=请尽快试用, got %v", got.msg.Metadata["message"])
	}

	// A blank/whitespace-only message must NOT add an 附言 line.
	notifier.calls = nil
	_, err = svc.DistributeItem(ctx, item, "admin-a", DistributeItemRequest{
		Targets:        []DistributionTarget{{ScopeType: "user", TargetID: "u2"}},
		PermissionMode: "readonly",
		Message:        "   ",
	})
	if err != nil {
		t.Fatalf("distribute (blank message): %v", err)
	}
	if len(notifier.calls) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(notifier.calls))
	}
	if strings.Contains(notifier.calls[0].msg.Body, "附言") {
		t.Fatalf("expected no 附言 line for blank message, got %q", notifier.calls[0].msg.Body)
	}
}

// fakeDeptResolver is a stand-in for the dept-sync client (deptMemberResolver) so
// the department-scope recipient resolution AND structural authorization can be
// tested without a live service.
type fakeDeptResolver struct {
	configured bool
	members    []deptsync.DeptUser
	err        error
	calledWith string
	deptPaths  map[string]string          // deptID -> dept_path
	userDepts  map[string][]deptsync.Dept // universalID -> departments the user belongs to
	subtrees   map[string]*deptsync.Dept  // deptID -> node-with-children (nil/no-children = leaf)
}

func (f *fakeDeptResolver) GetDeptUsersTree(deptID string) ([]deptsync.DeptUser, error) {
	f.calledWith = deptID
	return f.members, f.err
}

func (f *fakeDeptResolver) DepartmentSubtree(deptID string) (*deptsync.Dept, error) {
	return f.subtrees[deptID], nil
}

func (f *fakeDeptResolver) GetDepartmentPath(deptID string) (string, error) {
	if p, ok := f.deptPaths[deptID]; ok {
		return p, nil
	}
	return "", fmt.Errorf("dept %q not found", deptID)
}

func (f *fakeDeptResolver) GetUserDepartments(userID string) ([]deptsync.Dept, error) {
	return f.userDepts[userID], nil
}

func (f *fakeDeptResolver) Configured() bool { return f.configured }

func setupDepartmentUsersDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	// Only the columns resolveRecipients' department case touches are needed.
	if err := db.Exec(`CREATE TABLE users (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		subject_id TEXT,
		username TEXT,
		casdoor_universal_id TEXT,
		deleted_at DATETIME
	)`).Error; err != nil {
		t.Fatalf("create users table: %v", err)
	}
	return db
}

func assertSameStringSet(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("set length mismatch: got %v, want %v", got, want)
	}
	counts := make(map[string]int, len(got))
	for _, g := range got {
		counts[g]++
	}
	for _, w := range want {
		if counts[w] == 0 {
			t.Fatalf("missing %q in result %v", w, got)
		}
		counts[w]--
	}
}

func TestResolveRecipients_Department(t *testing.T) {
	db := setupDepartmentUsersDB(t)
	// Local users. uid-1 -> s1, uid-2 -> s2. uid-blank has an empty subject_id
	// (never resolved a subject -> excluded). uid-3 has NO local user at all
	// (dept-sync member who never signed into costrict-web -> skipped silently).
	seed := []struct{ subject, uid string }{
		{"s1", "uid-1"},
		{"s2", "uid-2"},
		{"", "uid-blank"},
	}
	for _, u := range seed {
		if err := db.Exec(`INSERT INTO users (subject_id, username, casdoor_universal_id) VALUES (?,?,?)`, u.subject, u.subject, u.uid).Error; err != nil {
			t.Fatalf("seed user: %v", err)
		}
	}

	// Subtree members: uid-1 appears twice (same person under two sub-departments),
	// uid-2 once, uid-3 has no local user, one blank universal_id, and uid-blank maps
	// to a user whose subject_id is empty.
	members := []deptsync.DeptUser{
		{UserID: "a", UniversalID: "uid-1", DeptID: "6571"},
		{UserID: "a", UniversalID: "uid-1", DeptID: "6572"},
		{UserID: "b", UniversalID: "uid-2", DeptID: "6571"},
		{UserID: "c", UniversalID: "uid-3", DeptID: "6573"},
		{UserID: "d", UniversalID: "", DeptID: "6573"},
		{UserID: "e", UniversalID: "uid-blank", DeptID: "6571"},
	}

	// Configured + members -> deduped subject ids for matched local users only.
	resolver := &fakeDeptResolver{configured: true, members: members}
	svc := NewDistributionService(db, nil, resolver)
	got, err := svc.resolveRecipients(db, DistributionTarget{ScopeType: "department", TargetID: "6560"})
	if err != nil {
		t.Fatalf("resolveRecipients(department): %v", err)
	}
	if resolver.calledWith != "6560" {
		t.Fatalf("expected GetDeptUsersTree called with dept 6560, got %q", resolver.calledWith)
	}
	// uid-1 dup collapsed, uid-3/blank skipped, uid-blank excluded by subject_id <> ''.
	assertSameStringSet(t, got, []string{"s1", "s2"})

	// dept-sync not configured -> empty recipients (must NOT mis-fire to everyone).
	svcUnconfigured := NewDistributionService(db, nil, &fakeDeptResolver{configured: false, members: members})
	got, err = svcUnconfigured.resolveRecipients(db, DistributionTarget{ScopeType: "department", TargetID: "6560"})
	if err != nil {
		t.Fatalf("resolveRecipients(unconfigured): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty recipients when dept-sync unconfigured, got %v", got)
	}

	// nil dept-sync client -> empty recipients.
	svcNil := NewDistributionService(db, nil, nil)
	got, err = svcNil.resolveRecipients(db, DistributionTarget{ScopeType: "department", TargetID: "6560"})
	if err != nil {
		t.Fatalf("resolveRecipients(nil deptSync): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty recipients with nil dept-sync, got %v", got)
	}

	// Empty subtree -> empty recipients.
	svcEmpty := NewDistributionService(db, nil, &fakeDeptResolver{configured: true, members: nil})
	got, err = svcEmpty.resolveRecipients(db, DistributionTarget{ScopeType: "department", TargetID: "6560"})
	if err != nil {
		t.Fatalf("resolveRecipients(empty members): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty recipients for empty subtree, got %v", got)
	}
}

// seedAuthzUsers seeds the operator and target users for the structural-authorization
// tests, returning a resolver wired so that: operator "mgr" (universal "u-mgr") belongs
// to the NON-LEAF department /root/dev (so manages its subtree); "plain" belongs to a
// leaf department (manages nothing); target "ut-in" is inside /root/dev, "ut-out" is
// outside it.
func seedAuthzUsers(t *testing.T) (*gorm.DB, *fakeDeptResolver) {
	t.Helper()
	db := setupDepartmentUsersDB(t)
	seed := []struct{ subject, uid string }{
		{"mgr", "u-mgr"},     // member of the non-leaf dept /root/dev → manages it
		{"plain", "u-plain"}, // member of a leaf dept → manages nothing
		{"ut-in", "u-in"},    // a target user inside the managed subtree
		{"ut-out", "u-out"},  // a target user outside the managed subtree
	}
	for _, u := range seed {
		if err := db.Exec(`INSERT INTO users (subject_id, username, casdoor_universal_id) VALUES (?,?,?)`, u.subject, u.subject, u.uid).Error; err != nil {
			t.Fatalf("seed user: %v", err)
		}
	}
	resolver := &fakeDeptResolver{
		configured: true,
		deptPaths: map[string]string{
			"dev":   "/root/dev",
			"fe":    "/root/dev/fe",
			"sales": "/root/sales",
		},
		// /root/dev is non-leaf (has child fe); /root/sales is a leaf (no children).
		subtrees: map[string]*deptsync.Dept{
			"dev":   {DeptID: "dev", DeptName: "研发", DeptPath: "/root/dev", Children: []deptsync.Dept{{DeptID: "fe", DeptName: "前端", DeptPath: "/root/dev/fe"}}},
			"sales": {DeptID: "sales", DeptName: "销售", DeptPath: "/root/sales"},
		},
		userDepts: map[string][]deptsync.Dept{
			"u-mgr":   {{DeptID: "dev", DeptPath: "/root/dev"}},     // member of non-leaf dev
			"u-plain": {{DeptID: "sales", DeptPath: "/root/sales"}}, // member of leaf sales → no reach
			"u-in":    {{DeptID: "fe", DeptPath: "/root/dev/fe"}},
			"u-out":   {{DeptID: "sales", DeptPath: "/root/sales"}},
		},
	}
	return db, resolver
}

func TestResolveDistributionScope(t *testing.T) {
	db, resolver := seedAuthzUsers(t)
	svc := NewDistributionService(db, nil, resolver)

	// Platform admin -> unlimited, no prefixes (dept-sync never consulted).
	if unlimited, prefixes, err := svc.resolveDistributionScope("anyone", true); err != nil || !unlimited || len(prefixes) != 0 {
		t.Fatalf("platform admin: want unlimited,no-prefixes; got unlimited=%v prefixes=%v err=%v", unlimited, prefixes, err)
	}

	// Department leader -> bounded by the led department's path.
	unlimited, prefixes, err := svc.resolveDistributionScope("mgr", false)
	if err != nil || unlimited {
		t.Fatalf("leader: want bounded; got unlimited=%v err=%v", unlimited, err)
	}
	assertSameStringSet(t, prefixes, []string{"/root/dev"})

	// Non-leader user -> no prefixes (cannot distribute).
	if _, prefixes, _ := svc.resolveDistributionScope("plain", false); len(prefixes) != 0 {
		t.Fatalf("non-leader: want no prefixes, got %v", prefixes)
	}

	// dept-sync unconfigured -> fail closed (no prefixes) even for a real leader.
	svcUnconfigured := NewDistributionService(db, nil, &fakeDeptResolver{configured: false})
	if unlimited, prefixes, _ := svcUnconfigured.resolveDistributionScope("mgr", false); unlimited || len(prefixes) != 0 {
		t.Fatalf("unconfigured: want fail-closed, got unlimited=%v prefixes=%v", unlimited, prefixes)
	}

	// User with no universal-id mapping -> no prefixes.
	if _, prefixes, _ := svc.resolveDistributionScope("ghost", false); len(prefixes) != 0 {
		t.Fatalf("unmapped user: want no prefixes, got %v", prefixes)
	}
}

func TestCanDistribute(t *testing.T) {
	db, resolver := seedAuthzUsers(t)
	svc := NewDistributionService(db, nil, resolver)

	if !svc.CanDistribute(nil, "anyone", true) {
		t.Fatalf("platform admin should be able to distribute")
	}
	if !svc.CanDistribute(nil, "mgr", false) {
		t.Fatalf("department leader should be able to distribute")
	}
	if svc.CanDistribute(nil, "plain", false) {
		t.Fatalf("non-leader should NOT be able to distribute")
	}
}

func TestAuthorizeTargets(t *testing.T) {
	db, resolver := seedAuthzUsers(t)
	svc := NewDistributionService(db, nil, resolver)

	// Platform admin: user/department targets pass, including cross-subtree dept.
	if err := svc.AuthorizeTargets("anyone", true, []DistributionTarget{
		{ScopeType: "user", TargetID: "ut-out"},
		{ScopeType: "department", TargetID: "sales"},
	}); err != nil {
		t.Fatalf("platform admin should pass user/department targets, got %v", err)
	}
	if err := svc.AuthorizeTargets("anyone", true, []DistributionTarget{
		{ScopeType: "organization", TargetID: "ACME"},
	}); !errors.Is(err, ErrUnsupportedScope) {
		t.Fatalf("organization for platform admin: want ErrUnsupportedScope, got %v", err)
	}

	// Leader: department inside subtree (self + descendant) passes.
	if err := svc.AuthorizeTargets("mgr", false, []DistributionTarget{
		{ScopeType: "department", TargetID: "dev"},
		{ScopeType: "department", TargetID: "fe"},
	}); err != nil {
		t.Fatalf("leader in-subtree departments should pass, got %v", err)
	}

	// Leader: a department outside the subtree is rejected.
	if err := svc.AuthorizeTargets("mgr", false, []DistributionTarget{
		{ScopeType: "department", TargetID: "sales"},
	}); !errors.Is(err, ErrTargetOutOfScope) {
		t.Fatalf("out-of-subtree department: want ErrTargetOutOfScope, got %v", err)
	}

	// Leader: a user inside the subtree passes; a user outside is rejected.
	if err := svc.AuthorizeTargets("mgr", false, []DistributionTarget{
		{ScopeType: "user", TargetID: "ut-in"},
	}); err != nil {
		t.Fatalf("in-subtree user should pass, got %v", err)
	}
	if err := svc.AuthorizeTargets("mgr", false, []DistributionTarget{
		{ScopeType: "user", TargetID: "ut-out"},
	}); !errors.Is(err, ErrTargetOutOfScope) {
		t.Fatalf("out-of-subtree user: want ErrTargetOutOfScope, got %v", err)
	}

	// Leader: organization scope is no longer a supported distribution target.
	if err := svc.AuthorizeTargets("mgr", false, []DistributionTarget{
		{ScopeType: "organization", TargetID: "ACME"},
	}); !errors.Is(err, ErrUnsupportedScope) {
		t.Fatalf("organization for leader: want ErrUnsupportedScope, got %v", err)
	}

	// Atomicity: one out-of-scope target in a batch rejects the whole request.
	if err := svc.AuthorizeTargets("mgr", false, []DistributionTarget{
		{ScopeType: "department", TargetID: "fe"},
		{ScopeType: "department", TargetID: "sales"},
	}); !errors.Is(err, ErrTargetOutOfScope) {
		t.Fatalf("mixed batch: want ErrTargetOutOfScope, got %v", err)
	}

	// Non-leader (no managed prefixes) is rejected with ErrCannotDistribute.
	if err := svc.AuthorizeTargets("plain", false, []DistributionTarget{
		{ScopeType: "department", TargetID: "dev"},
	}); !errors.Is(err, ErrCannotDistribute) {
		t.Fatalf("non-leader: want ErrCannotDistribute, got %v", err)
	}
}

func TestResolveDistributionAuthority(t *testing.T) {
	db, resolver := seedAuthzUsers(t)
	svc := NewDistributionService(db, nil, resolver)

	// Platform admin -> unlimited, empty (frontend uses full tree).
	auth, err := svc.ResolveDistributionAuthority("anyone", true)
	if err != nil || auth == nil || !auth.Unlimited || len(auth.Departments) != 0 {
		t.Fatalf("admin authority: want unlimited+empty, got %+v err=%v", auth, err)
	}

	// Leader -> not unlimited, lists led departments.
	auth, err = svc.ResolveDistributionAuthority("mgr", false)
	if err != nil || auth == nil || auth.Unlimited || len(auth.Departments) != 1 || auth.Departments[0].DeptPath != "/root/dev" {
		t.Fatalf("leader authority: want bounded with /root/dev, got %+v err=%v", auth, err)
	}

	// Non-leader -> not unlimited, no departments (no entry).
	auth, err = svc.ResolveDistributionAuthority("plain", false)
	if err != nil || auth == nil || auth.Unlimited || len(auth.Departments) != 0 {
		t.Fatalf("non-leader authority: want bounded+empty, got %+v err=%v", auth, err)
	}

	// dept-sync unconfigured -> non-admin gets empty authority.
	svcUnconfigured := NewDistributionService(db, nil, &fakeDeptResolver{configured: false})
	auth, err = svcUnconfigured.ResolveDistributionAuthority("mgr", false)
	if err != nil || auth == nil || auth.Unlimited || len(auth.Departments) != 0 {
		t.Fatalf("unconfigured authority: want bounded+empty, got %+v err=%v", auth, err)
	}
}

func TestListReceipts(t *testing.T) {
	db := setupDistributionServiceDB(t)
	now := time.Now()
	receipts := []models.ItemDistributionReceipt{
		{ID: "r1", DistributionID: "d1", UserID: "u1", ReceiptStatus: "unread", CreatedAt: now.Add(-2 * time.Hour)},
		{ID: "r2", DistributionID: "d1", UserID: "u2", ReceiptStatus: "read", CreatedAt: now.Add(-1 * time.Hour)},
		{ID: "r3", DistributionID: "d2", UserID: "u3", ReceiptStatus: "dismissed", CreatedAt: now},
	}
	for _, r := range receipts {
		if err := db.Create(&r).Error; err != nil {
			t.Fatalf("seed receipt: %v", err)
		}
	}
	svc := NewDistributionService(db, nil, nil)

	got, err := svc.ListReceipts(context.Background(), "d1")
	if err != nil {
		t.Fatalf("list receipts: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 receipts for d1, got %d", len(got))
	}
	// created_at DESC -> r2 then r1
	if got[0].ID != "r2" {
		t.Fatalf("expected r2 first, got %s", got[0].ID)
	}
}

func TestGetEffectivePermission(t *testing.T) {
	db := setupDistributionServiceDB(t)
	now := time.Now()
	// item-1: active distribution (dismissible) with a read receipt for u1.
	// item-2: active distribution (readonly) but u1's receipt is dismissed.
	// item-3: a paused distribution (should not count).
	dists := []models.ItemDistribution{
		{ID: "d1", ItemID: "item-1", DistributorID: "admin-a", PermissionMode: "dismissible", Status: "active", ScopeType: "user", TargetID: "u1", CreatedAt: now.Add(-2 * time.Hour)},
		{ID: "d2", ItemID: "item-2", DistributorID: "admin-a", PermissionMode: "readonly", Status: "active", ScopeType: "user", TargetID: "u1", CreatedAt: now.Add(-1 * time.Hour)},
		{ID: "d3", ItemID: "item-3", DistributorID: "admin-a", PermissionMode: "readonly", Status: "paused", ScopeType: "user", TargetID: "u1", CreatedAt: now},
	}
	for _, d := range dists {
		if err := db.Create(&d).Error; err != nil {
			t.Fatalf("seed distribution: %v", err)
		}
	}
	receipts := []models.ItemDistributionReceipt{
		{ID: "r1", DistributionID: "d1", UserID: "u1", ReceiptStatus: "read", CreatedAt: now},
		{ID: "r2", DistributionID: "d2", UserID: "u1", ReceiptStatus: "dismissed", CreatedAt: now},
		{ID: "r3", DistributionID: "d3", UserID: "u1", ReceiptStatus: "read", CreatedAt: now},
	}
	for _, r := range receipts {
		if err := db.Create(&r).Error; err != nil {
			t.Fatalf("seed receipt: %v", err)
		}
	}
	svc := NewDistributionService(db, nil, nil)
	ctx := context.Background()

	// item-1: active + non-dismissed -> returns the actual permission_mode.
	mode, ok := svc.GetEffectivePermission(ctx, "item-1", "u1")
	if !ok || mode != "dismissible" {
		t.Fatalf("item-1: expected (dismissible,true), got (%q,%v)", mode, ok)
	}

	// item-2: receipt dismissed -> no effective permission.
	if mode, ok := svc.GetEffectivePermission(ctx, "item-2", "u1"); ok || mode != "" {
		t.Fatalf("item-2 (dismissed): expected (\"\",false), got (%q,%v)", mode, ok)
	}

	// item-3: distribution paused -> no effective permission.
	if mode, ok := svc.GetEffectivePermission(ctx, "item-3", "u1"); ok || mode != "" {
		t.Fatalf("item-3 (paused): expected (\"\",false), got (%q,%v)", mode, ok)
	}

	// unknown item -> no effective permission.
	if mode, ok := svc.GetEffectivePermission(ctx, "item-none", "u1"); ok || mode != "" {
		t.Fatalf("item-none: expected (\"\",false), got (%q,%v)", mode, ok)
	}
}
