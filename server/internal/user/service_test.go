package user

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/golang-jwt/jwt/v5"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func signUserTestJWT(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tokenString, err := token.SignedString(key)
	if err != nil {
		t.Fatalf("sign jwt: %v", err)
	}
	return tokenString
}

func setupUserTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("failed to open test db: %v", err)
	}

	if err := db.AutoMigrate(&models.User{}, &models.UserAuthIdentity{}); err != nil {
		t.Fatalf("failed to migrate user table: %v", err)
	}

	return db
}

func TestUserServiceGetUserByID(t *testing.T) {
	db := setupUserTestDB(t)
	svc := NewUserService(db)

	user := models.User{SubjectID: "u1", Username: "alice", IsActive: true}
	if err := db.Create(&user).Error; err != nil {
		t.Fatalf("seed user: %v", err)
	}

	got, err := svc.GetUserByID(context.Background(), "u1")
	if err != nil {
		t.Fatalf("GetUserByID error: %v", err)
	}
	if got.SubjectID != "u1" || got.Username != "alice" {
		t.Fatalf("unexpected user: %+v", got)
	}
}

func TestUserServiceGetUsersByIDs(t *testing.T) {
	db := setupUserTestDB(t)
	svc := NewUserService(db)

	seed := []models.User{
		{SubjectID: "u1", Username: "alice", IsActive: true},
		{SubjectID: "u2", Username: "bob", IsActive: true},
	}
	for _, u := range seed {
		if err := db.Create(&u).Error; err != nil {
			t.Fatalf("seed user: %v", err)
		}
	}

	got, err := svc.GetUsersByIDs(context.Background(), []string{"u1", "u2", "u3"})
	if err != nil {
		t.Fatalf("GetUsersByIDs error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 users, got %d", len(got))
	}
}

func TestUserServiceGetUsersByUniversalIDs(t *testing.T) {
	db := setupUserTestDB(t)
	svc := NewUserService(db)

	uuid1 := "uuid-u1"
	uuid2 := "uuid-u2"
	seed := []models.User{
		{SubjectID: "u1", Username: "alice", CasdoorUniversalID: &uuid1, IsActive: true},
		{SubjectID: "u2", Username: "bob", CasdoorUniversalID: &uuid2, IsActive: true},
	}
	for _, u := range seed {
		if err := db.Create(&u).Error; err != nil {
			t.Fatalf("seed user: %v", err)
		}
	}

	got, err := svc.GetUsersByUniversalIDs([]string{"uuid-u1", "uuid-u2", "uuid-u3"})
	if err != nil {
		t.Fatalf("GetUsersByUniversalIDs error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 users, got %d", len(got))
	}
	if got["uuid-u1"] == nil || got["uuid-u1"].SubjectID != "u1" {
		t.Fatalf("expected uuid-u1 -> u1, got %+v", got["uuid-u1"])
	}
	if got["uuid-u2"] == nil || got["uuid-u2"].SubjectID != "u2" {
		t.Fatalf("expected uuid-u2 -> u2, got %+v", got["uuid-u2"])
	}
	if _, ok := got["uuid-u3"]; ok {
		t.Fatalf("did not expect uuid-u3 in result")
	}
}

func TestUserServiceSearchUsers(t *testing.T) {
	db := setupUserTestDB(t)
	svc := NewUserService(db)

	display := "Alice Smith"
	email := "alice@example.com"
	seed := []models.User{
		{SubjectID: "u1", Username: "alice", DisplayName: &display, Email: &email, IsActive: true},
		{SubjectID: "u2", Username: "bob", IsActive: true},
		{SubjectID: "u3", Username: "inactive", IsActive: false},
	}
	for _, u := range seed {
		if err := db.Create(&u).Error; err != nil {
			t.Fatalf("seed user: %v", err)
		}
	}

	got, err := svc.SearchUsers(context.Background(), "alice", 20)
	if err != nil {
		t.Fatalf("SearchUsers error: %v", err)
	}
	if len(got) != 1 || got[0].SubjectID != "u1" {
		t.Fatalf("unexpected search result: %+v", got)
	}
}

