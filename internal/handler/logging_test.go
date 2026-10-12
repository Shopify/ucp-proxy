package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"ucp-proxy/internal/adapter"
	"ucp-proxy/internal/middleware"
	"ucp-proxy/internal/model"
	"ucp-proxy/internal/negotiation"
)

func TestCheckoutLogsOmitSessionTokens(t *testing.T) {
	const token = "synthetic-checkout-bearer-token"
	for _, format := range []string{"text", "json"} {
		for _, checkoutID := range []string{
			"gid://store.example/Cart/" + token,
			"gid://store.example/Checkout/123:" + token,
			"gid://wix.site/Checkout/abc:" + token,
			token, // IDs are opaque; do not depend on a particular adapter's format.
		} {
			for _, outcome := range []string{"success", "error", "api error", "panic"} {
				for _, op := range []struct {
					name, method, suffix, body string
				}{
					{"create", "POST", "", `{"cart_token":"` + token + `"}`},
					{"get", "GET", "", ""},
					{"update", "PUT", "", `{"line_items":[],"discount_codes":[]}`},
					{"complete", "POST", "/complete", `{"payment":{"instruments":[{"id":"` + token + `","selected":true}]}}`},
					{"cancel", "POST", "/cancel", ""},
				} {
					t.Run(format+"/"+checkoutID+"/"+outcome+"/"+op.name, func(t *testing.T) {
						var logs bytes.Buffer
						logger := capturedLogger(&logs, format)
						var receivedID string
						result := func(id string) (*model.Checkout, error) {
							receivedID = id
							switch outcome {
							case "error":
								return nil, &url.Error{Op: "Get", URL: "https://store.example/?cart_token=" + token, Err: errors.New("request failed for " + checkoutID)}
							case "api error":
								return nil, fmt.Errorf("adapter request: %w", model.NewUpstreamError("store", errors.New(token)))
							case "panic":
								panic("Authorization: Bearer " + token + "; checkout=" + checkoutID)
							default:
								return &model.Checkout{ID: checkoutID, Status: model.StatusIncomplete}, nil
							}
						}
						mock := &adapter.Mock{
							CreateCheckoutFunc: func(_ context.Context, req *adapter.CreateCheckoutRequest) (*model.Checkout, error) {
								if req.CartToken != token {
									t.Errorf("cart token was changed: %q", req.CartToken)
								}
								return result(checkoutID)
							},
							GetCheckoutFunc: func(_ context.Context, id string) (*model.Checkout, error) { return result(id) },
							UpdateCheckoutFunc: func(_ context.Context, id string, _ *model.CheckoutUpdateRequest) (*model.Checkout, error) {
								return result(id)
							},
							CompleteCheckoutFunc: func(_ context.Context, id string, _ *model.CheckoutSubmitRequest) (*model.Checkout, error) {
								return result(id)
							},
							CancelCheckoutFunc: func(_ context.Context, id string) (*model.Checkout, error) { return result(id) },
						}
						mux := http.NewServeMux()
						New(mock, nil, logger).RegisterRoutes(mux)
						// Include negotiation's request-context copy, as in the stock server.
						profile, err := mock.GetProfile(context.Background())
						if err != nil {
							t.Fatal(err)
						}
						negotiator := negotiation.NewNegotiator(logTestProfileFetcher{}, profile)
						wrapped := middleware.Chain(middleware.Recovery(logger), middleware.Logging(logger), negotiation.Middleware(negotiator, logger))(mux)
						path := "/checkout-sessions"
						wantPath := path
						if op.name != "create" {
							path += "/" + url.PathEscape(checkoutID) + op.suffix
							wantPath += "/{id}" + op.suffix
						}
						req := httptest.NewRequest(op.method, path+"?token="+token, strings.NewReader(op.body))
						req.Header.Set("UCP-Agent", `profile="https://agent.example/profile"`)
						req.Header.Set("Authorization", "Bearer "+token)
						req.Header.Set("User-Agent", "test-agent/"+token)
						w := httptest.NewRecorder()
						wrapped.ServeHTTP(w, req)

						wantStatus := http.StatusOK
						if op.name == "create" {
							wantStatus = http.StatusCreated
						}
						if outcome != "success" {
							wantStatus = http.StatusInternalServerError
						}
						if outcome == "api error" {
							wantStatus = http.StatusBadGateway
						}
						if w.Code != wantStatus || receivedID != checkoutID {
							t.Fatalf("status=%d, received ID=%q; want %d, %q", w.Code, receivedID, wantStatus, checkoutID)
						}
						assertSessionTokenNotLogged(t, logs.String(), token)
						if !strings.Contains(logs.String(), wantPath) {
							t.Errorf("missing safe path %q: %s", wantPath, &logs)
						}
						if (outcome == "error" || outcome == "panic") && !strings.Contains(logs.String(), "error_type") {
							t.Errorf("missing error type: %s", &logs)
						}
						if outcome == "panic" {
							if !strings.Contains(logs.String(), "stack") {
								t.Errorf("missing panic stack: %s", &logs)
							}
							return
						}
						var checkout model.Checkout
						if err := json.Unmarshal(w.Body.Bytes(), &checkout); err != nil {
							t.Fatal(err)
						}
						if (outcome == "success" || op.name != "create") && checkout.ID != checkoutID {
							t.Errorf("response ID was changed: %q", checkout.ID)
						}
					})
				}
			}
		}
	}
}

