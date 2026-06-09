package proxy

import (
	"bufio"
	"bytes"
	"net"
	"net/http"

	"github.com/gin-gonic/gin"
)

type InterceptWriter struct {
	http.ResponseWriter
	buf        bytes.Buffer
	statusCode int
	maxBytes   int64
	overflowed bool
}

func NewInterceptWriter(w http.ResponseWriter, maxBytes int64) *InterceptWriter {
	return &InterceptWriter{
		ResponseWriter: w,
		maxBytes:       maxBytes,
		statusCode:     http.StatusOK,
	}
}

func (w *InterceptWriter) WriteHeader(code int) {
	w.statusCode = code
}

func (w *InterceptWriter) Write(b []byte) (int, error) {
	if w.overflowed {
		return w.ResponseWriter.Write(b)
	}

	if int64(w.buf.Len()+len(b)) > w.maxBytes {
		w.overflowed = true
		if w.buf.Len() > 0 {
			if _, err := w.ResponseWriter.Write(w.buf.Bytes()); err != nil {
				return 0, err
			}
			w.buf.Reset()
		}
		return w.ResponseWriter.Write(b)
	}

	return w.buf.Write(b)
}

func (w *InterceptWriter) Overflowed() bool {
	return w.overflowed
}

func (w *InterceptWriter) Buffer() []byte {
	return w.buf.Bytes()
}

func (w *InterceptWriter) Status() int {
	return w.statusCode
}

func (w *InterceptWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *InterceptWriter) CloseNotify() <-chan bool {
	if cn, ok := w.ResponseWriter.(http.CloseNotifier); ok {
		return cn.CloseNotify()
	}
	ch := make(chan bool, 1)
	return ch
}

func (w *InterceptWriter) Size() int {
	return w.buf.Len()
}

func (w *InterceptWriter) Written() bool {
	return w.statusCode != http.StatusOK || w.buf.Len() > 0 || w.overflowed
}

func (w *InterceptWriter) WriteHeaderNow() {
	if !w.Written() {
		w.ResponseWriter.WriteHeader(w.statusCode)
	}
}

func (w *InterceptWriter) FlushHeaders() {
	w.ResponseWriter.WriteHeader(w.statusCode)
}

func (w *InterceptWriter) Pusher() http.Pusher {
	if pusher, ok := w.ResponseWriter.(http.Pusher); ok {
		return pusher
	}
	return nil
}

func (w *InterceptWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := w.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, http.ErrNotSupported
}

func (w *InterceptWriter) WriteString(s string) (int, error) {
	return w.Write([]byte(s))
}

var _ gin.ResponseWriter = (*InterceptWriter)(nil)
