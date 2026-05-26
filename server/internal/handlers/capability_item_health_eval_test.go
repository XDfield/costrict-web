package handlers

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/datatypes"
)

// TestGetItem_HealthEvaluationPresent verifies the detail endpoint surfaces
// the health and evaluation jsonb blocks (via buildItemResponse) when the
// stored row carries non-empty values.
func TestGetItem_HealthEvaluationPresent(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-he1", Name: "he-reg", SourceType: "internal", RepoID: "repo-1", OwnerID: "u1",
	})
	health := `{"score":0.82,"freshness_label":"fresh","signals":{"freshness":0.9,"popularity":0.7,"source_trust":0.85}}`
	eval := `{"final_score":4.2,"decision":"accept","model_id":"deepseek-v4"}`
	database.DB.Create(&models.CapabilityItem{
		ID: "item-he1", RegistryID: "reg-he1", RepoID: "repo-1", Slug: "he-skill", ItemType: "skill",
		Name: "HE Skill", Status: "active", CreatedBy: "u1",
		Metadata:   datatypes.JSON([]byte("{}")),
		Health:     datatypes.JSON([]byte(health)),
		Evaluation: datatypes.JSON([]byte(eval)),
	})

	w := get(newItemRouter(""), "/api/items/item-he1")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var item map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&item); err != nil {
		t.Fatalf("decode body: %v", err)
	}

	gotHealth, ok := item["health"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected health object in response, got %v", item["health"])
	}
	if gotHealth["score"] != float64(0.82) || gotHealth["freshness_label"] != "fresh" {
		t.Fatalf("unexpected health payload: %v", gotHealth)
	}

	gotEval, ok := item["evaluation"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected evaluation object in response, got %v", item["evaluation"])
	}
	if gotEval["decision"] != "accept" || gotEval["final_score"] != float64(4.2) {
		t.Fatalf("unexpected evaluation payload: %v", gotEval)
	}
}

// TestGetItem_HealthEvaluationOmittedWhenEmpty verifies that an item with no
// health/evaluation data does not surface populated blocks. The DTO uses the
// json `omitempty` tag, so a nil column is dropped from the payload entirely;
// an empty `{}` column (the schema default) round-trips as an empty object.
// Either way the client never sees populated panels for an unscored item.
func TestGetItem_HealthEvaluationOmittedWhenEmpty(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-he2", Name: "he-reg2", SourceType: "internal", RepoID: "repo-1", OwnerID: "u1",
	})
	// Insert a row whose health/evaluation columns are SQL NULL so that the
	// reloaded datatypes.JSON is empty (len 0) and omitempty drops the field.
	if err := database.DB.Exec(
		`INSERT INTO capability_items (id, registry_id, repo_id, slug, item_type, name, status, created_by, descriptions, metadata, health, evaluation)
		 VALUES ('item-he2','reg-he2','repo-1','he-empty','skill','HE Empty','active','u1','{}','{}', NULL, NULL)`,
	).Error; err != nil {
		t.Fatalf("insert item: %v", err)
	}

	w := get(newItemRouter(""), "/api/items/item-he2")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var item map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&item); err != nil {
		t.Fatalf("decode body: %v", err)
	}

	if v, present := item["health"]; present {
		t.Fatalf("expected health to be omitted for empty column, got %v", v)
	}
	if v, present := item["evaluation"]; present {
		t.Fatalf("expected evaluation to be omitted for empty column, got %v", v)
	}
}
