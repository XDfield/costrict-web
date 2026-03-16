package internal

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

type flusher interface {
	Flush()
}

func DeviceProxyHandler(manager *TunnelManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		deviceID := c.Param("deviceID")

		session, ok := manager.Get(deviceID)
		if !ok {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "device tunnel not connected"})
			return
		}

		stream, err := session.Open()
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "failed to open tunnel stream"})
			return
		}
		defer stream.Close()

		path := c.Param("path")

		if strings.EqualFold(c.GetHeader("Upgrade"), "websocket") {
			fullPath := path
			if c.Request.URL.RawQuery != "" {
				fullPath += "?" + c.Request.URL.RawQuery
			}
			handleWebSocketProxy(c, stream, fullPath)
			return
		}

		c.Request.URL.Path = path
		requestURI := path
		if c.Request.URL.RawQuery != "" {
			requestURI += "?" + c.Request.URL.RawQuery
		}
		c.Request.RequestURI = requestURI

		if err := c.Request.Write(stream); err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": "failed to write request to tunnel"})
			return
		}

		resp, err := http.ReadResponse(bufio.NewReader(stream), c.Request)
		if err != nil {
			log.Printf("[proxy] ReadResponse error for %s %s: %v", c.Request.Method, path, err)
			c.JSON(http.StatusBadGateway, gin.H{"error": "failed to read response from tunnel"})
			return
		}
		defer resp.Body.Close()

		for k, vs := range resp.Header {
			for _, v := range vs {
				c.Header(k, v)
			}
		}
		c.Status(resp.StatusCode)
		if f, ok := c.Writer.(flusher); ok && resp.Header.Get("Content-Type") == "text/event-stream" {
			buf := make([]byte, 4096)
			for {
				n, err := resp.Body.Read(buf)
				if n > 0 {
					c.Writer.Write(buf[:n])
					f.Flush()
				}
				if err != nil {
					break
				}
			}
		} else {
			io.Copy(c.Writer, resp.Body)
		}
	}
}

func handleWebSocketProxy(c *gin.Context, stream io.ReadWriteCloser, path string) {
	var reqBuf bytes.Buffer
	fmt.Fprintf(&reqBuf, "%s %s HTTP/1.1\r\n", c.Request.Method, path)
	fmt.Fprintf(&reqBuf, "Host: %s\r\n", c.Request.Host)
	for k, vs := range c.Request.Header {
		for _, v := range vs {
			fmt.Fprintf(&reqBuf, "%s: %s\r\n", k, v)
		}
	}
	fmt.Fprintf(&reqBuf, "\r\n")

	if _, err := stream.Write(reqBuf.Bytes()); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to write ws upgrade to tunnel"})
		return
	}

	bufReader := bufio.NewReader(stream)
	resp, err := http.ReadResponse(bufReader, c.Request)
	if err != nil {
		log.Printf("[proxy] ws ReadResponse error for %s: %v", path, err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to read ws upgrade response"})
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusSwitchingProtocols {
		c.Status(resp.StatusCode)
		io.Copy(c.Writer, resp.Body)
		return
	}

	hijacker, ok := c.Writer.(http.Hijacker)
	if !ok {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "hijack not supported"})
		return
	}
	conn, bufrw, err := hijacker.Hijack()
	if err != nil {
		log.Printf("[proxy] hijack error: %v", err)
		return
	}
	defer conn.Close()

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
		io.Copy(stream, bufrw)
		stream.Close()
		close(done)
	}()
	io.Copy(bufrw, bufReader)
	bufrw.Flush()
	conn.Close()
	<-done
}
