// provider_mapping.go — Phase C3.3 typed-edit surface for the
// provider_mapping subsection of tenant_configs.config_yaml.
//
// Schema (per docs/identity-tenant/MULTI_TENANCY_DESIGN.md §9.3):
//
//	providers:
//	  <name>:
//	    enabled: true|false           # default true when omitted
//	    rank: 200                     # int; tiebreak ordering
//	    field_map:                    # IdP claim → system attribute
//	      employee_number: "emp_id"
//	    enterprise_sync:
//	      interval: "6h"              # Go duration string
//
// C3.2 ships raw YAML blob CRUD. C3.3 layers typed endpoints that parse
// this subsection into Go structs, validate, and re-serialize — without
// disturbing sibling sections (employment_providers, features, etc.) in
// the larger config_yaml blob.
//
// Provider names are not validated against a fixed registry: design §9.3
// says providers are dynamic per tenant Casdoor configuration. The name
// pattern [a-z0-9_]+ rejects obviously malformed keys without coupling
// to a provider list that would lag behind Casdoor's actual config.

package tenantconfig

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ProviderMapping is the typed view of the provider_mapping subsection.
// JSON shape matches what the API returns / accepts.
type ProviderMapping struct {
	Providers map[string]Provider `yaml:"providers" json:"providers"`
}

// Provider is a single IdP's tenant-scoped config.
//
// Enabled is a pointer so JSON/YAML "absent" can be distinguished from
// "explicit false": the design's merge semantics treat absent as
// "inherit default (true)", while explicit false means "off". On PUT
// (full replace) absent → default true is applied during Validate().
type Provider struct {
	Enabled        *bool             `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	Rank           *int              `yaml:"rank,omitempty" json:"rank,omitempty"`
	FieldMap       map[string]string `yaml:"field_map,omitempty" json:"field_map,omitempty"`
	EnterpriseSync *EnterpriseSync   `yaml:"enterprise_sync,omitempty" json:"enterprise_sync,omitempty"`
}

// EnterpriseSync carries employment-sync cadence. Interval is a string
// ("6h", "30m", "1h30m") parsed by time.ParseDuration.
type EnterpriseSync struct {
	Interval string `yaml:"interval,omitempty" json:"interval,omitempty"`
}

// Sentinel errors specific to provider_mapping schema validation.
// Reuses ErrInvalidYAML (cs-user-side 400) for surface-level YAML parse
// failures; these sentinels cover the typed-schema layer.
var (
	ErrProviderNameInvalid = errors.New("provider_mapping: invalid provider name")
	ErrIntervalInvalid     = errors.New("provider_mapping: invalid enterprise_sync.interval")
	ErrRankNegative        = errors.New("provider_mapping: rank must be non-negative")
)

// providerNamePattern matches the canonical provider name shape.
// Lowercase alphanumerics + underscore; 1-64 chars. Underscore (not
// hyphen) matches existing provider identifiers in the codebase
// (wxwork, azure_ad, dingtalk, feishu, etc.).
var providerNamePattern = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)

// maxInterval duration cap; rejects "1ns" DoS payloads and obvious
// typos like "9999h". 30 days is the longest sane sync cadence.
const maxInterval = 30 * 24 * time.Hour

// Validate enforces the typed schema. Returns nil on success.
// Side effect: applies defaults (Enabled → true when nil) in place so
// the subsequent serialize is canonical.
func (m *ProviderMapping) Validate() error {
	if m == nil {
		return nil // empty mapping is valid
	}
	for name, p := range m.Providers {
		if !providerNamePattern.MatchString(name) {
			return fmt.Errorf("%w: %q must match %s", ErrProviderNameInvalid, name, providerNamePattern.String())
		}
		if p.Rank != nil && *p.Rank < 0 {
			return fmt.Errorf("%w: provider %q rank=%d", ErrRankNegative, name, *p.Rank)
		}
		if p.EnterpriseSync != nil {
			if err := validateInterval(p.EnterpriseSync.Interval); err != nil {
				return fmt.Errorf("%w: provider %q: %v", ErrIntervalInvalid, name, err)
			}
		}
		// Default Enabled → true when nil. Done in place so the
		// serialized YAML carries the canonical value.
		if p.Enabled == nil {
			t := true
			m.Providers[name] = Provider{
				Enabled:        &t,
				Rank:           p.Rank,
				FieldMap:       p.FieldMap,
				EnterpriseSync: p.EnterpriseSync,
			}
		}
	}
	return nil
}

// validateInterval parses a Go duration string and bounds it.
// Empty interval is allowed (means "use default / no sync").
func validateInterval(s string) error {
	if s == "" {
		return nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	if d <= 0 {
		return fmt.Errorf("interval must be positive, got %s", d)
	}
	if d > maxInterval {
		return fmt.Errorf("interval %s exceeds %s cap", d, maxInterval)
	}
	return nil
}

// ParseProviderMapping extracts the provider_mapping subsection from a
// raw YAML blob. Returns a synthetic empty mapping when the blob has
// no provider_mapping key — every tenant implicitly has an empty one.
//
// yaml.v3 silently ignores unknown sibling keys, so this only fails on
// actual YAML parse errors (returned as ErrInvalidYAML).
func ParseProviderMapping(rawYAML string) (*ProviderMapping, error) {
	if strings.TrimSpace(rawYAML) == "" {
		return &ProviderMapping{Providers: map[string]Provider{}}, nil
	}

	var doc struct {
		ProviderMapping *ProviderMapping `yaml:"provider_mapping"`
	}
	if err := yaml.Unmarshal([]byte(rawYAML), &doc); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidYAML, err)
	}
	if doc.ProviderMapping == nil {
		return &ProviderMapping{Providers: map[string]Provider{}}, nil
	}
	if doc.ProviderMapping.Providers == nil {
		doc.ProviderMapping.Providers = map[string]Provider{}
	}
	return doc.ProviderMapping, nil
}

// SerializeProviderMapping renders the typed view back to YAML under the
// `provider_mapping:` top-level key. Returns the section text only —
// caller (service.UpdateProviderMapping) merges this into the larger
// config_yaml blob.
func SerializeProviderMapping(m *ProviderMapping) (string, error) {
	if m == nil {
		m = &ProviderMapping{Providers: map[string]Provider{}}
	}
	if m.Providers == nil {
		m.Providers = map[string]Provider{}
	}
	out := struct {
		ProviderMapping ProviderMapping `yaml:"provider_mapping"`
	}{ProviderMapping: *m}
	buf, err := yaml.Marshal(out)
	if err != nil {
		return "", fmt.Errorf("tenantconfig: marshal provider_mapping: %w", err)
	}
	return string(buf), nil
}