func TestUserServiceGetOrCreateUserCreate(t *testing.T) {
	db := setupUserTestDB(t)
	svc := NewUserService(db)

	claims := &JWTClaims{
		ID:                "u1",
		Sub:               "org/alice",
		UniversalID:       "uuid-u1",
		Name:              "alice",
		PreferredUsername: "Alice",
		Email:             "alice@example.com",
		Picture:           "https://example.com/a.png",
		Owner:             "org",
	}

	user, _, err := svc.GetOrCreateUser(context.Background(),claims)
	if err != nil {
		t.Fatalf("GetOrCreateUser create error: %v", err)
	}
	if user.SubjectID == "" || user.SubjectID == "uuid-u1" || user.SubjectID == "org/alice" || user.SubjectID == "u1" || user.Username != "alice" {
		t.Fatalf("unexpected created user: %+v", user)
	}
	if len(user.SubjectID) < 5 || user.SubjectID[:4] != "usr_" {
		t.Fatalf("expected local subject_id with usr_ prefix, got %+v", user)
	}
	if user.ExternalKey == nil || *user.ExternalKey != "casdoor:uuid-u1" {
		t.Fatalf("external_key not set: %+v", user)
	}
}

func TestUserServiceGetOrCreateUserUpdate(t *testing.T) {
	db := setupUserTestDB(t)
	svc := NewUserService(db)

	oldName := "Old Name"
	oldEmail := "old@example.com"
	now := time.Now().Add(-time.Hour)
	externalKey := "casdoor:uuid-u1"
	seed := models.User{
		SubjectID:   "legacy-u1",
		Username:    "alice",
		DisplayName: &oldName,
		Email:       &oldEmail,
		ExternalKey: &externalKey,
		IsActive:    false,
		LastLoginAt: &now,
	}
	if err := db.Create(&seed).Error; err != nil {
		t.Fatalf("seed user: %v", err)
	}

	claims := &JWTClaims{
		ID:                "u1",
		Sub:               "org/alice",
		UniversalID:       "uuid-u1",
		Name:              "alice",
		PreferredUsername: "Alice New",
		Email:             "new@example.com",
		Picture:           "https://example.com/a.png",
		Owner:             "org",
	}

	user, _, err := svc.GetOrCreateUser(context.Background(),claims)
	if err != nil {
		t.Fatalf("GetOrCreateUser update error: %v", err)
	}
	if user.SubjectID != "legacy-u1" {
		t.Fatalf("existing local subject_id should remain unchanged: %+v", user)
	}
	if user.DisplayName == nil || *user.DisplayName != oldName {
		t.Fatalf("display name should NOT be overwritten on re-login (user-owned): got %+v", user.DisplayName)
	}
	if user.Email == nil || *user.Email != oldEmail {
		t.Fatalf("email should NOT be overwritten on re-login (user-owned): got %+v", user.Email)
	}
	if !user.IsActive {
		t.Fatal("expected user to be active")
	}
	if user.ExternalKey == nil || *user.ExternalKey != "casdoor:uuid-u1" {
		t.Fatalf("external_key not backfilled: %+v", user)
	}
}

func TestUserServiceGetOrCreateUserMatchesByExternalKey(t *testing.T) {
	db := setupUserTestDB(t)
	svc := NewUserService(db)

	externalKey := "casdoor:uuid-u1"
	provider := "Github"
	seed := models.User{
		SubjectID:   "legacy-u1",
		Username:    "alice",
		ExternalKey: &externalKey,
		AuthProvider: &provider,
		IsActive:    true,
	}
	if err := db.Create(&seed).Error; err != nil {
		t.Fatalf("seed user: %v", err)
	}

	claims := &JWTClaims{
		ID:                "new-id",
		Sub:               "new-sub",
		UniversalID:       "uuid-u1",
		Name:              "alice-gh",
		PreferredUsername: "Alice GH",
		Provider:          "Github",
		ProviderUserID:    "18633160",
	}

	user, _, err := svc.GetOrCreateUser(context.Background(),claims)
	if err != nil {
		t.Fatalf("GetOrCreateUser error: %v", err)
	}
	if err := svc.BindIdentityToUser(context.Background(),user.SubjectID, claims); err != nil {
		t.Fatalf("bind identity: %v", err)
	}
	if user.SubjectID != "legacy-u1" {
		t.Fatalf("expected match by external key, got %+v", user)
	}
	if user.ProviderUserID == nil || *user.ProviderUserID != "18633160" {
		t.Fatalf("provider_user_id not updated: %+v", user)
	}
	if user.ExternalKey == nil || *user.ExternalKey != "casdoor:github:uuid-u1" {
		t.Fatalf("external_key not upgraded to provider-aware format: %+v", user)
	}
}

