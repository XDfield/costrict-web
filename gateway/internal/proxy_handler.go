package internal

import (
	"bufio"
	"io"
	"log"
	"net/http"

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
		c.Request.URL.Path = path
		c.Request.RequestURI = path

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
