// Package middleware provides HTTP middleware for the UCP proxy.
package middleware

import (
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"
)

// Logging returns middleware that logs request details.
// Logs method, safe path label, status, duration, and remote address.
// Raw URLs and headers may contain checkout session tokens and are not logged.
func Logging(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()

			// Wrap response writer to capture status code
			wrapped := &responseWriter{ResponseWriter: w, status: http.StatusOK}

			next.ServeHTTP(wrapped, r)

			logger.Info("request",
				slog.String("method", r.Method),
				slog.String("path", logPath(r.URL.Path)),
				slog.Int("status", wrapped.status),
				slog.Duration("duration", time.Since(start)),
				slog.String("remote", r.RemoteAddr),
			)
		})
	}
}

// Recovery returns middleware that recovers from panics.
// Logs the panic type and stack trace, returns 500 Internal Server Error.
// Panic values can contain checkout tokens, so they must not be formatted.
func Recovery(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if err := recover(); err != nil {
					logger.Error("panic recovered",
						slog.String("error_type", fmt.Sprintf("%T", err)),
						slog.String("path", logPath(r.URL.Path)),
						slog.String("method", r.Method),
						slog.String("stack", string(debug.Stack())),
					)

					// Avoid writing if headers already sent
					http.Error(w, "Internal Server Error", http.StatusInternalServerError)
				}
			}()

			next.ServeHTTP(wrapped(w), r)
		})
	}
}

// logPath returns only fixed labels, never a path containing a bearer checkout ID.
// Request.Pattern is not reliable here: negotiation copies the request before
// routing, and redirects, rejections, and panics can happen before a route matches.
func logPath(path string) string {
	switch path {
	case "/checkout-sessions", "/mcp", "/health", "/healthz", "/.well-known/ucp":
		return path
	}
	if strings.HasPrefix(path, "/checkout-sessions/") {
		switch {
		case strings.HasSuffix(path, "/complete"):
			return "/checkout-sessions/{id}/complete"
		case strings.HasSuffix(path, "/cancel"):
			return "/checkout-sessions/{id}/cancel"
		default:
			return "/checkout-sessions/{id}"
		}
	}
	return "[unmatched]"
}

// responseWriter wraps http.ResponseWriter to capture status code.
type responseWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *responseWriter) WriteHeader(status int) {
	if !w.wroteHeader {
		w.status = status
		w.wroteHeader = true
		w.ResponseWriter.WriteHeader(status)
	}
}

func (w *responseWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

// wrapped returns a responseWriter, handling the case where w is already wrapped.
func wrapped(w http.ResponseWriter) http.ResponseWriter {
	if _, ok := w.(*responseWriter); ok {
		return w
	}
	return &responseWriter{ResponseWriter: w, status: http.StatusOK}
}

// Chain combines multiple middleware into a single middleware.
// Middleware is applied in order: first middleware wraps the last.
func Chain(middlewares ...func(http.Handler) http.Handler) func(http.Handler) http.Handler {
	return func(final http.Handler) http.Handler {
		for i := len(middlewares) - 1; i >= 0; i-- {
			final = middlewares[i](final)
		}
		return final
	}
}