// logTestProfileFetcher avoids external requests while exercising negotiation.
type logTestProfileFetcher struct{ err error }

func (f logTestProfileFetcher) Fetch(context.Context, string) (*negotiation.AgentProfile, error) {
	return &negotiation.AgentProfile{UCP: model.UCPMetadata{Version: "2026-01-11"}}, f.err
}

func capturedLogger(w io.Writer, format string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: slog.LevelDebug}
	if format == "json" {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}

func assertSessionTokenNotLogged(t *testing.T, logs, token string) {
	t.Helper()
	if logs == "" {
		t.Fatal("expected operational logs")
	}
	if strings.Contains(logs, token) {
		t.Errorf("session token leaked in logs: %s", logs)
	}
}

func TestMCPLogsOmitSessionTokens(t *testing.T) {
	const token = "synthetic-mcp-bearer-token"
	const id = "gid://wix.site/Checkout/abc:" + token
	for _, format := range []string{"text", "json"} {
		for _, tool := range []string{"create_checkout", "get_checkout", "update_checkout", "complete_checkout", "cancel_checkout"} {
			t.Run(format+"/"+tool, func(t *testing.T) {
				var logs bytes.Buffer
				adapterErr := fmt.Errorf("request failed: %w", model.NewUpstreamError("store", errors.New("Authorization: Bearer "+token)))
				mock := &adapter.Mock{
					CreateCheckoutFunc: func(context.Context, *adapter.CreateCheckoutRequest) (*model.Checkout, error) { return nil, adapterErr },
					GetCheckoutFunc:    func(context.Context, string) (*model.Checkout, error) { return nil, adapterErr },
					UpdateCheckoutFunc: func(context.Context, string, *model.CheckoutUpdateRequest) (*model.Checkout, error) {
						return nil, adapterErr
					},
					CompleteCheckoutFunc: func(context.Context, string, *model.CheckoutSubmitRequest) (*model.Checkout, error) {
						return nil, adapterErr
					},
					CancelCheckoutFunc: func(context.Context, string) (*model.Checkout, error) { return nil, adapterErr },
				}
				mux := http.NewServeMux()
				New(mock, nil, capturedLogger(&logs, format)).RegisterRoutes(mux)
				sessionID := initMCPSession(t, mux)
				input := map[string]interface{}{"meta": testMeta(), "id": id}
				switch tool {
				case "create_checkout":
					delete(input, "id")
					input["checkout"] = map[string]interface{}{"cart_token": token}
				case "update_checkout":
					input["checkout"] = map[string]interface{}{"line_items": []interface{}{}, "discount_codes": []string{}}
				case "complete_checkout":
					input["checkout"] = map[string]interface{}{}
				}
				args, err := json.Marshal(input)
				if err != nil {
					t.Fatal(err)
				}
				body, err := json.Marshal(jsonrpcRequest{JSONRPC: "2.0", ID: 2, Method: "tools/call", Params: toolCallParams{Name: tool, Arguments: args}})
				if err != nil {
					t.Fatal(err)
				}
				req := httptest.NewRequest("POST", "/mcp", bytes.NewReader(body))
				setMCPHeaders(req, sessionID)
				w := httptest.NewRecorder()
				mux.ServeHTTP(w, req)
				if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "internal error") {
					t.Fatalf("unexpected MCP response: %d %s", w.Code, w.Body)
				}
				assertSessionTokenNotLogged(t, logs.String(), token)
				if !strings.Contains(logs.String(), "mcp internal error") || !strings.Contains(logs.String(), "error_type") {
					t.Errorf("missing error diagnostics: %s", &logs)
				}
			})
		}
	}
}

