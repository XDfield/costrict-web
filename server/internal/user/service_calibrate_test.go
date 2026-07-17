package user

import (
	"context"
	"testing"

	"github.com/costrict/costrict-web/server/internal/models"
)

func TestGetOrCreateUserDisplayNameProtectedByBestIdentityRank(t *testing.T) {
	db := setupUserTestDB(t)
	svc := NewUserService(db)

	// Step 1: User first logs in via IDTrust (rank 300)
	idtrustClaims := &JWTClaims{
		ID:                "idtrust-id",
		Sub:               "idtrust-sub",
		UniversalID:       "shared-uuid",
		Name:              "idtrust_user",
		PreferredUsername: "张三",
		Provider:          "idtrust",
		ProviderUserID:    "idtrust-001",
		Email:             "idtrust@example.com",
		Picture:           "https://example.com/idtrust.png",
	}
	user, err := svc.GetOrCreateUser(context.Background(), idtrustClaims)
	if err != nil {
		t.Fatalf("create idtrust user: %v", err)
	}
	if err := svc.BindIdentityToUser(context.Background(), user.SubjectID, idtrustClaims); err != nil {
		t.Fatalf("bind idtrust identity: %v", err)
	}

	// Verify initial DisplayName
	refreshed, _ := svc.GetUserByID(context.Background(), user.SubjectID)
	if refreshed.DisplayName == nil || *refreshed.DisplayName != "张三" {
		t.Fatalf("expected display_name=张三 after idtrust login, got %v", refreshed.DisplayName)
	}

	// Step 2: User also binds phone (rank 100)
	phoneClaims := &JWTClaims{
		ID:                "phone-id",
		Sub:               "phone-sub",
		UniversalID:       "shared-uuid",
		Name:              "phone_15500000001",
		PreferredUsername: "手机用户",
		Provider:          "phone",
		Phone:             "15500000001",
	}
	if err := svc.BindIdentityToUser(context.Background(), user.SubjectID, phoneClaims); err != nil {
		t.Fatalf("bind phone identity: %v", err)
	}

	// Step 3: User logs in again via phone — DisplayName should NOT be overwritten
	phoneLoginUser, err := svc.GetOrCreateUser(context.Background(), phoneClaims)
	if err != nil {
		t.Fatalf("phone login: %v", err)
	}
	if phoneLoginUser.DisplayName == nil || *phoneLoginUser.DisplayName != "张三" {
		t.Fatalf("expected display_name=张三 protected from phone login, got %v", phoneLoginUser.DisplayName)
	}
	if phoneLoginUser.AuthProvider == nil || *phoneLoginUser.AuthProvider != "phone" {
		t.Fatalf("expected auth_provider=phone (updated), got %v", phoneLoginUser.AuthProvider)
	}

	// Step 4: User logs in again via IDTrust — DisplayName SHOULD be allowed to update
	idtrustUpdatedClaims := &JWTClaims{
		ID:                "idtrust-id",
		Sub:               "idtrust-sub",
		UniversalID:       "shared-uuid",
		Name:              "idtrust_user",
		PreferredUsername: "张三丰",
		Provider:          "idtrust",
		ProviderUserID:    "idtrust-001",
	}
	idtrustLoginUser, err := svc.GetOrCreateUser(context.Background(), idtrustUpdatedClaims)
	if err != nil {
		t.Fatalf("idtrust login: %v", err)
	}
	if idtrustLoginUser.DisplayName == nil || *idtrustLoginUser.DisplayName != "张三丰" {
		t.Fatalf("expected display_name=张三丰 after idtrust re-login, got %v", idtrustLoginUser.DisplayName)
	}
}

func TestGetOrCreateUserCalibratesDirtyDisplayNameFromBestIdentity(t *testing.T) {
	db := setupUserTestDB(t)
	svc := NewUserService(db)

	// Step 1: Create user with IDTrust identity (rank 300)
	idtrustClaims := &JWTClaims{
		ID:                "idtrust-id",
		Sub:               "idtrust-sub",
		UniversalID:       "shared-uuid",
		Name:              "idtrust_user",
		PreferredUsername: "正确名称",
		Provider:          "idtrust",
		ProviderUserID:    "idtrust-001",
	}
	user, err := svc.GetOrCreateUser(context.Background(), idtrustClaims)
	if err != nil {
		t.Fatalf("create idtrust user: %v", err)
	}
	if err := svc.BindIdentityToUser(context.Background(), user.SubjectID, idtrustClaims); err != nil {
		t.Fatalf("bind idtrust identity: %v", err)
	}

	// Step 2: Simulate dirty data — directly overwrite DisplayName with a wrong value
	dirtyName := "脏数据名称"
	db.Model(&models.User{}).Where("subject_id = ?", user.SubjectID).Update("display_name", dirtyName)

	// Step 3: Phone login should trigger calibration and fix DisplayName from best identity
	phoneClaims := &JWTClaims{
		ID:                "phone-id",
		Sub:               "phone-sub",
		UniversalID:       "shared-uuid",
		Name:              "phone_15500000001",
		PreferredUsername: "手机用户",
		Provider:          "phone",
		Phone:             "15500000001",
	}
	calibrated, err := svc.GetOrCreateUser(context.Background(), phoneClaims)
	if err != nil {
		t.Fatalf("phone login with calibration: %v", err)
	}
	if calibrated.DisplayName == nil || *calibrated.DisplayName != "正确名称" {
		t.Fatalf("expected display_name calibrated to 正确名称 from idtrust identity, got %v", calibrated.DisplayName)
	}
}

