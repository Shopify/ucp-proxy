package negotiation

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

func TestNegotiationLogsOmitSessionTokens(t *testing.T) {
	const token = "synthetic-negotiation-bearer-token"
	profileURL := "https://agent.example/" + token + "?token=" + token
	for _, tt := range []struct {
		name, header, message string
		status                int
	}{
		{"invalid header", "invalid=" + token + "@", "invalid UCP-Agent header", http.StatusBadRequest},
		{"fallback", "profile=\"" + profileURL + "\"", "using fallback profile due to fetch error", http.StatusNoContent},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var logs bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&logs, nil))
			fetcher := &mockProfileFetcher{err: &url.Error{Op: "Get", URL: profileURL, Err: errors.New("Authorization: Bearer " + token)}}
			wrapped := Middleware(testNegotiator(fetcher), logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
			req := httptest.NewRequest("GET", "/checkout-sessions/"+token, nil)
			req.Header.Set(UCPAgentHeader, tt.header)
			w := httptest.NewRecorder()
			wrapped.ServeHTTP(w, req)
			if w.Code != tt.status {
				t.Fatalf("status = %d, want %d: %s", w.Code, tt.status, w.Body)
			}
			if strings.Contains(logs.String(), token) {
				t.Errorf("session token leaked: %s", &logs)
			}
			if !strings.Contains(logs.String(), tt.message) || !strings.Contains(logs.String(), "error_type") {
				t.Errorf("missing negotiation diagnostics: %s", &logs)
			}
		})
	}
}
