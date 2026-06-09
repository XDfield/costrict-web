package proxy

import (
	"bytes"
	"net/http/httptest"
	"testing"
)

func TestInterceptWriter_Normal(t *testing.T) {
	rec := httptest.NewRecorder()
	w := NewInterceptWriter(rec, 1024)

	w.WriteHeader(200)
	w.Write([]byte("hello"))

	if w.Status() != 200 {
		t.Errorf("expected status 200, got %d", w.Status())
	}
	if w.Overflowed() {
		t.Error("should not be overflowed")
	}
	if string(w.Buffer()) != "hello" {
		t.Errorf("expected buffer 'hello', got '%s'", w.Buffer())
	}
}

func TestInterceptWriter_ExactMax(t *testing.T) {
	rec := httptest.NewRecorder()
	w := NewInterceptWriter(rec, 5)

	data := []byte("12345")
	w.Write(data)

	if w.Overflowed() {
		t.Error("should not overflow at exact max")
	}
	if !bytes.Equal(w.Buffer(), data) {
		t.Errorf("expected buffer %q, got %q", data, w.Buffer())
	}
}

func TestInterceptWriter_OverMax(t *testing.T) {
	rec := httptest.NewRecorder()
	w := NewInterceptWriter(rec, 5)

	w.Write([]byte("123456"))

	if !w.Overflowed() {
		t.Error("should be overflowed")
	}
	if w.buf.Len() != 0 {
		t.Error("buffer should be flushed on overflow")
	}
	if rec.Body.String() != "123456" {
		t.Errorf("expected passthrough '123456', got '%s'", rec.Body.String())
	}
}

func TestInterceptWriter_MultipleWrites(t *testing.T) {
	rec := httptest.NewRecorder()
	w := NewInterceptWriter(rec, 10)

	w.Write([]byte("123"))
	w.Write([]byte("456"))
	w.Write([]byte("7890"))

	if w.Overflowed() {
		t.Error("should not overflow, total 10 bytes = max")
	}
	if string(w.Buffer()) != "1234567890" {
		t.Errorf("expected '1234567890', got '%s'", w.Buffer())
	}
}

func TestInterceptWriter_MultipleWritesOverflow(t *testing.T) {
	rec := httptest.NewRecorder()
	w := NewInterceptWriter(rec, 10)

	w.Write([]byte("123"))
	w.Write([]byte("456"))
	w.Write([]byte("78901"))

	if !w.Overflowed() {
		t.Error("should be overflowed")
	}
	body := rec.Body.String()
	if body != "12345678901" {
		t.Errorf("expected passthrough '12345678901', got '%s'", body)
	}
}

func TestInterceptWriter_Flush(t *testing.T) {
	rec := httptest.NewRecorder()
	w := NewInterceptWriter(rec, 1024)

	w.Write([]byte("data"))
	w.Flush()
}
