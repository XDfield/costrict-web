package project

import (
	"reflect"
	"strings"
	"testing"

	"github.com/costrict/costrict-web/server/internal/models"
)

func TestTextEqualsBuildsPostgresSafeComparison(t *testing.T) {
	got := textEquals("pm.user_id")
	want := "pm.user_id = CAST(? AS TEXT)"
	if got != want {
		t.Fatalf("textEquals() = %q, want %q", got, want)
	}
}

func TestProjectModelUserColumnsExplicitlyUseText(t *testing.T) {
	tests := []struct {
		name      string
		field      reflect.StructField
		wantSnippet string
	}{
		{
			name:        "Project.CreatorID",
			field:       mustField(t, reflect.TypeOf(models.Project{}), "CreatorID"),
			wantSnippet: "type:text",
		},
		{
			name:        "ProjectMember.UserID",
			field:       mustField(t, reflect.TypeOf(models.ProjectMember{}), "UserID"),
			wantSnippet: "type:text",
		},
		{
			name:        "ProjectMember.ProjectID",
			field:       mustField(t, reflect.TypeOf(models.ProjectMember{}), "ProjectID"),
			wantSnippet: "type:uuid",
		},
		{
			name:        "ProjectInvitation.InviterID",
			field:       mustField(t, reflect.TypeOf(models.ProjectInvitation{}), "InviterID"),
			wantSnippet: "type:text",
		},
		{
			name:        "ProjectInvitation.ProjectID",
			field:       mustField(t, reflect.TypeOf(models.ProjectInvitation{}), "ProjectID"),
			wantSnippet: "type:uuid",
		},
		{
			name:        "ProjectInvitation.InviteeID",
			field:       mustField(t, reflect.TypeOf(models.ProjectInvitation{}), "InviteeID"),
			wantSnippet: "type:text",
		},
		{
			name:        "ProjectRepository.ProjectID",
			field:       mustField(t, reflect.TypeOf(models.ProjectRepository{}), "ProjectID"),
			wantSnippet: "type:uuid",
		},
		{
			name:        "ProjectRepository.BoundByUserID",
			field:       mustField(t, reflect.TypeOf(models.ProjectRepository{}), "BoundByUserID"),
			wantSnippet: "type:text",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gormTag := tt.field.Tag.Get("gorm")
			if gormTag == "" {
				t.Fatalf("gorm tag is empty")
			}
			if !strings.Contains(gormTag, tt.wantSnippet) {
				t.Fatalf("gorm tag %q does not contain %q", gormTag, tt.wantSnippet)
			}
		})
	}
}

func mustField(t *testing.T, typ reflect.Type, name string) reflect.StructField {
	t.Helper()
	field, ok := typ.FieldByName(name)
	if !ok {
		t.Fatalf("field %s not found in %s", name, typ.Name())
	}
	return field
}
