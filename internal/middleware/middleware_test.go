package middleware

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLogging(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	handler := Logging(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte("created"))
	}))

	req := httptest.NewRequest("POST", "/checkout-sessions", nil)
	req.Header.Set("User-Agent", "test-agent")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Errorf("Status = %d, want %d", w.Code, http.StatusCreated)
	}

	logged := buf.String()

	// Verify log contains expected fields
	checks := []string{"method=POST", "path=/checkout-sessions", "status=201"}
	for _, check := range checks {
		if !strings.Contains(logged, check) {
			t.Errorf("Log missing %q: %s", check, logged)
		}
	}
}

func TestLoggingDefaultStatus(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	handler := Logging(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Don't write status - should default to 200
		w.Write([]byte("ok"))
	}))

	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	logged := buf.String()
	if !strings.Contains(logged, "status=200") {
		t.Errorf("Expected status=200 in log: %s", logged)
	}
}

func TestRecovery(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	handler := Recovery(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("test panic")
	}))

	req := httptest.NewRequest("GET", "/panic", nil)
	w := httptest.NewRecorder()

	// Should not panic
	handler.ServeHTTP(w, req)

	// Should return 500
	if w.Code != http.StatusInternalServerError {
		t.Errorf("Status = %d, want %d", w.Code, http.StatusInternalServerError)
	}

	// Should log the panic
	logged := buf.String()
	if !strings.Contains(logged, "panic recovered") {
		t.Errorf("Log missing panic recovery: %s", logged)
	}
	if !strings.Contains(logged, "error_type=string") || !strings.Contains(logged, "stack=") {
		t.Errorf("Log missing panic type or stack: %s", logged)
	}
	if strings.Contains(logged, "test panic") {
		t.Errorf("Log must not include the panic value: %s", logged)
	}
}

func TestRecoveryNoPanic(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	handler := Recovery(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))

	req := httptest.NewRequest("GET", "/ok", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Status = %d, want %d", w.Code, http.StatusOK)
	}
	if w.Body.String() != "ok" {
		t.Errorf("Body = %s, want ok", w.Body.String())
	}
}

func TestChain(t *testing.T) {
	var order []string

	middleware1 := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			order = append(order, "m1-before")
			next.ServeHTTP(w, r)
			order = append(order, "m1-after")
		})
	}

	middleware2 := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			order = append(order, "m2-before")
			next.ServeHTTP(w, r)
			order = append(order, "m2-after")
		})
	}

	handler := Chain(middleware1, middleware2)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		order = append(order, "handler")
	}))

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	expected := []string{"m1-before", "m2-before", "handler", "m2-after", "m1-after"}
	if len(order) != len(expected) {
		t.Fatalf("Order length = %d, want %d", len(order), len(expected))
	}
	for i, v := range expected {
		if order[i] != v {
			t.Errorf("Order[%d] = %s, want %s", i, order[i], v)
		}
	}
}

func TestResponseWriterMultipleWriteHeader(t *testing.T) {
	w := httptest.NewRecorder()
	rw := &responseWriter{ResponseWriter: w, status: http.StatusOK}

	// First write should work
	rw.WriteHeader(http.StatusCreated)
	if rw.status != http.StatusCreated {
		t.Errorf("Status = %d, want %d", rw.status, http.StatusCreated)
	}

	// Second write should be ignored
	rw.WriteHeader(http.StatusNotFound)
	if rw.status != http.StatusCreated {
		t.Errorf("Status after second write = %d, want %d", rw.status, http.StatusCreated)
	}

	// Underlying writer should have received first status
	if w.Code != http.StatusCreated {
		t.Errorf("Underlying status = %d, want %d", w.Code, http.StatusCreated)
	}
}

func TestResponseWriterImplicitStatus(t *testing.T) {
	w := httptest.NewRecorder()
	rw := &responseWriter{ResponseWriter: w, status: http.StatusOK}

	// Write without WriteHeader should trigger implicit 200
	rw.Write([]byte("test"))

	if !rw.wroteHeader {
		t.Error("wroteHeader should be true after Write")
	}
	if rw.status != http.StatusOK {
		t.Errorf("Status = %d, want %d", rw.status, http.StatusOK)
	}
}
