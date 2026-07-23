//go:build cgo

// Real-token integration tests for employment_providers field_map. Loads the
// actual Casdoor JWTs shipped in testdata/ (one idtrust login, one github
// login) and exercises ApplyEnterpriseMapping end-to-end with tenant config
// that mirrors what an operator would write in production.
//
// The tokens carry the real Casdoor claim shape — most importantly:
//   - per-provider fields are flat keys with underscore separator inside the
//     `properties` map (e.g. properties.oauth_Custom_id, properties.oauth_GitHub_email),
//     NOT nested sub-maps. This is the convention server's legacy
//     authidentity/normalize.go also relies on.
//   - signupApplication is the same generic Casdoor app id ("application_v94pr3")
//     for BOTH tokens, so detection-by-signupApplication can't differentiate
//     idtrust vs github. The differentiator is the JWT's top-level `provider`
//     field ("IDTrust - Sangfor" for idtrust; absent for github — server
//     detects github from the oauth_GitHub_* properties).

package user

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testdataDir is the repo-relative path to the cs-user testdata directory.
// Tests in this package live in cs-user/internal/user/, so the path is two
// levels up.
const testdataDir = "../../testdata"

// loadTestJWTPayload reads a JWT file from testdata/ and returns the decoded
// payload as a generic map. Mirrors what server's
// ParseJWTClaimsFromAccessToken would surface as JWTClaims.ExternalClaims.
// Signature is NOT verified — these tests cover the post-decode mapping
// logic only (signature verification is server/cs-user's auth layer concern).
func loadTestJWTPayload(t *testing.T, name string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(testdataDir, name))
	if err != nil {
		t.Fatalf("read %s/%s: %v", testdataDir, name, err)
	}
	token := strings.TrimSpace(string(raw))
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("%s/%s is not a valid JWT (expected 3 segments, got %d)", testdataDir, name, len(parts))
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	return payload
}

// TestApplyEnterpriseMapping_RealIDTrustToken exercises the full
// tenant-config → field_map → employment_identities write path using the
// actual idtrust JWT Casdoor produces for a Sangfor internal user. Mirrors
// what an operator would write to migrate off server's hardcoded
// authidentity/normalize.go idtrust branch.
//
// Real claim highlights (from testdata/idtrust_jwt_token.txt):
//   - provider: "IDTrust - Sangfor"   (server normalizes to "idtrust")
//   - signupApplication: "application_v94pr3"
//   - properties.oauth_Custom_id: "42766"
//   - properties.oauth_Custom_username: "陈烜"
//   - properties.oauth_Custom_email: "42766@sangfor.com"
func TestApplyEnterpriseMapping_RealIDTrustToken(t *testing.T) {
	t.Parallel()
	payload := loadTestJWTPayload(t, "idtrust_jwt_token.txt")

	svc := newEmploymentMappingService(t)
	seedTenantConfig(t, svc, "default", `employment_providers:
  enabled: [idtrust]
  field_map:
    idtrust:
      enterprise_uid:   "properties.oauth_Custom_id"
      job_title:        "properties.oauth_Custom_username"
      employment_type:  "signupApplication"
`)

	// Server's NormalizeClaimsMap would normalize "IDTrust - Sangfor" →
	// "idtrust" before calling cs-user, so we pass the normalized form.
	if err := svc.ApplyEnterpriseMapping(t.Context(), EmploymentMappingParams{
		UserSubjectID:  "usr_sangfor_42766",
		Provider:       "idtrust",
		ExternalClaims: payload,
	}); err != nil {
		t.Fatalf("ApplyEnterpriseMapping: %v", err)
	}

	row, err := svc.GetEmploymentIdentity(t.Context(), "usr_sangfor_42766")
	if err != nil {
		t.Fatalf("GetEmploymentIdentity: %v", err)
	}
	if row.Provider != "idtrust" {
		t.Errorf("row.Provider: got %q, want idtrust", row.Provider)
	}
	if row.EnterpriseUID == nil || *row.EnterpriseUID != "42766" {
		t.Errorf("EnterpriseUID: got %v, want 42766", row.EnterpriseUID)
	}
	if row.JobTitle == nil || *row.JobTitle != "陈烜" {
		t.Errorf("JobTitle: got %v, want 陈烜", row.JobTitle)
	}
	// signupApplication value as employment_type — proves top-level key
	// lookup still works alongside flat-keyed property paths.
	if row.EmploymentType == nil || *row.EmploymentType != "application_v94pr3" {
		t.Errorf("EmploymentType: got %v, want application_v94pr3", row.EmploymentType)
	}
}

