// Package handler provides HTTP handlers for the UCP proxy API.
package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"ucp-proxy/internal/adapter"
	"ucp-proxy/internal/model"
	"ucp-proxy/internal/negotiation"
)

// Handler holds dependencies for HTTP handlers.
type Handler struct {
	adapter    adapter.Adapter
	negotiator *negotiation.Negotiator
	logger     *slog.Logger
}

// New creates a new Handler with the given adapter, negotiator, and logger.
// The negotiator may be nil to disable UCP negotiation (for testing/backward compat).
func New(a adapter.Adapter, negotiator *negotiation.Negotiator, logger *slog.Logger) *Handler {
	return &Handler{
		adapter:    a,
		negotiator: negotiator,
		logger:     logger,
	}
}

// RegisterRoutes registers all HTTP routes with the given ServeMux.
// Uses Go 1.22+ method routing patterns.
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	// Discovery endpoint
	mux.HandleFunc("GET /.well-known/ucp", h.handleWellKnown)

	// REST transport - checkout operations per UCP spec
	mux.HandleFunc("POST /checkout-sessions", h.handleCreateCheckout)
	mux.HandleFunc("GET /checkout-sessions/{id}", h.handleGetCheckout)
	mux.HandleFunc("PUT /checkout-sessions/{id}", h.handleUpdateCheckout)
	mux.HandleFunc("POST /checkout-sessions/{id}/complete", h.handleCompleteCheckout)
	mux.HandleFunc("POST /checkout-sessions/{id}/cancel", h.handleCancelCheckout)

	// MCP transport - JSON-RPC endpoint using official MCP SDK
	mux.Handle("/mcp", h.NewMCPHandler())

	// Health check
	mux.HandleFunc("GET /health", h.handleHealth)
	mux.HandleFunc("GET /healthz", h.handleHealth)
}

// === Response Helpers ===

// writeJSON sends a JSON response with the given status code.
func (h *Handler) writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(data); err != nil {
		h.logger.Error("failed to encode response", slog.String("error_type", fmt.Sprintf("%T", err)))
	}
}

// writeError sends an error response, extracting status/code from APIError if present.
// Uses errors.As() to unwrap error chains (e.g., fmt.Errorf wrapping).
func (h *Handler) writeError(w http.ResponseWriter, err error) {
	var apiErr *model.APIError

	if errors.As(err, &apiErr) {
		// Found APIError in error chain - use it
	} else {
		// Wrap unexpected errors
		apiErr = &model.APIError{
			Code:       "INTERNAL_ERROR",
			Message:    "an internal error occurred",
			StatusCode: http.StatusInternalServerError,
		}
		h.logger.Error("internal error", slog.String("error_type", fmt.Sprintf("%T", err)))
	}

	h.writeJSON(w, apiErr.StatusCode, errorResponse{
		Error: errorBody{
			Code:    apiErr.Code,
			Message: apiErr.Message,
		},
	})
}

// errorResponse is the JSON structure for error responses.
type errorResponse struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// MaxRequestBodySize limits JSON request bodies to 1MB to prevent DoS.
const MaxRequestBodySize = 1 << 20 // 1MB

// writeCheckoutError returns a UCP-compliant error response for checkout operations.
// Instead of {"error": {...}}, returns a Checkout with status and messages array.
// This is required because UCP spec expects all checkout responses to be Checkout objects.
func (h *Handler) writeCheckoutError(w http.ResponseWriter, checkoutID string, err error) {
	var apiErr *model.APIError

	if errors.As(err, &apiErr) {
		// Found APIError in error chain - use it
	} else {
		// Wrap unexpected errors
		apiErr = &model.APIError{
			Code:       "INTERNAL_ERROR",
			Message:    "an internal error occurred",
			StatusCode: http.StatusInternalServerError,
		}
		// Error text can include checkout tokens, upstream URLs, or headers.
		h.logger.Error("internal error", slog.String("error_type", fmt.Sprintf("%T", err)))
	}

	// Map HTTP status to UCP message severity
	severity := mapStatusToSeverity(apiErr.StatusCode)

	// Build minimal checkout response with error message
	checkout := &model.Checkout{
		ID:     checkoutID,
		Status: model.StatusIncomplete,
		Messages: []model.Message{
			model.NewErrorMessage(apiErr.Code, apiErr.Message, severity),
		},
		// Provide empty required fields to satisfy schema
		UCP:       model.UCPMetadata{Version: "2026-01-11"},
		LineItems: []model.LineItem{},
		Totals:    []model.Total{},
		Links:     []model.Link{},
		Payment:   model.Payment{},
	}

	h.writeJSON(w, apiErr.StatusCode, checkout)
}

// mapStatusToSeverity converts HTTP status codes to UCP message severity.
// Severity determines whether the agent can retry with different input.
func mapStatusToSeverity(statusCode int) model.MessageSeverity {
	switch statusCode {
	case 400, 402, 422: // Bad Request, Payment Required, Unprocessable
		return model.SeverityRecoverable // Agent can fix input and retry
	case 404:
		return model.SeverityUnrecoverable // Resource doesn't exist
	case 401, 403:
		return model.SeverityUnrecoverable // Auth issues need intervention
	case 429:
		return model.SeverityRecoverable // Rate limit - can retry later
	case 500, 502, 503, 504:
		return model.SeverityUnrecoverable // Server issues
	default:
		return model.SeverityUnrecoverable
	}
}

// decodeJSON reads JSON from request body into v.
// Limits body size to MaxRequestBodySize to prevent memory exhaustion.
// Returns an APIError if decoding fails.
func decodeJSON(r *http.Request, v interface{}) error {
	// Limit request body size to prevent DoS
	r.Body = http.MaxBytesReader(nil, r.Body, MaxRequestBodySize)

	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		// Don't expose internal error details to client
		return model.NewValidationError("body", "invalid JSON")
	}
	return nil
}
