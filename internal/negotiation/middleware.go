package negotiation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
)

// UCPAgentHeader is the header name per UCP spec Section 5.6.
const UCPAgentHeader = "UCP-Agent"

// Middleware creates HTTP middleware that performs UCP negotiation.
// Parses UCP-Agent header, fetches agent profile, negotiates capabilities.
// Stores NegotiatedContext in http.Request context for handlers.
//
// Per UCP spec Section 5: UCP-Agent header is REQUIRED on all requests.
// Requests without it are rejected with 400 Bad Request.
func Middleware(negotiator *Negotiator, logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Skip negotiation for discovery endpoint and health checks
			// These don't require UCP-Agent header
			if isExemptPath(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}

			// Extract UCP-Agent header
			header := r.Header.Get(UCPAgentHeader)
			if header == "" {
				writeNegotiationError(w, http.StatusBadRequest, UCPAgentRequired,
					"UCP-Agent header is required for all requests")
				return
			}

			// Parse profile URL from header
			profileURL, err := ParseUCPAgentHeader(header)
			if err != nil {
				// Headers, profile URLs, and error text can contain credentials.
				logger.Warn("invalid UCP-Agent header",
					slog.String("error_type", fmt.Sprintf("%T", err)))
				writeNegotiationError(w, http.StatusBadRequest, UCPAgentRequired,
					"Invalid UCP-Agent header: "+err.Error())
				return
			}

			// Negotiate capabilities
			ctx, err := negotiator.Negotiate(r.Context(), profileURL)
			if err != nil {
				// Check for version error
				var verErr *VersionError
				if errors.As(err, &verErr) {
					writeNegotiationError(w, http.StatusBadRequest, verErr.Code, verErr.Message)
					return
				}

				// Other negotiation errors
				logger.Error("negotiation failed",
					slog.String("error_type", fmt.Sprintf("%T", err)))
				writeNegotiationError(w, http.StatusBadGateway, "negotiation_failed",
					"Failed to negotiate with agent profile")
				return
			}

			// Log if using degraded mode (fetch failed but using fallback)
			if ctx.FetchError != nil {
				logger.Warn("using fallback profile due to fetch error",
					slog.String("error_type", fmt.Sprintf("%T", ctx.FetchError)))
			}

			// Store context for handlers
			reqCtx := context.WithValue(r.Context(), NegotiationContextKey, ctx)
			next.ServeHTTP(w, r.WithContext(reqCtx))
		})
	}
}

// isExemptPath returns true for paths that don't require UCP-Agent header.
// Discovery endpoint must work without negotiation (chicken-egg problem).
// Health checks are infrastructure, not UCP protocol.
func isExemptPath(path string) bool {
	switch {
	case path == "/.well-known/ucp":
		return true
	case path == "/health" || path == "/healthz":
		return true
	default:
		return false
	}
}

// writeNegotiationError writes a UCP-compliant error response.
// Uses the standard error envelope format.
func writeNegotiationError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	resp := struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}{}
	resp.Error.Code = code
	resp.Error.Message = message

	json.NewEncoder(w).Encode(resp)
}

// GetNegotiatedContext retrieves the negotiation result from request context.
// Returns nil if negotiation was skipped (e.g., exempt path) or not set.
func GetNegotiatedContext(ctx context.Context) *NegotiatedContext {
	v := ctx.Value(NegotiationContextKey)
	if v == nil {
		return nil
	}
	return v.(*NegotiatedContext)
}

// NegotiateForMCP performs negotiation for MCP transport.
// MCP doesn't use middleware - each tool call must negotiate explicitly.
// Extract profile URL from _meta.ucp.profile in request params.
func NegotiateForMCP(
	ctx context.Context,
	negotiator *Negotiator,
	profileURL string,
) (*NegotiatedContext, error) {
	if profileURL == "" {
		return nil, &MissingProfileError{
			Code:    UCPAgentRequired,
			Message: "_meta.ucp.profile is required in MCP requests",
		}
	}

	negotiated, err := negotiator.Negotiate(ctx, profileURL)
	if err != nil {
		return nil, err
	}

	return negotiated, nil
}

// MissingProfileError is returned when profile URL is missing from request.
type MissingProfileError struct {
	Code    string
	Message string
}

func (e *MissingProfileError) Error() string {
	return e.Message
}

// ExtractMCPProfileURL extracts profile URL from MCP request params.
// MCP format: {"_meta": {"ucp": {"profile": "https://..."}}}
// Returns empty string if not present.
func ExtractMCPProfileURL(params map[string]interface{}) string {
	meta, ok := params["_meta"].(map[string]interface{})
	if !ok {
		return ""
	}

	ucp, ok := meta["ucp"].(map[string]interface{})
	if !ok {
		return ""
	}

	profile, ok := ucp["profile"].(string)
	if !ok {
		return ""
	}

	return strings.TrimSpace(profile)
}