// TestApplyEnterpriseMapping_RealGithubToken exercises the github equivalent
// of the idtrust test. Github real JWT has flat oauth_GitHub_* property keys
// AND a top-level phone_number (which would route to "phone" in server's
// legacy NormalizeClaimsMap). For this test we pass provider="github"
// directly to isolate the field_map plumbing — server's job is upstream of
// this unit.
//
// Real claim highlights (from testdata/github_jwt_token.txt):
//   - signupApplication: "application_v94pr3"
//   - github: "18633160"
//   - properties.oauth_GitHub_id: "18633160"
//   - properties.oauth_GitHub_email: "chenxuan958864951@qq.com"
//   - properties.oauth_GitHub_displayName: "DoSun"
func TestApplyEnterpriseMapping_RealGithubToken(t *testing.T) {
	t.Parallel()
	payload := loadTestJWTPayload(t, "github_jwt_token.txt")

	svc := newEmploymentMappingService(t)
	seedTenantConfig(t, svc, "default", `employment_providers:
  enabled: [github]
  field_map:
    github:
      enterprise_uid:  "properties.oauth_GitHub_id"
      cost_center:     "properties.oauth_GitHub_email"
      job_title:       "properties.oauth_GitHub_displayName"
`)

	if err := svc.ApplyEnterpriseMapping(t.Context(), EmploymentMappingParams{
		UserSubjectID:  "usr_github_18633160",
		Provider:       "github",
		ExternalClaims: payload,
	}); err != nil {
		t.Fatalf("ApplyEnterpriseMapping: %v", err)
	}

	row, err := svc.GetEmploymentIdentity(t.Context(), "usr_github_18633160")
	if err != nil {
		t.Fatalf("GetEmploymentIdentity: %v", err)
	}
	if row.Provider != "github" {
		t.Errorf("row.Provider: got %q, want github", row.Provider)
	}
	if row.EnterpriseUID == nil || *row.EnterpriseUID != "18633160" {
		t.Errorf("EnterpriseUID: got %v, want 18633160", row.EnterpriseUID)
	}
	if row.CostCenter == nil || *row.CostCenter != "chenxuan958864951@qq.com" {
		t.Errorf("CostCenter: got %v, want chenxuan958864951@qq.com", row.CostCenter)
	}
	if row.JobTitle == nil || *row.JobTitle != "DoSun" {
		t.Errorf("JobTitle: got %v, want DoSun", row.JobTitle)
	}
}

