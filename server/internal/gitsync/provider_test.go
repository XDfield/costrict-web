package gitsync

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestStubProvider_KnownTeamReturnsMembers(t *testing.T) {
	p := NewStubProviderFromMap(map[string][]TeamMember{
		"team-a": {
			{SubjectID: "u1", GiteaUsername: "alice"},
			{SubjectID: "u2", GiteaUsername: "bob"},
		},
	})

	members, err := p.ListTeamMembers(context.Background(), "team-a")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("expected 2 members, got %d", len(members))
	}
	if members[0].GiteaUsername != "alice" || members[1].GiteaUsername != "bob" {
		t.Errorf("unexpected members: %+v", members)
	}
}

func TestStubProvider_UnknownTeamReturnsErrTeamNotFound(t *testing.T) {
	p := NewStubProviderFromMap(map[string][]TeamMember{
		"team-a": {{SubjectID: "u1", GiteaUsername: "alice"}},
	})

	_, err := p.ListTeamMembers(context.Background(), "team-b")
	if !errors.Is(err, ErrTeamNotFound) {
		t.Errorf("expected ErrTeamNotFound, got %v", err)
	}
}

func TestStubProvider_EmptyTeamReturnsEmptyList(t *testing.T) {
	// Known team with zero members — must NOT be treated as "not found".
	// During sync this triggers a full purge of Gitea-side members.
	p := NewStubProviderFromMap(map[string][]TeamMember{
		"empty-team": {},
	})

	members, err := p.ListTeamMembers(context.Background(), "empty-team")
	if err != nil {
		t.Fatalf("unexpected error for known empty team: %v", err)
	}
	if len(members) != 0 {
		t.Errorf("expected empty slice, got %d members", len(members))
	}
}

func TestStubProvider_NilProviderReturnsErrTeamNotFound(t *testing.T) {
	var p *StubTeamProvider
	_, err := p.ListTeamMembers(context.Background(), "any")
	if !errors.Is(err, ErrTeamNotFound) {
		t.Errorf("expected ErrTeamNotFound on nil provider, got %v", err)
	}
}

func TestStubProvider_NewStubProviderEmptyByDefault(t *testing.T) {
	p := NewStubProvider()
	_, err := p.ListTeamMembers(context.Background(), "any")
	if !errors.Is(err, ErrTeamNotFound) {
		t.Errorf("expected ErrTeamNotFound on empty provider, got %v", err)
	}
}

func TestStubProvider_WithTeamBuilder(t *testing.T) {
	p := NewStubProvider().
		WithTeam("team-a", []TeamMember{{SubjectID: "u1", GiteaUsername: "alice"}}).
		WithTeam("team-b", []TeamMember{{SubjectID: "u2", GiteaUsername: "bob"}})

	a, _ := p.ListTeamMembers(context.Background(), "team-a")
	b, _ := p.ListTeamMembers(context.Background(), "team-b")
	if len(a) != 1 || a[0].GiteaUsername != "alice" {
		t.Errorf("team-a wrong: %+v", a)
	}
	if len(b) != 1 || b[0].GiteaUsername != "bob" {
		t.Errorf("team-b wrong: %+v", b)
	}
}

func TestStubProvider_ReturnsDefensiveCopy(t *testing.T) {
	original := []TeamMember{{SubjectID: "u1", GiteaUsername: "alice"}}
	p := NewStubProviderFromMap(map[string][]TeamMember{"team-a": original})

	got1, _ := p.ListTeamMembers(context.Background(), "team-a")
	got1[0].GiteaUsername = "MUTATED"

	got2, _ := p.ListTeamMembers(context.Background(), "team-a")
	if !reflect.DeepEqual(got2, original) {
		t.Errorf("provider state was mutated by caller: got %+v, want %+v", got2, original)
	}
}
