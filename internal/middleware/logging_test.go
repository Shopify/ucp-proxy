package middleware

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestRequestLogsOmitSessionTokens(t *testing.T) {
	const token = "synthetic-request-bearer-token"
	id := "gid://store.example/Cart/" + token
	for _, tt := range []struct {
		name, method, path, wantPath string
		status                       int
	}{
		{"escaped", "GET", "/checkout-sessions/" + url.PathEscape(id), "/checkout-sessions/{id}", 200},
		{"unescaped redirect", "GET", "/checkout-sessions/" + id, "/checkout-sessions/{id}", 301},
		{"unknown action", "GET", "/checkout-sessions/" + url.PathEscape(id) + "/unknown", "/checkout-sessions/{id}", 404},
		{"wrong method", "PATCH", "/checkout-sessions/" + url.PathEscape(id), "/checkout-sessions/{id}", 405},
		{"unknown path", "GET", "/" + token, "[unmatched]", 404},
		{"health query", "GET", "/health?token=" + token, "/health", 200},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var logs bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&logs, nil))
			mux := http.NewServeMux()
			ok := func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }
			mux.HandleFunc("GET /checkout-sessions/{id}", ok)
			mux.HandleFunc("GET /health", ok)
			w := httptest.NewRecorder()
			Logging(logger)(mux).ServeHTTP(w, httptest.NewRequest(tt.method, tt.path, nil))
			// Go 1.26 changed ServeMux path-cleaning redirects from 301 to 307.
			redirect := tt.status == http.StatusMovedPermanently && w.Code == http.StatusTemporaryRedirect
			if w.Code != tt.status && !redirect {
				t.Fatalf("status = %d, want %d", w.Code, tt.status)
			}
			if strings.Contains(logs.String(), token) {
				t.Errorf("session token leaked: %s", &logs)
			}
			if !strings.Contains(logs.String(), tt.wantPath) {
				t.Errorf("missing safe path %q: %s", tt.wantPath, &logs)
			}
			for _, field := range []string{"method", "status", "duration", "remote"} {
				if !strings.Contains(logs.String(), field) {
					t.Errorf("missing %s: %s", field, &logs)
				}
			}
		})
	}
}

func TestRecoveryLogsOmitSessionTokensBeforeRouting(t *testing.T) {
	const token = "synthetic-panic-bearer-token"
	for _, value := range []any{token, errors.New(token), struct{ Token string }{token}} {
		var logs bytes.Buffer
		logger := slog.New(slog.NewJSONHandler(&logs, nil))
		wrapped := Recovery(logger)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(value) }))
		w := httptest.NewRecorder()
		wrapped.ServeHTTP(w, httptest.NewRequest("GET", "/checkout-sessions/"+token, nil))
		if w.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want 500", w.Code)
		}
		if strings.Contains(logs.String(), token) || !strings.Contains(logs.String(), "error_type") || !strings.Contains(logs.String(), "stack") {
			t.Errorf("unsafe or missing panic diagnostics: %s", &logs)
		}
	}
}