// TestApplyEnterpriseMapping_RealIDTrustToken_DetectionViaCompoundProvider
// verifies that when an unrecognized compound provider name ("IDTrust - Sangfor")
// reaches cs-user with NO matching entry in the enabled list, the operator
// can still resolve it via provider_detection. The matcher uses signupApplication,
// which for both real tokens is "application_v94pr3" — the Sangfor internal
// app id — so we treat any login through that app as an enterprise login and
// let field_map.idtrust do the rest.
//
// In production this would coexist with github also coming through the same
// Casdoor app; server-side `provider` (set from the JWT's `provider` field)
// would take precedence for github via the "explicit provider wins" rule.
func TestApplyEnterpriseMapping_RealIDTrustToken_DetectionViaCompoundProvider(t *testing.T) {
	t.Parallel()
	payload := loadTestJWTPayload(t, "idtrust_jwt_token.txt")

	svc := newEmploymentMappingService(t)
	seedTenantConfig(t, svc, "default", `employment_providers:
  enabled: [idtrust]
  provider_detection:
    - signup_application: "application_v94pr3"
      provider: idtrust
  field_map:
    idtrust:
      enterprise_uid: "properties.oauth_Custom_id"
`)

	// Server passes the raw compound provider ("IDTrust - Sangfor") because
	// it isn't normalized on this hypothetical deployment. Detection should
	// still resolve via signupApplication.
	if err := svc.ApplyEnterpriseMapping(t.Context(), EmploymentMappingParams{
		UserSubjectID:  "usr_sangfor_42766",
		Provider:       "IDTrust - Sangfor", // unknown to enabled list
		ExternalClaims: payload,
	}); err != nil {
		t.Fatalf("ApplyEnterpriseMapping with compound provider: %v", err)
	}

	row, err := svc.GetEmploymentIdentity(t.Context(), "usr_sangfor_42766")
	if err != nil {
		t.Fatalf("GetEmploymentIdentity: %v", err)
	}
	if row.Provider != "idtrust" {
		t.Errorf("detection should resolve to idtrust, got row.Provider=%q", row.Provider)
	}
	if row.EnterpriseUID == nil || *row.EnterpriseUID != "42766" {
		t.Errorf("EnterpriseUID: got %v, want 42766", row.EnterpriseUID)
	}
}

// TestApplyEnterpriseMapping_RealGithubToken_NoMappingForIDTrustFields
// confirms github field_map doesn't accidentally pick up oauth_Custom_*
// (idtrust's namespace) — the underscore-prefix convention keeps providers'
// fields isolated inside properties even when both could be configured on
// the same tenant.
func TestApplyEnterpriseMapping_RealGithubToken_NoMappingForIDTrustFields(t *testing.T) {
	t.Parallel()
	payload := loadTestJWTPayload(t, "github_jwt_token.txt")

	svc := newEmploymentMappingService(t)
	seedTenantConfig(t, svc, "default", `employment_providers:
  enabled: [github]
  field_map:
    github:
      enterprise_uid: "properties.oauth_Custom_id"  # wrong namespace for github
`)

	err := svc.ApplyEnterpriseMapping(t.Context(), EmploymentMappingParams{
		UserSubjectID:  "usr_github_18633160",
		Provider:       "github",
		ExternalClaims: payload,
	})
	if err != nil {
		t.Fatalf("ApplyEnterpriseMapping: %v", err)
	}
	row, _ := svc.GetEmploymentIdentity(t.Context(), "usr_github_18633160")
	if row.EnterpriseUID != nil {
		t.Errorf("github field_map should NOT see oauth_Custom_id, got EnterpriseUID=%v", *row.EnterpriseUID)
	}
}

// guard: if someone removes the testdata files, surface a clear failure
// rather than a silent skip (these tests are the only end-to-end coverage
// against real Casdoor claim shapes).
func TestEnsureRealTokenTestdataExists(t *testing.T) {
	t.Parallel()
	for _, name := range []string{
		"idtrust_jwt_token.txt",
		"github_jwt_token.txt",
	} {
		if _, err := os.Stat(filepath.Join(testdataDir, name)); err != nil {
			t.Errorf("missing %s/%s: %v — real-token integration tests cannot run", testdataDir, name, err)
		}
	}
	// Sanity-check the decoded payloads aren't empty (catches accidental
	// truncation). Deep assertions live in the per-token tests above.
	if p := loadTestJWTPayload(t, "idtrust_jwt_token.txt"); len(p) == 0 {
		t.Error("idtrust_jwt_token.txt decoded to empty payload")
	}
	if p := loadTestJWTPayload(t, "github_jwt_token.txt"); len(p) == 0 {
		t.Error("github_jwt_token.txt decoded to empty payload")
	}
}
