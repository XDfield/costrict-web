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

	if err := db.AutoMigrate(&models.User{}); err != nil {
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
		"name":         "XDfield",
		"displayName":  "gh_XDfield",
		"provider":     "Github",
		"properties": map[string]any{
			"oauth_GitHub_id":          "18633160",
			"oauth_GitHub_username":    "XDfield",
			"oauth_GitHub_displayName": "DoSun",
			"oauth_GitHub_email":       "chenxuan@example.com",
			"oauth_GitHub_avatarUrl":   "https://avatars.githubusercontent.com/u/18633160?v=4",
		},
		"exp": time.Now().Add(time.Hour).Unix(),
	})

	claims, err := ParseJWTClaimsFromAccessToken(tokenString)
	if err != nil {
		t.Fatalf("ParseJWTClaimsFromAccessToken error: %v", err)
	}
	if claims.Name != "XDfield" {
		t.Fatalf("expected github username from properties, got %+v", claims)
	}
	if claims.PreferredUsername != "DoSun" {
		t.Fatalf("expected github display name from properties, got %+v", claims)
	}
	if claims.Email != "chenxuan@example.com" {
		t.Fatalf("expected github email from properties, got %+v", claims)
	}
	if claims.Picture == "" || claims.ProviderUserID != "18633160" || claims.Provider != "Github" {
		t.Fatalf("expected github provider profile fields, got %+v", claims)
	}
}

func TestParseJWTClaimsFromAccessTokenIDTrustUsesProperties(t *testing.T) {
	tokenString := signUserTestJWT(t, jwt.MapClaims{
		"id":           "42766",
		"sub":          "universal-custom-1",
		"universal_id": "universal-custom-1",
		"name":         "random-generated-name",
		"displayName":  "陈烜42766",
		"provider":     "idtrust",
		"properties": map[string]any{
			"oauth_Custom_id":          "42766",
			"oauth_Custom_username":    "陈烜",
			"oauth_Custom_displayName": "陈烜",
			"oauth_Custom_email":       "15986746954",
		},
		"exp": time.Now().Add(time.Hour).Unix(),
	})

	claims, err := ParseJWTClaimsFromAccessToken(tokenString)
	if err != nil {
		t.Fatalf("ParseJWTClaimsFromAccessToken error: %v", err)
	}
	if claims.Name != "陈烜" {
		t.Fatalf("expected idtrust username from properties, got %+v", claims)
	}
	if claims.PreferredUsername != "陈烜" {
		t.Fatalf("expected idtrust display name from properties, got %+v", claims)
	}
	if claims.ProviderUserID != "42766" {
		t.Fatalf("expected idtrust provider user id from properties, got %+v", claims)
	}
	if claims.Email != "" {
		t.Fatalf("expected invalid email-like phone not mapped to email, got %+v", claims)
	}
	if claims.Phone != "15986746954" {
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
