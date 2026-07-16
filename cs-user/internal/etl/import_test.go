//go:build cgo

package etl

import (
	"context"
	"errors"
	"testing"

	"github.com/costrict/costrict-web/cs-user/internal/models"
	"gorm.io/gorm"
)

func TestImportUsers_EmptyTargetInsertsAll(t *testing.T) {
	target := newDB(t)
	batch := []*models.User{
		freshUser("subj-1", "alice"),
		freshUser("subj-2", "bob"),
	}
	var acc Stats
	if err := ImportUsers(context.Background(), target, batch, false, 0, &acc); err != nil {
		t.Fatalf("ImportUsers: %v", err)
	}
	if acc.Inserted != 2 || acc.Updated != 0 || acc.Unchanged != 0 {
		t.Errorf("stats = %+v, want inserted=2", acc)
	}
	n, _ := CountUsers(context.Background(), target)
	if n != 2 {
		t.Errorf("target has %d rows, want 2", n)
	}
}

func TestImportUsers_NoDiffSkipsWrite(t *testing.T) {
	target := newDB(t)
	// Seed target with the exact rows the batch will try to write.
	seedUser(t, target, freshUser("subj-1", "alice"))
	seedUser(t, target, freshUser("subj-2", "bob"))

	batch := []*models.User{
		freshUser("subj-1", "alice"),
		freshUser("subj-2", "bob"),
	}
	var acc Stats
	if err := ImportUsers(context.Background(), target, batch, false, 0, &acc); err != nil {
		t.Fatalf("ImportUsers: %v", err)
	}
	if acc.Unchanged != 2 || acc.Inserted != 0 || acc.Updated != 0 {
		t.Errorf("stats = %+v, want unchanged=2", acc)
	}
}

func TestImportUsers_DiffTriggersUpdate(t *testing.T) {
	target := newDB(t)
	seedUser(t, target, freshUser("subj-1", "alice"))

	// Source has a new username.
	batch := []*models.User{freshUser("subj-1", "alice2")}
	var acc Stats
	if err := ImportUsers(context.Background(), target, batch, false, 0, &acc); err != nil {
		t.Fatalf("ImportUsers: %v", err)
	}
	if acc.Updated != 1 || acc.Inserted != 0 || acc.Unchanged != 0 {
		t.Errorf("stats = %+v, want updated=1", acc)
	}

	// Target's row should now reflect the new username.
	var got models.User
	if err := target.Unscoped().Where("subject_id = ?", "subj-1").First(&got).Error; err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Username != "alice2" {
		t.Errorf("target username = %q, want alice2", got.Username)
	}
}

func TestImportUsers_UpdateClearsPointerField(t *testing.T) {
	target := newDB(t)
	existing := freshUser("subj-1", "alice")
	existing.Email = strPtr("alice@example.com")
	seedUser(t, target, existing)

	// Source has nil email — target's email must be set to NULL.
	src := freshUser("subj-1", "alice")
	src.Email = nil
	var acc Stats
	if err := ImportUsers(context.Background(), target, []*models.User{src}, false, 0, &acc); err != nil {
		t.Fatalf("ImportUsers: %v", err)
	}
	if acc.Updated != 1 {
		t.Fatalf("stats = %+v, want updated=1", acc)
	}

	var got models.User
	if err := target.Unscoped().Where("subject_id = ?", "subj-1").First(&got).Error; err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Email != nil {
		t.Errorf("target email = %v, want nil (cleared by update)", *got.Email)
	}
}

func TestImportUsers_PreservesTargetIDAndCreatedAt(t *testing.T) {
	target := newDB(t)
	seedUser(t, target, freshUser("subj-1", "alice"))

	// Capture target's ID + CreatedAt before update.
	var before models.User
	if err := target.Unscoped().Where("subject_id = ?", "subj-1").First(&before).Error; err != nil {
		t.Fatalf("load before: %v", err)
	}
	targetID := before.ID
	targetCreated := before.CreatedAt

	// Source row claims a different ID + CreatedAt — must be ignored.
	src := freshUser("subj-1", "alice2")
	src.ID = 999999
	src.CreatedAt = targetCreated.Add(-9999 * 24 * 3600 * 1000_000_000)

	var acc Stats
	if err := ImportUsers(context.Background(), target, []*models.User{src}, false, 0, &acc); err != nil {
		t.Fatalf("ImportUsers: %v", err)
	}
	if acc.Updated != 1 {
		t.Fatalf("stats = %+v, want updated=1", acc)
	}

	var after models.User
	if err := target.Unscoped().Where("subject_id = ?", "subj-1").First(&after).Error; err != nil {
		t.Fatalf("load after: %v", err)
	}
	if after.ID != targetID {
		t.Errorf("target ID changed: %d → %d", targetID, after.ID)
	}
	if !after.CreatedAt.Equal(targetCreated) {
		t.Errorf("target CreatedAt changed: %v → %v", targetCreated, after.CreatedAt)
	}
	if after.Username != "alice2" {
		t.Errorf("username not updated: %q", after.Username)
	}
}

