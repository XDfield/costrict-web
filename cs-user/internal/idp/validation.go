// idp/validation.go — IdP source configuration validation.
//
// E2.4: Validates provider-specific configuration schemas.
// Different IdP types have different required fields:
//   - OAuth/OIDC: client_id, client_secret, authorization_url, token_url, userinfo_url
//   - LDAP: host, port, bind_dn, base_dn, user_filter
//   - Password: no config required (built-in)

package idp

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// Validator validates IdP source configurations.
type Validator struct {
	// AllowInsecure allows HTTP URLs (not HTTPS) for OAuth endpoints.
	// Should be false in production.
	AllowInsecure bool
}

// NewValidator creates a new Validator with safe defaults.
func NewValidator() *Validator {
	return &Validator{
		AllowInsecure: false,
	}
}

// ValidateConfig validates a provider's configuration based on its type.
// Returns nil if valid, error with details if invalid.
func (v *Validator) ValidateConfig(provider string, config map[string]interface{}) error {
	if config == nil {
		return fmt.Errorf("config cannot be nil")
	}

	// Determine provider type and validate accordingly
	switch provider {
	case "github", "google", "azure_ad", "feishu", "dingtalk", "wecom":
		return v.validateOAuthConfig(provider, config)
	case "ldap":
		return v.validateLDAPConfig(config)
	case "password":
		return v.validatePasswordConfig(config)
	case "idtrust":
		// idtrust may use OAuth or custom protocol; be permissive for now
		return v.validateIdtrustConfig(config)
	default:
		// Unknown providers — basic validation only
		return v.validateGenericConfig(config)
	}
}

// validateOAuthConfig validates OAuth/OIDC provider configuration.
func (v *Validator) validateOAuthConfig(provider string, config map[string]interface{}) error {
	// Required fields for OAuth/OIDC
	requiredFields := []string{
		"client_id",
		"client_secret",
		"authorization_url",
		"token_url",
	}

	// userinfo_url is optional for some providers (e.g., RFC 7643 token may contain user info)
	// but required for others
	if provider == "google" || provider == "azure_ad" || provider == "github" {
		requiredFields = append(requiredFields, "userinfo_url")
	}

	for _, field := range requiredFields {
		if _, ok := config[field]; !ok {
			return fmt.Errorf("missing required field: %s", field)
		}
		if isEmpty(config[field]) {
			return fmt.Errorf("field %s cannot be empty", field)
		}
	}

	// Validate URL fields
	urlFields := []string{"authorization_url", "token_url"}
	if _, ok := config["userinfo_url"]; ok {
		urlFields = append(urlFields, "userinfo_url")
	}

	for _, field := range urlFields {
		urlStr, ok := config[field].(string)
		if !ok {
			return fmt.Errorf("field %s must be a string", field)
		}
		if err := v.validateURL(urlStr); err != nil {
			return fmt.Errorf("invalid %s: %w", field, err)
		}
	}

	// Validate scopes if provided
	if scopes, ok := config["scopes"]; ok {
		if err := v.validateScopes(scopes); err != nil {
			return fmt.Errorf("invalid scopes: %w", err)
		}
	}

	// Note: scopes field is optional for OAuth providers
	// Some providers (e.g., RFC 7643 compliant) may not need explicit scopes

	return nil
}

// validateLDAPConfig validates LDAP provider configuration.
func (v *Validator) validateLDAPConfig(config map[string]interface{}) error {
	requiredFields := []string{
		"host",
		"base_dn",
		"user_filter",
	}

	for _, field := range requiredFields {
		if _, ok := config[field]; !ok {
			return fmt.Errorf("missing required field: %s", field)
		}
		if isEmpty(config[field]) {
			return fmt.Errorf("field %s cannot be empty", field)
		}
	}

	// Validate host format
	host, ok := config["host"].(string)
	if !ok {
		return fmt.Errorf("host must be a string")
	}
	// Allow host:port format or plain hostname
	if strings.Contains(host, ":") {
		parts := strings.Split(host, ":")
		if len(parts) != 2 {
			return fmt.Errorf("invalid host:port format")
		}
		// Validate port is numeric
		port := parts[1]
		if !regexp.MustCompile(`^\d+$`).MatchString(port) {
			return fmt.Errorf("port must be numeric")
		}
	}

	// Validate port if provided separately
	if port, ok := config["port"]; ok {
		portNum, ok := port.(int)
		if !ok {
			return fmt.Errorf("port must be an integer")
		}
		if portNum <= 0 || portNum > 65535 {
			return fmt.Errorf("port must be between 1 and 65535")
		}
	}

	// Validate optional bind_dn if provided
	if bindDN, ok := config["bind_dn"]; ok && !isEmpty(bindDN) {
		bindDNStr, ok := bindDN.(string)
		if !ok {
			return fmt.Errorf("bind_dn must be a string")
		}
		if !strings.Contains(bindDNStr, "=") {
			return fmt.Errorf("bind_dn must be in DN format (e.g., cn=admin,dc=example,dc=com)")
		}
	}

	return nil
}

