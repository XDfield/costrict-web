package models

import (
	"encoding/json"
	"testing"
)

// TestJWTClaims_JSONRoundTrip verifies the wire shape matches Casdoor's token
// payload (snake_case) so server can serialize and cs-user can deserialize
// without field renaming. This is the only contract test — semantic validation
// (normalizeJWTClaims etc.) lives in the user package.
func TestJWTClaims_JSONRoundTrip(t *testing.T) {
	t.Parallel()

	in := JWTClaims{
		ID:                "id-123",
		Sub:               "sub-abc",
		UniversalID:       "uuid-xyz",
		Name:              "alice",
		PreferredUsername: "alice",
		Email:             "alice@example.com",
		Picture:           "https://example.com/a.png",
		Owner:             "owner-1",
		Provider:          "github",
		ProviderUserID:    "12345",
		Phone:             "+8613800000000",
	}

	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// Spot-check the wire format uses snake_case + omitempty.
	want := `{"id":"id-123","sub":"sub-abc","universal_id":"uuid-xyz","name":"alice","preferred_username":"alice","email":"alice@example.com","picture":"https://example.com/a.png","owner":"owner-1","provider":"github","provider_user_id":"12345","phone":"+8613800000000"}`
	if string(raw) != want {
		t.Fatalf("wire format mismatch.\n got: %s\nwant: %s", raw, want)
	}

	var out JWTClaims
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out != in {
		t.Fatalf("round-trip lost data: got %+v, want %+v", out, in)
	}
}

// TestJWTClaims_OmitemptyDropsZeros verifies zero-valued fields are dropped
// from the wire so a sparse claim (e.g. phone-only login) doesn't pollute the
// payload with empty strings.
func TestJWTClaims_OmitemptyDropsZeros(t *testing.T) {
	t.Parallel()

	in := JWTClaims{Sub: "sub-1", Provider: "phone", Phone: "+86123"}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"sub":"sub-1","provider":"phone","phone":"+86123"}`
	if string(raw) != want {
		t.Fatalf("omitempty failed.\n got: %s\nwant: %s", raw, want)
	}
}

func TestBindIdentityOptions_JSONRoundTrip(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   BindIdentityOptions
		want string
	}{
		{"default zero", BindIdentityOptions{}, `{}`},
		{"force rebind", BindIdentityOptions{ForceRebind: true}, `{"force_rebind":true}`},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			raw, err := json.Marshal(c.in)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(raw) != c.want {
				t.Fatalf("got %s, want %s", raw, c.want)
			}
		})
	}
}
