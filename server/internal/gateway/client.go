package gateway

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Client struct {
	httpClient *http.Client
}

func NewClient() *Client {
	return &Client{
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

func (c *Client) ProxyRequest(gatewayInternalURL, deviceID string, r *http.Request, w http.ResponseWriter) error {
	target := fmt.Sprintf("%s/device/%s/proxy%s", gatewayInternalURL, deviceID, r.URL.Path)
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}

	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return c.proxyWebSocket(target, r, w)
	}

	proxyReq, err := http.NewRequestWithContext(r.Context(), r.Method, target, r.Body)
	if err != nil {
		return err
	}
	proxyReq.Header = r.Header.Clone()

	resp, err := c.httpClient.Do(proxyReq)
	if err != nil {
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"error":"gateway unreachable, please retry"}`))
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusServiceUnavailable {
		body, _ := io.ReadAll(resp.Body)
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write(body)
		return nil
	}

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

func (c *Client) proxyWebSocket(target string, r *http.Request, w http.ResponseWriter) error {
	u, err := url.Parse(target)
	if err != nil {
		return fmt.Errorf("invalid target url: %w", err)
	}

	host := u.Host
	if u.Port() == "" {
		if u.Scheme == "https" {
			host += ":443"
		} else {
			host += ":80"
		}
	}

	conn, err := net.DialTimeout("tcp", host, 10*time.Second)
	if err != nil {
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"error":"gateway unreachable, please retry"}`))
		return nil
	}
	defer conn.Close()

	var reqBuf bytes.Buffer
	fmt.Fprintf(&reqBuf, "%s %s HTTP/1.1\r\n", r.Method, u.RequestURI())
	fmt.Fprintf(&reqBuf, "Host: %s\r\n", u.Host)
	for k, vs := range r.Header {
		for _, v := range vs {
			fmt.Fprintf(&reqBuf, "%s: %s\r\n", k, v)
		}
	}
	fmt.Fprintf(&reqBuf, "\r\n")

	if _, err := conn.Write(reqBuf.Bytes()); err != nil {
		return fmt.Errorf("failed to write ws upgrade: %w", err)
	}

	bufReader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(bufReader, r)
	if err != nil {
		return fmt.Errorf("failed to read ws upgrade response: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusSwitchingProtocols {
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
		return nil
	}

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		return fmt.Errorf("hijack not supported")
	}
	clientConn, bufrw, err := hijacker.Hijack()
	if err != nil {
		return fmt.Errorf("hijack failed: %w", err)
	}
	defer clientConn.Close()

	fmt.Fprintf(bufrw, "HTTP/1.1 101 Switching Protocols\r\n")
	for k, vs := range resp.Header {
		for _, v := range vs {
			fmt.Fprintf(bufrw, "%s: %s\r\n", k, v)
		}
	}
	fmt.Fprintf(bufrw, "\r\n")
	bufrw.Flush()

	done := make(chan struct{})
	go func() {
		io.Copy(conn, bufrw)
		conn.Close()
		close(done)
	}()
	io.Copy(bufrw, bufReader)
	bufrw.Flush()
	clientConn.Close()
	<-done
	return nil
}