func TestImportUsers_PropagatesSoftDelete(t *testing.T) {
	target := newDB(t)
	seedUser(t, target, freshUser("subj-1", "alice"))

	// Source row is soft-deleted — target should be soft-deleted too.
	src := freshUser("subj-1", "alice")
	src.DeletedAt = gorm.DeletedAt{Time: now(), Valid: true}
	var acc Stats
	if err := ImportUsers(context.Background(), target, []*models.User{src}, false, 0, &acc); err != nil {
		t.Fatalf("ImportUsers: %v", err)
	}
	if acc.Updated != 1 {
		t.Fatalf("stats = %+v, want updated=1", acc)
	}

	var got models.User
	if err := target.Unscoped().Where("subject_id = ?", "subj-1").First(&got).Error; err != nil {
		t.Fatalf("load: %v", err)
	}
	if !got.DeletedAt.Valid {
		t.Errorf("target DeletedAt.Valid = false, want true")
	}
}

func TestImportUsers_EmptyBatchNoOp(t *testing.T) {
	target := newDB(t)
	var acc Stats
	if err := ImportUsers(context.Background(), target, nil, false, 0, &acc); err != nil {
		t.Fatalf("ImportUsers: %v", err)
	}
	if acc.Inserted+acc.Updated+acc.Unchanged+acc.Failed != 0 {
		t.Errorf("expected zero stats, got %+v", acc)
	}
}

func TestImportUsers_NilDBRejected(t *testing.T) {
	var acc Stats
	err := ImportUsers(context.Background(), nil, []*models.User{freshUser("a", "b")}, false, 0, &acc)
	if !errors.Is(err, ErrNilDB) {
		t.Errorf("expected ErrNilDB, got %v", err)
	}
}

func TestImportUsers_SkipsRowsWithEmptySubjectID(t *testing.T) {
	target := newDB(t)
	batch := []*models.User{
		{SubjectID: "", Username: "no-subject"},
		freshUser("subj-1", "alice"),
	}
	var acc Stats
	if err := ImportUsers(context.Background(), target, batch, false, 0, &acc); err != nil {
		t.Fatalf("ImportUsers: %v", err)
	}
	if acc.Failed != 1 {
		t.Errorf("failed = %d, want 1 (empty subject_id)", acc.Failed)
	}
	if acc.Inserted != 1 {
		t.Errorf("inserted = %d, want 1", acc.Inserted)
	}
}

func TestImportAuthIdentities_EmptyTargetInsertsAll(t *testing.T) {
	target := newDB(t)
	batch := []*models.UserAuthIdentity{
		freshAuthIdentity("k1", "subj-1", "casdoor"),
		freshAuthIdentity("k2", "subj-2", "oauth2"),
	}
	var acc Stats
	if err := ImportAuthIdentities(context.Background(), target, batch, false, 0, &acc); err != nil {
		t.Fatalf("ImportAuthIdentities: %v", err)
	}
	if acc.Inserted != 2 {
		t.Errorf("inserted = %d, want 2", acc.Inserted)
	}
}

func TestImportAuthIdentities_DiffTriggersUpdate(t *testing.T) {
	target := newDB(t)
	seedAuthIdentity(t, target, freshAuthIdentity("k1", "subj-1", "casdoor"))

	src := freshAuthIdentity("k1", "subj-1", "oauth2")
	var acc Stats
	if err := ImportAuthIdentities(context.Background(), target, []*models.UserAuthIdentity{src}, false, 0, &acc); err != nil {
		t.Fatalf("ImportAuthIdentities: %v", err)
	}
	if acc.Updated != 1 {
		t.Fatalf("stats = %+v, want updated=1", acc)
	}

	var got models.UserAuthIdentity
	if err := target.Unscoped().Where("external_key = ?", "k1").First(&got).Error; err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Provider != "oauth2" {
		t.Errorf("provider = %q, want oauth2", got.Provider)
	}
}

func TestImportAuthIdentities_NilDBRejected(t *testing.T) {
	var acc Stats
	err := ImportAuthIdentities(context.Background(), nil, []*models.UserAuthIdentity{freshAuthIdentity("k", "s", "p")}, false, 0, &acc)
	if !errors.Is(err, ErrNilDB) {
		t.Errorf("expected ErrNilDB, got %v", err)
	}
}

func TestValidateSource_FindsDuplicates(t *testing.T) {
	db := newDB(t)
	// Two rows with the same casdoor_universal_id.
	u1 := freshUser("subj-1", "alice")
	u1.CasdoorUniversalID = strPtr("dup-uni-id")
	u2 := freshUser("subj-2", "bob")
	u2.CasdoorUniversalID = strPtr("dup-uni-id")
	u3 := freshUser("subj-3", "carol")
	u3.CasdoorUniversalID = strPtr("unique-uni-id")
	seedUser(t, db, u1)
	seedUser(t, db, u2)
	seedUser(t, db, u3)

	dups, err := ValidateSource(context.Background(), db)
	if err != nil {
		t.Fatalf("ValidateSource: %v", err)
	}
	if len(dups) != 1 {
		t.Fatalf("got %d dups, want 1: %+v", len(dups), dups)
	}
	if dups[0].Value != "dup-uni-id" || dups[0].Count != 2 {
		t.Errorf("dup = %+v, want dup-uni-id count=2", dups[0])
	}
}

func TestValidateSource_NoDuplicatesReturnsEmpty(t *testing.T) {
	db := newDB(t)
	u := freshUser("subj-1", "alice")
	u.CasdoorUniversalID = strPtr("uni-1")
	seedUser(t, db, u)

	dups, err := ValidateSource(context.Background(), db)
	if err != nil {
		t.Fatalf("ValidateSource: %v", err)
	}
	if len(dups) != 0 {
		t.Errorf("expected no dups, got %+v", dups)
	}
}
