package user

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/golang-jwt/jwt/v4"
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

	got, err := svc.GetUserByID("u1")
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

	got, err := svc.GetUsersByIDs([]string{"u1", "u2", "u3"})
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

	got, err := svc.SearchUsers("alice", 20)
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

	user, err := svc.GetOrCreateUser(claims)
	if err != nil {
		t.Fatalf("GetOrCreateUser create error: %v", err)
	}
	if user.SubjectID == "" || user.SubjectID == "uuid-u1" || user.SubjectID == "org/alice" || user.SubjectID == "u1" || user.Username != "alice" {
		t.Fatalf("unexpected created user: %+v", user)
	}
	if len(user.SubjectID) < 5 || user.SubjectID[:4] != "usr_" {
		t.Fatalf("expected local subject_id with usr_ prefix, got %+v", user)
	}
	if user.CasdoorID == nil || *user.CasdoorID != "u1" {
		t.Fatalf("casdoor_id not set: %+v", user)
	}
	if user.CasdoorUniversalID == nil || *user.CasdoorUniversalID != "uuid-u1" {
		t.Fatalf("casdoor_universal_id not set: %+v", user)
	}
	if user.CasdoorSub == nil || *user.CasdoorSub != "org/alice" {
		t.Fatalf("casdoor_sub not set: %+v", user)
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
	seed := models.User{
		SubjectID:   "legacy-u1",
		Username:    "alice",
		DisplayName: &oldName,
		Email:       &oldEmail,
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

	user, err := svc.GetOrCreateUser(claims)
	if err != nil {
		t.Fatalf("GetOrCreateUser update error: %v", err)
	}
	if user.SubjectID != "legacy-u1" {
		t.Fatalf("existing local subject_id should remain unchanged: %+v", user)
	}
	if user.DisplayName == nil || *user.DisplayName != "Alice New" {
		t.Fatalf("display name not updated: %+v", user)
	}
	if user.Email == nil || *user.Email != "new@example.com" {
		t.Fatalf("email not updated: %+v", user)
	}
	if !user.IsActive {
		t.Fatal("expected user to be active")
	}
	if user.CasdoorID == nil || *user.CasdoorID != "u1" {
		t.Fatalf("casdoor_id not backfilled: %+v", user)
	}
	if user.CasdoorUniversalID == nil || *user.CasdoorUniversalID != "uuid-u1" {
		t.Fatalf("casdoor_universal_id not backfilled: %+v", user)
	}
	if user.CasdoorSub == nil || *user.CasdoorSub != "org/alice" {
		t.Fatalf("casdoor_sub not backfilled: %+v", user)
	}
	if user.Organization == nil || *user.Organization != "org" {
		t.Fatalf("organization not backfilled: %+v", user)
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

	user, err := svc.GetOrCreateUser(claims)
	if err != nil {
		t.Fatalf("GetOrCreateUser error: %v", err)
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

	first, err := svc.GetOrCreateUser(claims)
	if err != nil {
		t.Fatalf("first GetOrCreateUser error: %v", err)
	}
	second, err := svc.GetOrCreateUser(claims)
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
	svc := NewCachedUserService(db)

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

func TestParseJWTClaimsFromAccessToken(t *testing.T) {
	tokenString := signUserTestJWT(t, jwt.MapClaims{
		"id":                 "casdoor-id-1",
		"sub":                "org/alice",
		"universal_id":       "universal-1",
		"name":               "alice",
		"preferred_username": "Alice",
		"email":              "alice@example.com",
		"picture":            "https://example.com/avatar.png",
		"owner":              "org",
		"exp":                time.Now().Add(time.Hour).Unix(),
	})

	claims, err := ParseJWTClaimsFromAccessToken(tokenString)
	if err != nil {
		t.Fatalf("ParseJWTClaimsFromAccessToken error: %v", err)
	}
	if claims.ID != "casdoor-id-1" || claims.Sub != "org/alice" || claims.UniversalID != "universal-1" {
		t.Fatalf("unexpected identifiers: %+v", claims)
	}
	if claims.PreferredUsername != "Alice" || claims.Email != "alice@example.com" || claims.Owner != "org" {
		t.Fatalf("unexpected profile claims: %+v", claims)
	}
}

func TestParseJWTClaimsFromAccessTokenGithubProperties(t *testing.T) {
	tokenString := signUserTestJWT(t, jwt.MapClaims{
		"id":           "18633160",
		"sub":          "universal-gh-1",
		"universal_id": "universal-gh-1",
		"name":         "acct_github_user",
		"displayName":  "gh_acct_github_user",
		"provider":     "Github",
		"properties": map[string]any{
			"oauth_GitHub_id":          "18633160",
			"oauth_GitHub_username":    "acct_github_user",
			"oauth_GitHub_displayName": "Display Github User",
			"oauth_GitHub_email":       "user_github@example.com",
			"oauth_GitHub_avatarUrl":   "https://avatars.githubusercontent.com/u/18633160?v=4",
		},
		"exp": time.Now().Add(time.Hour).Unix(),
	})

	claims, err := ParseJWTClaimsFromAccessToken(tokenString)
	if err != nil {
		t.Fatalf("ParseJWTClaimsFromAccessToken error: %v", err)
	}
	if claims.Name != "acct_github_user" {
		t.Fatalf("expected github username from properties, got %+v", claims)
	}
	if claims.PreferredUsername != "Display Github User" {
		t.Fatalf("expected github display name from properties, got %+v", claims)
	}
	if claims.Email != "user_github@example.com" {
		t.Fatalf("expected github email from properties, got %+v", claims)
	}
	if claims.Picture == "" || claims.ProviderUserID != "18633160" || claims.Provider != "Github" {
		t.Fatalf("expected github provider profile fields, got %+v", claims)
	}
}

func TestParseJWTClaimsFromAccessTokenIDTrustUsesProperties(t *testing.T) {
	tokenString := signUserTestJWT(t, jwt.MapClaims{
		"id":           "custom-user-001",
		"sub":          "universal-custom-1",
		"universal_id": "universal-custom-1",
		"name":         "random-generated-name",
		"displayName":  "display_custom_user_001",
		"provider":     "idtrust",
		"properties": map[string]any{
			"oauth_Custom_id":          "custom-user-001",
			"oauth_Custom_username":    "custom_user",
			"oauth_Custom_displayName": "Display Custom User",
			"oauth_Custom_email":       "15500000001",
		},
		"exp": time.Now().Add(time.Hour).Unix(),
	})

	claims, err := ParseJWTClaimsFromAccessToken(tokenString)
	if err != nil {
		t.Fatalf("ParseJWTClaimsFromAccessToken error: %v", err)
	}
	if claims.Name != "custom_user" {
		t.Fatalf("expected idtrust username from properties, got %+v", claims)
	}
	if claims.PreferredUsername != "Display Custom User" {
		t.Fatalf("expected idtrust display name from properties, got %+v", claims)
	}
	if claims.ProviderUserID != "custom-user-001" {
		t.Fatalf("expected idtrust provider user id from properties, got %+v", claims)
	}
	if claims.Email != "" {
		t.Fatalf("expected invalid email-like phone not mapped to email, got %+v", claims)
	}
	if claims.Phone != "15500000001" {
		t.Fatalf("expected phone inferred from custom email field, got %+v", claims)
	}
}

func TestCachedUserServiceGetUsersByIDsAndWarmup(t *testing.T) {
	db := setupUserTestDB(t)
	svc := NewCachedUserService(db)

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

	if err := svc.WarmupCache(context.Background()); err != nil {
		t.Fatalf("WarmupCache error: %v", err)
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
	user, err := svc.GetOrCreateUser(phoneClaims)
	if err != nil {
		t.Fatalf("create phone user: %v", err)
	}

	githubClaims := &JWTClaims{ID: "gh-id", Sub: "gh-sub", UniversalID: "gh-uuid", Name: "acct_github_user", PreferredUsername: "Display Github User", Provider: "github", ProviderUserID: "provider-gh-001", Picture: "https://avatars.example.com/a.png"}
	if err := svc.BindIdentityToUser(user.SubjectID, githubClaims); err != nil {
		t.Fatalf("bind github identity: %v", err)
	}

	identities, err := svc.ListUserIdentities(user.SubjectID)
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
	refreshed, err := svc.GetUserByID(user.SubjectID)
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

	user, err := svc.GetOrCreateUser(&JWTClaims{ID: "gh-id", Sub: "gh-sub", UniversalID: "gh-uuid", Name: "acct_github_user", PreferredUsername: "Display Github User", Provider: "github", ProviderUserID: "provider-gh-001"})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := svc.BindIdentityToUser(user.SubjectID, &JWTClaims{ID: "phone-id", Sub: "phone-sub", UniversalID: "phone-uuid", Name: "phone_15500000001", PreferredUsername: "ph_15500000001", Provider: "phone", Phone: "15500000001"}); err != nil {
		t.Fatalf("bind phone identity: %v", err)
	}
	identities, _ := svc.ListUserIdentities(user.SubjectID)
	var githubIdentityID uint
	for _, identity := range identities {
		if identity.Provider == "github" {
			githubIdentityID = identity.ID
		}
	}
	if githubIdentityID == 0 {
		t.Fatal("expected github identity to exist")
	}
	if err := svc.UnbindIdentity(user.SubjectID, githubIdentityID); err != nil {
		t.Fatalf("unbind github identity: %v", err)
	}
	identities, _ = svc.ListUserIdentities(user.SubjectID)
	if len(identities) != 1 || !identities[0].IsPrimary || identities[0].Provider != "phone" {
		t.Fatalf("expected remaining phone identity to become primary, got %+v", identities)
	}
}

func TestGetOrCreateUserAutoBindSameUniversalIDDifferentProvider(t *testing.T) {
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

	ghUser, err := svc.GetOrCreateUser(githubClaims)
	if err != nil {
		t.Fatalf("create github user: %v", err)
	}
	if ghUser.CasdoorUniversalID == nil || *ghUser.CasdoorUniversalID != "shared-uuid" {
		t.Fatalf("expected universal_id shared-uuid, got %+v", ghUser)
	}

	identities, _ := svc.ListUserIdentities(ghUser.SubjectID)
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

	phoneUser, err := svc.GetOrCreateUser(phoneClaims)
	if err != nil {
		t.Fatalf("get or create phone user: %v", err)
	}
	if phoneUser.SubjectID != ghUser.SubjectID {
		t.Fatalf("expected same subject_id for same universal_id, got github=%s phone=%s", ghUser.SubjectID, phoneUser.SubjectID)
	}

	identities, err = svc.ListUserIdentities(ghUser.SubjectID)
	if err != nil {
		t.Fatalf("list identities: %v", err)
	}
	if len(identities) != 2 {
		t.Fatalf("expected 2 identities (github + phone), got %d", len(identities))
	}

	providerSet := map[string]bool{}
	for _, id := range identities {
		providerSet[id.Provider] = true
	}
	if !providerSet["github"] || !providerSet["phone"] {
		t.Fatalf("expected both github and phone identities, got %+v", identities)
	}

	refreshed, _ := svc.GetUserByID(ghUser.SubjectID)
	if refreshed.Phone == nil || *refreshed.Phone != "15500000001" {
		t.Fatalf("expected phone to be merged into user profile, got %+v", refreshed)
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

	user, err := svc.GetOrCreateUser(claims)
	if err != nil {
		t.Fatalf("GetOrCreateUser error: %v", err)
	}
	if user.SubjectID != "legacy-u1" {
		t.Fatalf("expected match by legacy external_key, got %+v", user)
	}

	identities, _ := svc.ListUserIdentities(user.SubjectID)
	if len(identities) != 1 {
		t.Fatalf("expected 1 identity after legacy fallback, got %d", len(identities))
	}
	if identities[0].ExternalKey != "casdoor:github:shared-uuid" {
		t.Fatalf("expected identity external_key upgraded to provider-aware format, got %s", identities[0].ExternalKey)
	}
}