func TestUserServiceGetOrCreateUserKeepsLocalSubjectIDAcrossLogins(t *testing.T) {
	db := setupUserTestDB(t)
	svc := NewUserService(db)

	claims := &JWTClaims{
		ID:                "u1",
		Sub:               "org/alice",
		UniversalID:       "uuid-u1",
		Name:              "alice",
		PreferredUsername: "Alice",
		Email:             "alice@example.com",
	}

	first, _, err := svc.GetOrCreateUser(context.Background(),claims)
	if err != nil {
		t.Fatalf("first GetOrCreateUser error: %v", err)
	}
	second, _, err := svc.GetOrCreateUser(context.Background(),claims)
	if err != nil {
		t.Fatalf("second GetOrCreateUser error: %v", err)
	}
	if first.SubjectID == "" || len(first.SubjectID) < 5 || first.SubjectID[:4] != "usr_" {
		t.Fatalf("expected first local subject_id, got %+v", first)
	}
	if second.SubjectID != first.SubjectID {
		t.Fatalf("expected stable local subject_id across logins, got first=%s second=%s", first.SubjectID, second.SubjectID)
	}
}

func TestCachedUserServiceCacheFlow(t *testing.T) {
	db := setupUserTestDB(t)
	svc := NewCachedUserService(NewUserService(db))
	user := models.User{SubjectID: "u1", Username: "alice", IsActive: true}
	if err := db.Create(&user).Error; err != nil {
		t.Fatalf("seed user: %v", err)
	}

	got1, err := svc.GetUserByID(context.Background(), "u1")
	if err != nil {
		t.Fatalf("first GetUserByID error: %v", err)
	}
	if got1.SubjectID != "u1" {
		t.Fatalf("unexpected user: %+v", got1)
	}

	if err := db.Delete(&models.User{}, "subject_id = ?", "u1").Error; err != nil {
		t.Fatalf("delete user: %v", err)
	}

	got2, err := svc.GetUserByID(context.Background(), "u1")
	if err != nil {
		t.Fatalf("cached GetUserByID error: %v", err)
	}
	if got2.SubjectID != "u1" {
		t.Fatalf("unexpected cached user: %+v", got2)
	}

	svc.InvalidateCache("u1")
	if _, err := svc.GetUserByID(context.Background(), "u1"); err == nil {
		t.Fatal("expected error after cache invalidation and db delete")
	}
}

func TestCachedUserServiceGetUsersByIDs(t *testing.T) {
	db := setupUserTestDB(t)
	svc := NewCachedUserService(NewUserService(db))
	seed := []models.User{
		{SubjectID: "u1", Username: "alice", IsActive: true},
		{SubjectID: "u2", Username: "bob", IsActive: true},
		{SubjectID: "u3", Username: "inactive", IsActive: false},
	}
	for _, u := range seed {
		if err := db.Create(&u).Error; err != nil {
			t.Fatalf("seed user: %v", err)
		}
	}

	got, err := svc.GetUsersByIDs(context.Background(), []string{"u1", "u2", "u9"})
	if err != nil {
		t.Fatalf("GetUsersByIDs error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 users, got %d", len(got))
	}
}