func TestGetOrCreateUserCalibratesDirtyEmailAndAvatarFromBestIdentity(t *testing.T) {
	db := setupUserTestDB(t)
	svc := NewUserService(db)

	// Create user with GitHub identity (rank 200)
	githubClaims := &JWTClaims{
		ID:                "gh-id",
		Sub:               "gh-sub",
		UniversalID:       "shared-uuid",
		Name:              "gh_user",
		PreferredUsername: "GH User",
		Provider:          "github",
		ProviderUserID:    "gh-001",
		Email:             "correct@example.com",
		Picture:           "https://avatars.example.com/correct.png",
	}
	user, err := svc.GetOrCreateUser(context.Background(), githubClaims)
	if err != nil {
		t.Fatalf("create github user: %v", err)
	}
	if err := svc.BindIdentityToUser(context.Background(), user.SubjectID, githubClaims); err != nil {
		t.Fatalf("bind github identity: %v", err)
	}

	// Also bind phone identity
	phoneClaims := &JWTClaims{
		ID:                "phone-id",
		Sub:               "phone-sub",
		UniversalID:       "shared-uuid",
		Name:              "phone_15500000001",
		PreferredUsername: "Phone User",
		Provider:          "phone",
		Phone:             "15500000001",
	}
	if err := svc.BindIdentityToUser(context.Background(), user.SubjectID, phoneClaims); err != nil {
		t.Fatalf("bind phone identity: %v", err)
	}

	// Simulate dirty data in user table
	dirtyEmail := "dirty@example.com"
	dirtyAvatar := "https://example.com/dirty.png"
	db.Model(&models.User{}).Where("subject_id = ?", user.SubjectID).Updates(map[string]interface{}{
		"email":      dirtyEmail,
		"avatar_url": dirtyAvatar,
	})

	// Phone login should trigger calibration
	calibrated, err := svc.GetOrCreateUser(context.Background(), phoneClaims)
	if err != nil {
		t.Fatalf("phone login with calibration: %v", err)
	}
	if calibrated.Email == nil || *calibrated.Email != "correct@example.com" {
		t.Fatalf("expected email calibrated to correct@example.com from github identity, got %v", calibrated.Email)
	}
	if calibrated.AvatarURL == nil || *calibrated.AvatarURL != "https://avatars.example.com/correct.png" {
		t.Fatalf("expected avatar_url calibrated from github identity, got %v", calibrated.AvatarURL)
	}
}

func TestGetOrCreateUserPhoneLoginDoesNotOverwriteIDTrustDisplayName(t *testing.T) {
	db := setupUserTestDB(t)
	svc := NewUserService(db)

	// User logs in via IDTrust first
	idtrustClaims := &JWTClaims{
		ID:                "idtrust-id",
		Sub:               "idtrust-sub",
		UniversalID:       "shared-uuid",
		Name:              "idtrust_user",
		PreferredUsername: "IDTrust名称",
		Provider:          "idtrust",
		ProviderUserID:    "idtrust-001",
	}
	user, err := svc.GetOrCreateUser(context.Background(), idtrustClaims)
	if err != nil {
		t.Fatalf("create idtrust user: %v", err)
	}
	if err := svc.BindIdentityToUser(context.Background(), user.SubjectID, idtrustClaims); err != nil {
		t.Fatalf("bind idtrust identity: %v", err)
	}

	// User then logs in via phone with a different PreferredUsername
	phoneClaims := &JWTClaims{
		ID:                "phone-id",
		Sub:               "phone-sub",
		UniversalID:       "shared-uuid",
		Name:              "phone_15500000001",
		PreferredUsername: "Phone名称",
		Provider:          "phone",
		Phone:             "15500000001",
	}
	phoneUser, err := svc.GetOrCreateUser(context.Background(), phoneClaims)
	if err != nil {
		t.Fatalf("phone login: %v", err)
	}

	// DisplayName should remain as IDTrust value, not phone value
	if phoneUser.DisplayName == nil || *phoneUser.DisplayName != "IDTrust名称" {
		t.Fatalf("expected DisplayName=IDTrust名称 (protected from phone), got %v", phoneUser.DisplayName)
	}

	// Phone field should still be updated since it comes from claims directly
	if phoneUser.Phone == nil || *phoneUser.Phone != "15500000001" {
		t.Fatalf("expected phone field updated, got %v", phoneUser.Phone)
	}
}