func TestRejectedCheckoutLogsOmitSessionTokens(t *testing.T) {
	const token = "synthetic-rejected-bearer-token"
	for _, tt := range []struct {
		name, method, suffix, body, header string
	}{
		{"missing agent", "GET", "", "", ""},
		{"invalid JSON", "PUT", "", "{", `profile="https://agent.example/profile"`},
		{"missing payment", "POST", "/complete", `{}`, `profile="https://agent.example/profile"`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var logs bytes.Buffer
			logger := capturedLogger(&logs, "json")
			mock := &adapter.Mock{}
			profile, err := mock.GetProfile(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			mux := http.NewServeMux()
			New(mock, nil, logger).RegisterRoutes(mux)
			wrapped := middleware.Logging(logger)(negotiation.Middleware(negotiation.NewNegotiator(logTestProfileFetcher{}, profile), logger)(mux))
			req := httptest.NewRequest(tt.method, "/checkout-sessions/"+token+tt.suffix, strings.NewReader(tt.body))
			req.Header.Set("UCP-Agent", tt.header)
			w := httptest.NewRecorder()
			wrapped.ServeHTTP(w, req)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", w.Code)
			}
			assertSessionTokenNotLogged(t, logs.String(), token)
			if !strings.Contains(logs.String(), "/checkout-sessions/{id}"+tt.suffix) {
				t.Errorf("missing safe path: %s", &logs)
			}
		})
	}
}

func TestResponseErrorLogsOmitSessionTokens(t *testing.T) {
	const token = "synthetic-response-bearer-token"
	for _, format := range []string{"text", "json"} {
		t.Run(format, func(t *testing.T) {
			var logs bytes.Buffer
			h := New(&adapter.Mock{}, nil, capturedLogger(&logs, format))
			err := errors.New("response failed for " + token)
			h.writeJSON(&errorResponseWriter{ResponseRecorder: httptest.NewRecorder(), err: err}, http.StatusOK, &model.Checkout{ID: token})
			h.writeError(httptest.NewRecorder(), err)
			assertSessionTokenNotLogged(t, logs.String(), token)
			if !strings.Contains(logs.String(), "failed to encode response") || !strings.Contains(logs.String(), "error_type") {
				t.Errorf("missing response error diagnostics: %s", &logs)
			}
		})
	}
}

type errorResponseWriter struct {
	*httptest.ResponseRecorder
	err error
}

func (w *errorResponseWriter) Write([]byte) (int, error) { return 0, w.err }

func TestMCPNegotiationLogsOmitSessionTokens(t *testing.T) {
	const token = "synthetic-mcp-profile-bearer-token"
	var logs bytes.Buffer
	mock := &adapter.Mock{}
	profile, err := mock.GetProfile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	profileURL := "https://agent.example/" + token + "?token=" + token
	fetcher := logTestProfileFetcher{err: &url.Error{Op: "Get", URL: profileURL, Err: errors.New(token)}}
	h := New(mock, negotiation.NewNegotiator(fetcher, profile), capturedLogger(&logs, "json"))
	negotiated, err := h.mcpNegotiate(context.Background(), &MCPMeta{UCPAgent: &UCPAgentMeta{Profile: profileURL}})
	if err != nil || negotiated.FetchError == nil {
		t.Fatalf("expected fallback, got %v, %v", negotiated, err)
	}
	assertSessionTokenNotLogged(t, logs.String(), token)
	if !strings.Contains(logs.String(), "using fallback profile due to fetch error") || !strings.Contains(logs.String(), "error_type") {
		t.Errorf("missing fallback diagnostics: %s", &logs)
	}
}
