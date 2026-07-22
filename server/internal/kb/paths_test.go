package kb

import (
	"errors"
	"testing"
)

const teamID = "7f3c9a1e-d4b2-4c8e-9a3f-1b2c3d4e5f6a"

func TestKBRepoPath_BasicCases(t *testing.T) {
	cases := []struct {
		name string
		url  string
		want string
	}{
		{"T01_github_with_git", "https://github.com/ownerA/proj.git", "t-7f3c9a1e/kb-github.com__ownera__proj"},
		{"T02_github_no_git", "https://github.com/ownerA/proj", "t-7f3c9a1e/kb-github.com__ownera__proj"},
		{"T03_trailing_slash", "https://github.com/ownerA/proj/", "t-7f3c9a1e/kb-github.com__ownera__proj"},
		{"T04_gitlab_uppercase", "https://GITLAB.COM/Group.Foo/bar-baz.git", "t-7f3c9a1e/kb-gitlab.com__group.foo__bar-baz"},
		{"T05_gitea_internal", "https://gitea.costrict.local/team-x/internal-svc", "t-7f3c9a1e/kb-gitea.costrict.local__team-x__internal-svc"},
		{"T06_with_port_3segs", "https://gitlab.example.com:8443/group/sub/proj.git", "t-7f3c9a1e/kb-gitlab.example.com:8443__group__sub__proj"},
		{"T07_http_scheme", "http://gitea.intranet/myteam/Svc", "t-7f3c9a1e/kb-gitea.intranet__myteam__svc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := KBRepoPath(tc.url, teamID)
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestKBRepoPath_EscapeCases(t *testing.T) {
	cases := []struct {
		name string
		url  string
		want string
	}{
		{"T10_dot_underscore", "https://github.com/owner.A/proj_v2", "t-7f3c9a1e/kb-github.com__owner.a__proj_v2"},
		{"T11_plus_to_underscore", "https://github.com/owner+test/proj", "t-7f3c9a1e/kb-github.com__owner_test__proj"},
		{"T12_chinese_chars", "https://github.com/中文仓/repo", "t-7f3c9a1e/kb-github.com_______repo"},
		{"T13_leading_dot_segment", "https://github.com/.hidden/proj", "t-7f3c9a1e/kb-github.com___.hidden__proj"},
		{"T14_at_to_underscore", "https://github.com/team/proj@v1", "t-7f3c9a1e/kb-github.com__team__proj_v1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := KBRepoPath(tc.url, teamID)
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestKBRepoPath_EquivalenceClasses(t *testing.T) {
	t.Run("E01_url_normalization", func(t *testing.T) {
		urls := []string{
			"https://github.com/o/p.git",
			"https://github.com/o/p",
			"https://github.com/o/p/",
			"HTTPS://GitHub.Com/o/p",
		}
		want := "t-7f3c9a1e/kb-github.com__o__p"
		for _, u := range urls {
			got, err := KBRepoPath(u, teamID)
			if err != nil {
				t.Fatalf("url %q: unexpected err: %v", u, err)
			}
			if got != want {
				t.Errorf("url %q: got %q, want %q", u, got, want)
			}
		}
	})

	t.Run("E02_query_fragment_ignored", func(t *testing.T) {
		urls := []string{
			"https://github.com/o/p?branch=main",
			"https://github.com/o/p#readme",
			"https://github.com/o/p.git?x=1",
		}
		want := "t-7f3c9a1e/kb-github.com__o__p"
		for _, u := range urls {
			got, err := KBRepoPath(u, teamID)
			if err != nil {
				t.Fatalf("url %q: unexpected err: %v", u, err)
			}
			if got != want {
				t.Errorf("url %q: got %q, want %q", u, got, want)
			}
		}
	})
}

func TestKBRepoPath_TeamIsolation(t *testing.T) {
	// ET02 vs ET03: same URL, different team → different kb_repo_path
	url := "https://github.com/o/p.git"
	gotA, err := KBRepoPath(url, "7f3c9a1e-d4b2-4c8e-9a3f-1b2c3d4e5f6a")
	if err != nil {
		t.Fatal(err)
	}
	gotB, err := KBRepoPath(url, "9b8c7d6e-1234-5678-9abc-def012345678")
	if err != nil {
		t.Fatal(err)
	}
	if gotA == gotB {
		t.Errorf("expected team isolation; got identical %q", gotA)
	}
	wantA := "t-7f3c9a1e/kb-github.com__o__p"
	wantB := "t-9b8c7d6e/kb-github.com__o__p"
	if gotA != wantA || gotB != wantB {
		t.Errorf("got A=%q B=%q; want A=%q B=%q", gotA, gotB, wantA, wantB)
	}
}

func TestKBRepoPath_InvalidInputs(t *testing.T) {
	cases := []struct {
		name string
		url  string
		tid  string
		err  error
	}{
		{"X01_not_a_url", "not-a-url", teamID, ErrInvalidURL},
		{"X02_ftp_scheme", "ftp://github.com/o/p", teamID, ErrInvalidURL},
		{"X03_file_scheme", "file:///path/to/repo", teamID, ErrInvalidURL},
		{"X04_bare_host_trailing_slash", "https://github.com/", teamID, ErrInvalidURL},
		{"X05_bare_host", "https://github.com", teamID, ErrInvalidURL},
		{"X06_empty_url", "", teamID, ErrInvalidURL},
		{"X08_bad_team_id", "https://github.com/o/p", "not-a-uuid", ErrInvalidTeamID},
		{"X09_empty_team_id", "https://github.com/o/p", "", ErrInvalidTeamID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := KBRepoPath(tc.url, tc.tid)
			if !errors.Is(err, tc.err) {
				t.Errorf("got err=%v, want %v", err, tc.err)
			}
		})
	}
}

func TestKBRepoPath_BoundaryCases(t *testing.T) {
	cases := []struct {
		name string
		url  string
		want string
	}{
		{"B01_dotgit_segment", "https://github.com/o/p/.git", "t-7f3c9a1e/kb-github.com__o__p"},
		{"B02_double_slash", "https://github.com/o//p", "t-7f3c9a1e/kb-github.com__o__p"},
		{"B04_costrict_kb_segment", "https://gitea.costrict.local/costrict-kb/foo", "t-7f3c9a1e/kb-gitea.costrict.local__costrict-kb__foo"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := KBRepoPath(tc.url, teamID)
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAlgorithmVersion(t *testing.T) {
	if AlgorithmVersion != "v2" {
		t.Errorf("AlgorithmVersion = %q, want v2 (bump only on format change)", AlgorithmVersion)
	}
}