func TestBindIdentityToUserCreatesSecondaryIdentityAndPromotesByRank(t *testing.T) {
	db := setupUserTestDB(t)
	svc := NewUserService(db)

	phoneClaims := &JWTClaims{ID: "phone-id", Sub: "phone-sub", UniversalID: "phone-uuid", Name: "phone_15500000001", PreferredUsername: "ph_15500000001", Provider: "phone", Phone: "15500000001"}
	user, _, err := svc.GetOrCreateUser(context.Background(),phoneClaims)
	if err != nil {
		t.Fatalf("create phone user: %v", err)
	}
	if err := svc.BindIdentityToUser(context.Background(),user.SubjectID, phoneClaims); err != nil {
		t.Fatalf("bind phone identity: %v", err)
	}

	githubClaims := &JWTClaims{ID: "gh-id", Sub: "gh-sub", UniversalID: "gh-uuid", Name: "acct_github_user", PreferredUsername: "Display Github User", Provider: "github", ProviderUserID: "provider-gh-001", Picture: "https://avatars.example.com/a.png"}
	if err := svc.BindIdentityToUser(context.Background(),user.SubjectID, githubClaims); err != nil {
		t.Fatalf("bind github identity: %v", err)
	}

	identities, err := svc.ListUserIdentities(context.Background(), user.SubjectID)
	if err != nil {
		t.Fatalf("list identities: %v", err)
	}
	if len(identities) != 2 {
		t.Fatalf("expected 2 identities, got %d", len(identities))
	}
	primaryCount := 0
	for _, identity := range identities {
		if identity.IsPrimary {
			primaryCount++
			if identity.Provider != "github" {
				t.Fatalf("expected github to be promoted primary, got %+v", identity)
			}
		}
	}
	if primaryCount != 1 {
		t.Fatalf("expected exactly 1 primary identity, got %d", primaryCount)
	}
	refreshed, err := svc.GetUserByID(context.Background(), user.SubjectID)
	if err != nil {
		t.Fatalf("reload user: %v", err)
	}
	if refreshed.AuthProvider == nil || *refreshed.AuthProvider != "github" {
		t.Fatalf("expected user auth_provider upgraded to github, got %+v", refreshed)
	}
}

func TestUnbindIdentityReassignsPrimary(t *testing.T) {
	db := setupUserTestDB(t)
	svc := NewUserService(db)

	user, _, err := svc.GetOrCreateUser(context.Background(),&JWTClaims{ID: "gh-id", Sub: "gh-sub", UniversalID: "gh-uuid", Name: "acct_github_user", PreferredUsername: "Display Github User", Provider: "github", ProviderUserID: "provider-gh-001"})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	// Explicitly bind the github identity since GetOrCreateUser might not auto-bind
	if err := svc.BindIdentityToUser(context.Background(),user.SubjectID, &JWTClaims{ID: "gh-id", Sub: "gh-sub", UniversalID: "gh-uuid", Name: "acct_github_user", PreferredUsername: "Display Github User", Provider: "github", ProviderUserID: "provider-gh-001"}); err != nil {
		t.Fatalf("bind github identity: %v", err)
	}
	if err := svc.BindIdentityToUser(context.Background(),user.SubjectID, &JWTClaims{ID: "phone-id", Sub: "phone-sub", UniversalID: "phone-uuid", Name: "phone_15500000001", PreferredUsername: "ph_15500000001", Provider: "phone", Phone: "15500000001"}); err != nil {
		t.Fatalf("bind phone identity: %v", err)
	}
	identities, _ := svc.ListUserIdentities(context.Background(), user.SubjectID)
	var githubIdentityID uint
	for _, identity := range identities {
		if identity.Provider == "github" {
			githubIdentityID = identity.ID
		}
	}
	if githubIdentityID == 0 {
		t.Fatal("expected github identity to exist")
	}
	if err := svc.UnbindIdentityByProvider(context.Background(),user.SubjectID, "github"); err != nil {
		t.Fatalf("unbind github identity: %v", err)
	}
	identities, _ = svc.ListUserIdentities(context.Background(), user.SubjectID)
	if len(identities) != 1 || !identities[0].IsPrimary || identities[0].Provider != "phone" {
		t.Fatalf("expected remaining phone identity to become primary, got %+v", identities)
	}
}

