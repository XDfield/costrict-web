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
			field:       reflect.TypeOf(models.Project{}).Field(3),
			wantSnippet: "type:text",
		},
		{
			name:        "ProjectMember.UserID",
			field:       reflect.TypeOf(models.ProjectMember{}).Field(2),
			wantSnippet: "type:text",
		},
		{
			name:        "ProjectMember.ProjectID",
			field:       reflect.TypeOf(models.ProjectMember{}).Field(1),
			wantSnippet: "type:uuid",
		},
		{
			name:        "ProjectInvitation.InviterID",
			field:       reflect.TypeOf(models.ProjectInvitation{}).Field(2),
			wantSnippet: "type:text",
		},
		{
			name:        "ProjectInvitation.ProjectID",
			field:       reflect.TypeOf(models.ProjectInvitation{}).Field(1),
			wantSnippet: "type:uuid",
		},
		{
			name:        "ProjectInvitation.InviteeID",
			field:       reflect.TypeOf(models.ProjectInvitation{}).Field(3),
			wantSnippet: "type:text",
		},
		{
			name:        "ProjectRepository.ProjectID",
			field:       reflect.TypeOf(models.ProjectRepository{}).Field(1),
			wantSnippet: "type:uuid",
		},
		{
			name:        "ProjectRepository.BoundByUserID",
			field:       reflect.TypeOf(models.ProjectRepository{}).Field(5),
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