// validatePasswordConfig validates password provider (built-in).
func (v *Validator) validatePasswordConfig(config map[string]interface{}) error {
	// Password provider doesn't require any config
	// Config may be empty or contain tenant-specific password policies
	return nil
}

// validateIdtrustConfig validates idtrust provider configuration.
// idtrust is a custom enterprise IdP; be permissive for now.
func (v *Validator) validateIdtrustConfig(config map[string]interface{}) error {
	// Require at least one configuration field
	if len(config) == 0 {
		return fmt.Errorf("idtrust config cannot be empty")
	}

	// If OAuth fields are present, validate them
	if _, hasOAuth := config["authorization_url"]; hasOAuth {
		return v.validateOAuthConfig("idtrust", config)
	}

	// Otherwise, accept any config (idtrust may use custom protocol)
	return nil
}

// validateGenericConfig performs basic validation for unknown provider types.
func (v *Validator) validateGenericConfig(config map[string]interface{}) error {
	if len(config) == 0 {
		return nil // Empty config is acceptable for unknown providers
	}

	// Check for obviously invalid values (nil for required-looking fields)
	for key, val := range config {
		if val == nil && strings.Contains(strings.ToLower(key), "url") {
			return fmt.Errorf("field %s cannot be nil", key)
		}
	}

	return nil
}

// validateURL validates a URL string.
func (v *Validator) validateURL(urlStr string) error {
	parsed, err := url.Parse(urlStr)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}

	if parsed.Scheme == "" {
		return fmt.Errorf("URL must have a scheme (https or http)")
	}

	// Enforce HTTPS in production unless explicitly allowed
	if !v.AllowInsecure && parsed.Scheme != "https" {
		return fmt.Errorf("URL must use HTTPS scheme")
	}

	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("URL scheme must be http or https")
	}

	if parsed.Host == "" {
		return fmt.Errorf("URL must have a host")
	}

	return nil
}

// validateScopes validates scopes field.
func (v *Validator) validateScopes(scopes interface{}) error {
	switch v := scopes.(type) {
	case string:
		if v == "" {
			return fmt.Errorf("scopes cannot be empty string")
		}
		return nil
	case []interface{}:
		if len(v) == 0 {
			return fmt.Errorf("scopes array cannot be empty")
		}
		for _, s := range v {
			if ss, ok := s.(string); ok {
				if ss == "" {
					return fmt.Errorf("scope string cannot be empty")
				}
			} else {
				return fmt.Errorf("scope must be string")
			}
		}
		return nil
	case []string:
		if len(v) == 0 {
			return fmt.Errorf("scopes array cannot be empty")
		}
		for _, s := range v {
			if s == "" {
				return fmt.Errorf("scope string cannot be empty")
			}
		}
		return nil
	default:
		return fmt.Errorf("scopes must be string or array")
	}
}

// isEmpty checks if a value is empty (nil, zero, empty string, etc.)
func isEmpty(v interface{}) bool {
	if v == nil {
		return true
	}
	switch val := v.(type) {
	case string:
		return strings.TrimSpace(val) == ""
	case int, int64, int32:
		return val == 0
	case float64:
		return val == 0.0
	case bool:
		return !val
	case []interface{}:
		return len(val) == 0
	case map[string]interface{}:
		return len(val) == 0
	default:
		return false
	}
}