// TestGetOrCreateUser_SameUniversalID_DifferentProvider_CreatesSeparateUsers
// pins the new identity contract: universal_id is provider-scoped, so two
// identities with the same universal_id but different providers resolve to
// different external_keys (`casdoor:github:<uuid>` vs `casdoor:phone:<uuid>`)
// and therefore to different users. The old universal_id-only fallback that
// auto-bound them to the same user was removed when the legacy
// casdoor_universal_id column was dropped from cs-user, because that
// behavior violated the "identity is provider-scoped" invariant and would
// silently merge unrelated accounts whenever two providers happened to reuse
// the same universal_id.
func TestGetOrCreateUser_SameUniversalID_DifferentProvider_CreatesSeparateUsers(t *testing.T) {
	db := setupUserTestDB(t)
	svc := NewUserService(db)

	githubClaims := &JWTClaims{
		ID:                "gh-id",
		Sub:               "gh-sub",
		UniversalID:       "shared-uuid",
		Name:              "gh_user",
		PreferredUsername: "GH User",
		Provider:          "github",
		ProviderUserID:    "gh-001",
		Email:             "gh@example.com",
		Picture:           "https://avatars.example.com/gh.png",
	}

	ghUser, _, err := svc.GetOrCreateUser(context.Background(), githubClaims)
	if err != nil {
		t.Fatalf("create github user: %v", err)
	}
	if err := svc.BindIdentityToUser(context.Background(), ghUser.SubjectID, githubClaims); err != nil {
		t.Fatalf("bind github identity: %v", err)
	}

	identities, _ := svc.ListUserIdentities(context.Background(), ghUser.SubjectID)
	if len(identities) != 1 {
		t.Fatalf("expected 1 identity after github login, got %d", len(identities))
	}
	if identities[0].Provider != "github" {
		t.Fatalf("expected github identity, got %s", identities[0].Provider)
	}

	phoneClaims := &JWTClaims{
		ID:                "phone-id",
		Sub:               "phone-sub",
		UniversalID:       "shared-uuid",
		Name:              "phone_15500000001",
		PreferredUsername: "ph_15500000001",
		Provider:          "phone",
		Phone:             "15500000001",
	}

	phoneUser, _, err := svc.GetOrCreateUser(context.Background(), phoneClaims)
	if err != nil {
		t.Fatalf("get or create phone user: %v", err)
	}
	if err := svc.BindIdentityToUser(context.Background(), phoneUser.SubjectID, phoneClaims); err != nil {
		t.Fatalf("bind phone identity: %v", err)
	}
	if phoneUser.SubjectID == ghUser.SubjectID {
		t.Fatalf("different providers must NOT auto-merge even when universal_id matches: github=%s phone=%s", ghUser.SubjectID, phoneUser.SubjectID)
	}

	// Each user should have exactly one identity (their own provider).
	ghIdentities, _ := svc.ListUserIdentities(context.Background(), ghUser.SubjectID)
	if len(ghIdentities) != 1 || ghIdentities[0].Provider != "github" {
		t.Fatalf("github user should have only github identity, got %+v", ghIdentities)
	}
	phoneIdentities, _ := svc.ListUserIdentities(context.Background(), phoneUser.SubjectID)
	if len(phoneIdentities) != 1 || phoneIdentities[0].Provider != "phone" {
		t.Fatalf("phone user should have only phone identity, got %+v", phoneIdentities)
	}
}

func TestGetOrCreateUserLegacyExternalKeyFallback(t *testing.T) {
	db := setupUserTestDB(t)
	svc := NewUserService(db)

	legacyKey := "casdoor:shared-uuid"
	provider := "github"
	seed := models.User{
		SubjectID:   "legacy-u1",
		Username:    "gh_user",
		ExternalKey: &legacyKey,
		AuthProvider: &provider,
		IsActive:    true,
	}
	if err := db.Create(&seed).Error; err != nil {
		t.Fatalf("seed user: %v", err)
	}
	seedIdentity := models.UserAuthIdentity{
		UserSubjectID: "legacy-u1",
		Provider:      "github",
		ExternalKey:   legacyKey,
		IsPrimary:     true,
	}
	if err := db.Create(&seedIdentity).Error; err != nil {
		t.Fatalf("seed identity: %v", err)
	}

	claims := &JWTClaims{
		ID:                "gh-id",
		Sub:               "gh-sub",
		UniversalID:       "shared-uuid",
		Name:              "gh_user",
		PreferredUsername: "GH User",
		Provider:          "github",
		ProviderUserID:    "gh-001",
	}

	user, _, err := svc.GetOrCreateUser(context.Background(),claims)
	if err != nil {
		t.Fatalf("GetOrCreateUser error: %v", err)
	}
	if err := svc.BindIdentityToUser(context.Background(),user.SubjectID, claims); err != nil {
		t.Fatalf("bind identity: %v", err)
	}
	if user.SubjectID != "legacy-u1" {
		t.Fatalf("expected match by legacy external_key, got %+v", user)
	}

	identities, _ := svc.ListUserIdentities(context.Background(), user.SubjectID)
	if len(identities) != 1 {
		t.Fatalf("expected 1 identity after legacy fallback, got %d", len(identities))
	}
	if identities[0].ExternalKey != "casdoor:github:shared-uuid" {
		t.Fatalf("expected identity external_key upgraded to provider-aware format, got %s", identities[0].ExternalKey)
	}
}

