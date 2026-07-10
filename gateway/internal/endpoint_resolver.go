package internal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// EndpointResolver resolves the public endpoint the gateway should register.
// If Nacos is configured, it fetches the configured data ID; otherwise it
// returns the statically configured endpoint.
type EndpointResolver interface {
	Resolve(cfg *Config) (string, error)
}

// NewEndpointResolver returns the default resolver.
func NewEndpointResolver() EndpointResolver {
	return &defaultEndpointResolver{client: http.DefaultClient}
}

// NewEndpointResolverWithClient returns a resolver that uses the provided HTTP client.
func NewEndpointResolverWithClient(client *http.Client) EndpointResolver {
	if client == nil {
		client = http.DefaultClient
	}
	return &defaultEndpointResolver{client: client}
}

type defaultEndpointResolver struct {
	client *http.Client
}

func (r *defaultEndpointResolver) Resolve(cfg *Config) (string, error) {
	if cfg == nil {
		return "", errors.New("config is nil")
	}

	if !nacosEnabled(cfg.Nacos) {
		return cfg.Endpoint, nil
	}

	endpoint, err := resolveFromNacos(r.client, cfg.Nacos)
	if err != nil {
		return "", fmt.Errorf("resolve endpoint from Nacos failed: %w", err)
	}

	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return "", errors.New("Nacos returned empty endpoint")
	}

	if err := validateEndpoint(endpoint); err != nil {
		return "", err
	}

	return endpoint, nil
}

func validateEndpoint(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("Nacos returned invalid endpoint URL %q: %w", endpoint, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("Nacos returned invalid endpoint URL %q: scheme must be http or https", endpoint)
	}
	if u.Host == "" {
		return fmt.Errorf("Nacos returned invalid endpoint URL %q: missing host", endpoint)
	}
	return nil
}

func nacosEnabled(n NacosConfig) bool {
	return n.ServerAddr != "" && n.DataID != ""
}

func resolveFromNacos(client *http.Client, n NacosConfig) (string, error) {
	serverAddr := n.ServerAddr
	if !strings.Contains(serverAddr, "://") {
		serverAddr = "http://" + serverAddr
	}

	base, err := url.Parse(serverAddr)
	if err != nil {
		return "", fmt.Errorf("invalid nacos server addr %q: %w", n.ServerAddr, err)
	}
	if base.Port() == "" {
		base.Host = base.Host + ":8848"
	}

	base.Path = "/nacos/v1/cs/configs"
	q := base.Query()
	q.Set("dataId", n.DataID)
	q.Set("group", n.Group)
	if n.NamespaceID != "" {
		q.Set("tenant", n.NamespaceID)
	}
	if n.AccessToken != "" {
		q.Set("accessToken", n.AccessToken)
	}
	if n.Username != "" {
		q.Set("username", n.Username)
		q.Set("password", n.Password)
	}
	base.RawQuery = q.Encode()

	timeout := n.TimeoutMs
	if timeout == 0 {
		timeout = 5000
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Millisecond)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base.String(), nil)
	if err != nil {
		return "", fmt.Errorf("build nacos request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("request nacos config: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return "", fmt.Errorf("read nacos error response: %w", err)
		}
		return "", fmt.Errorf("nacos returned status %d: %s", resp.StatusCode, string(body))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read nacos response: %w", err)
	}

	return string(body), nil
}
