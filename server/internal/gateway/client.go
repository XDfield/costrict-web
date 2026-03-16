package gateway

import (
	"fmt"
	"io"
	"net/http"
	"strings"
)

type Client struct {
	httpClient *http.Client
}

func NewClient() *Client {
	return &Client{
		httpClient: &http.Client{},
	}
}

func (c *Client) ProxyRequest(gatewayInternalURL, deviceID string, r *http.Request, w http.ResponseWriter) error {
	target := fmt.Sprintf("%s/device/%s/proxy%s", gatewayInternalURL, deviceID, r.URL.Path)
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}

	proxyReq, err := http.NewRequestWithContext(r.Context(), r.Method, target, r.Body)
	if err != nil {
		return err
	}
	proxyReq.Header = r.Header.Clone()

	resp, err := c.httpClient.Do(proxyReq)
	if err != nil {
		return fmt.Errorf("gateway unreachable: %w", err)
	}
	defer resp.Body.Close()

	skipHeaders := map[string]bool{
		"Access-Control-Allow-Origin":      true,
		"Access-Control-Allow-Headers":     true,
		"Access-Control-Allow-Methods":     true,
		"Access-Control-Allow-Credentials": true,
		"Access-Control-Expose-Headers":    true,
		"Access-Control-Max-Age":           true,
	}
	for k, vs := range resp.Header {
		if skipHeaders[k] {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		flusher, ok := w.(http.Flusher)
		if !ok {
			io.Copy(w, resp.Body)
			return nil
		}
		buf := make([]byte, 4096)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				w.Write(buf[:n])
				flusher.Flush()
			}
			if err != nil {
				break
			}
		}
		return nil
	}

	io.Copy(w, resp.Body)
	return nil
}