func TestUnbindIdentitySetsExplicitlyUnbound(t *testing.T) {
	db := setupUserTestDB(t)
	svc := NewUserService(db)

	// Create user with github + phone identities
	user, _, err := svc.GetOrCreateUser(context.Background(),&JWTClaims{ID: "gh-id", Sub: "gh-sub", UniversalID: "gh-uuid", Name: "acct_github_user", PreferredUsername: "Display Github User", Provider: "github", ProviderUserID: "provider-gh-001"})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := svc.BindIdentityToUser(context.Background(),user.SubjectID, &JWTClaims{ID: "gh-id", Sub: "gh-sub", UniversalID: "gh-uuid", Name: "acct_github_user", PreferredUsername: "Display Github User", Provider: "github", ProviderUserID: "provider-gh-001"}); err != nil {
		t.Fatalf("bind github identity: %v", err)
	}
	if err := svc.BindIdentityToUser(context.Background(),user.SubjectID, &JWTClaims{ID: "phone-id", Sub: "phone-sub", UniversalID: "phone-uuid", Name: "phone_15500000001", PreferredUsername: "ph_15500000001", Provider: "phone", Phone: "15500000001"}); err != nil {
		t.Fatalf("bind phone identity: %v", err)
	}

	// Unbind github identity
	if err := svc.UnbindIdentityByProvider(context.Background(),user.SubjectID, "github"); err != nil {
		t.Fatalf("unbind github identity: %v", err)
	}

	// Verify the identity is soft-deleted and explicitly_unbound is set
	var unboundIdentity models.UserAuthIdentity
	if err := db.Unscoped().Where("user_subject_id = ? AND provider = ?", user.SubjectID, "github").First(&unboundIdentity).Error; err != nil {
		t.Fatalf("find unbound identity: %v", err)
	}

	if !unboundIdentity.ExplicitlyUnbound {
		t.Fatalf("expected explicitly_unbound to be true, got false")
	}
	if !unboundIdentity.DeletedAt.Valid {
		t.Fatalf("expected deleted_at to be set (soft delete), got zero time")
	}

	// Verify unbound identity doesn't appear in ListUserIdentities
	identities, _ := svc.ListUserIdentities(context.Background(), user.SubjectID)
	if len(identities) != 1 {
		t.Fatalf("expected 1 identity in list (excluding unbound), got %d", len(identities))
	}
	if identities[0].Provider != "phone" {
		t.Fatalf("expected remaining provider to be phone, got %s", identities[0].Provider)
	}
}

func TestBindIdentityToUserSkipsExplicitlyUnbound(t *testing.T) {
	db := setupUserTestDB(t)
	svc := NewUserService(db)

	// Create user with github + phone identities
	user, _, err := svc.GetOrCreateUser(context.Background(),&JWTClaims{ID: "gh-id", Sub: "gh-sub", UniversalID: "gh-uuid", Name: "acct_github_user", PreferredUsername: "Display Github User", Provider: "github", ProviderUserID: "provider-gh-001"})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := svc.BindIdentityToUser(context.Background(),user.SubjectID, &JWTClaims{ID: "gh-id", Sub: "gh-sub", UniversalID: "gh-uuid", Name: "acct_github_user", PreferredUsername: "Display Github User", Provider: "github", ProviderUserID: "provider-gh-001"}); err != nil {
		t.Fatalf("bind github identity: %v", err)
	}
	if err := svc.BindIdentityToUser(context.Background(),user.SubjectID, &JWTClaims{ID: "phone-id", Sub: "phone-sub", UniversalID: "phone-uuid", Name: "phone_15500000001", PreferredUsername: "ph_15500000001", Provider: "phone", Phone: "15500000001"}); err != nil {
		t.Fatalf("bind phone identity: %v", err)
	}

	// Unbind github identity
	if err := svc.UnbindIdentityByProvider(context.Background(),user.SubjectID, "github"); err != nil {
		t.Fatalf("unbind github identity: %v", err)
	}

	// Simulate concurrent request with old JWT token trying to rebind github
	githubClaims := &JWTClaims{ID: "gh-id", Sub: "gh-sub", UniversalID: "gh-uuid", Name: "acct_github_user", PreferredUsername: "Display Github User", Provider: "github", ProviderUserID: "provider-gh-001"}
	if err := svc.BindIdentityToUser(context.Background(),user.SubjectID, githubClaims); err != nil {
		t.Fatalf("BindIdentityToUser should not error on explicitly unbound identity, got: %v", err)
	}

	// Verify the identity remains soft-deleted and explicitly_unbound
	var unboundIdentity models.UserAuthIdentity
	if err := db.Unscoped().Where("user_subject_id = ? AND provider = ?", user.SubjectID, "github").First(&unboundIdentity).Error; err != nil {
		t.Fatalf("find unbound identity: %v", err)
	}

	if !unboundIdentity.ExplicitlyUnbound {
		t.Fatalf("expected explicitly_unbound to remain true after re-bind attempt, got false")
	}
	if !unboundIdentity.DeletedAt.Valid {
		t.Fatalf("expected deleted_at to remain set after re-bind attempt, got zero time")
	}

	// Verify identity still doesn't appear in ListUserIdentities
	identities, _ := svc.ListUserIdentities(context.Background(), user.SubjectID)
	if len(identities) != 1 {
		t.Fatalf("expected 1 identity in list (re-binding should be prevented), got %d", len(identities))
	}
	if identities[0].Provider != "phone" {
		t.Fatalf("expected remaining provider to be phone, got %s", identities[0].Provider)
	}
}

func TestGetOrCreateUserDoesNotRebindExplicitlyUnbound(t *testing.T) {
	db := setupUserTestDB(t)
	svc := NewUserService(db)

	// Create user with github + phone identities
	user, _, err := svc.GetOrCreateUser(context.Background(),&JWTClaims{ID: "gh-id", Sub: "gh-sub", UniversalID: "gh-uuid", Name: "acct_github_user", PreferredUsername: "Display Github User", Provider: "github", ProviderUserID: "provider-gh-001"})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := svc.BindIdentityToUser(context.Background(),user.SubjectID, &JWTClaims{ID: "gh-id", Sub: "gh-sub", UniversalID: "gh-uuid", Name: "acct_github_user", PreferredUsername: "Display Github User", Provider: "github", ProviderUserID: "provider-gh-001"}); err != nil {
		t.Fatalf("bind github identity: %v", err)
	}
	if err := svc.BindIdentityToUser(context.Background(),user.SubjectID, &JWTClaims{ID: "phone-id", Sub: "phone-sub", UniversalID: "phone-uuid", Name: "phone_15500000001", PreferredUsername: "ph_15500000001", Provider: "phone", Phone: "15500000001"}); err != nil {
		t.Fatalf("bind phone identity: %v", err)
	}

	// Unbind github identity
	if err := svc.UnbindIdentityByProvider(context.Background(),user.SubjectID, "github"); err != nil {
		t.Fatalf("unbind github identity: %v", err)
	}

	// Simulate login callback with old github JWT token
	githubClaims := &JWTClaims{ID: "gh-id", Sub: "gh-sub", UniversalID: "gh-uuid", Name: "acct_github_user", PreferredUsername: "Display Github User", Provider: "github", ProviderUserID: "provider-gh-001"}
	_, _, err = svc.GetOrCreateUser(context.Background(),githubClaims)
	if err != nil {
		t.Fatalf("GetOrCreateUser should not error on explicitly unbound identity, got: %v", err)
	}

	// Verify the identity remains soft-deleted and explicitly_unbound
	var unboundIdentity models.UserAuthIdentity
	if err := db.Unscoped().Where("user_subject_id = ? AND provider = ?", user.SubjectID, "github").First(&unboundIdentity).Error; err != nil {
		t.Fatalf("find unbound identity: %v", err)
	}

	if !unboundIdentity.ExplicitlyUnbound {
		t.Fatalf("expected explicitly_unbound to remain true after GetOrCreateUser, got false")
	}
	if !unboundIdentity.DeletedAt.Valid {
		t.Fatalf("expected deleted_at to remain set after GetOrCreateUser, got zero time")
	}

	// Verify identity still doesn't appear in ListUserIdentities
	identities, _ := svc.ListUserIdentities(context.Background(), user.SubjectID)
	if len(identities) != 1 {
		t.Fatalf("expected 1 identity in list (GetOrCreateUser should not rebind), got %d", len(identities))
	}
	if identities[0].Provider != "phone" {
		t.Fatalf("expected remaining provider to be phone, got %s", identities[0].Provider)
	}
}
