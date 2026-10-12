# Development Guide

This guide covers how to run the proxy locally, build new adapters, and the patterns established from WooCommerce and Wix implementations.

## Quick Start

### Running Locally

```bash
# Demo mode (in-memory, no external dependencies)
MERCHANT_ID=demo ADAPTER_TYPE=demo go run ./cmd/proxy

# WooCommerce (requires config file)
CONFIG_FILE=config.woo.local.json go run ./cmd/proxy

# Wix (requires config file)
CONFIG_FILE=config.wix.local.json go run ./cmd/proxy
```

### Running Tests

```bash
go test ./...                    # All tests
go test ./internal/woocommerce/  # Specific package
go test -v -run TestCartToUCP    # Specific test
```

### Logging

Checkout IDs contain bearer session tokens. Do not log them, request bodies,
raw URLs or headers, or arbitrary error/panic values that may echo credentials.
Request logs use fixed path labels (for example `/checkout-sessions/{id}`), with
`[unmatched]` for unknown paths, including requests rejected before routing.
When adding endpoints, extend the safe path labels in `internal/middleware`.

Operation names, counts, method, status, duration, remote address, error types,
and panic stack traces remain available for diagnostics. API responses and IDs
are unchanged. Configure any ingress/access logs separately to avoid recording
raw checkout URLs; application logging cannot redact another component's logs.

### Config File Structure

```json
{
  "port": "8080",
  "environment": "development",
  "log_level": "debug",
  "adapter_type": "woocommerce",
  "merchant_id": "my-store",
  "merchant": {
    "store_url": "https://mystore.com",
    "api_key": "ck_xxx",
    "api_secret": "cs_xxx",
    "policy_links": {
      "privacy_policy": "https://mystore.com/privacy",
      "terms_of_service": "https://mystore.com/terms"
    },
    "payment_handlers": {
      "com.google.pay": [{
        "id": "gpay-checkout",
        "version": "2026-01-11",
        "spec": "https://ucp.dev/handlers/google.pay",
        "config": { "...": "handler-specific config" }
      }]
    }
  }
}
```

See `config.example.json` for the full schema with all optional fields.

---

## Building an Adapter

### The Adapter Interface

Every adapter implements the `Adapter` interface defined in `internal/adapter/adapter.go`. The interface has six methods: `GetProfile`, `CreateCheckout`, `GetCheckout`, `UpdateCheckout`, `CompleteCheckout`, and `CancelCheckout`.

### Recommended File Structure

```
internal/{platform}/
├── adapter.go      # Adapter interface impl (entry point, orchestration)
├── client.go       # HTTP client, API calls (keep adapter.go thin)
├── transform.go    # Platform types ↔ UCP types
├── types.go        # Platform API request/response types
├── batch.go        # Batch operations (if platform supports)
└── *_test.go       # Tests for each file
```

**Why this split?**
- `types.go`: Pure data structures, easy to review against API docs
- `transform.go`: Isolated mapping logic, testable without HTTP
- `client.go`: HTTP concerns isolated, easier to mock
- `adapter.go`: Thin orchestration layer

---

## Design Patterns (Lessons from WooCommerce & Wix)

### 1. Stateless Operation via Checkout ID Encoding

The proxy must remain stateless. Encode session tokens in the checkout ID.

**WooCommerce** (cart token):
```
gid://store.com/Cart/{cart_token}
gid://store.com/Checkout/{order_id}:{cart_token}
```

**Wix** (OAuth access token):
```
gid://wix.{site_id}/Checkout/{checkout_id}:{access_token}
```

See `internal/woocommerce/transform.go` and `internal/wix/transform.go` for the actual `BuildCheckoutID` and `ParseCheckoutID` implementations.

**Trade-offs:**
- (+) No server-side session storage
- (+) Horizontally scalable
- (-) IDs are longer/opaque
- (-) Tokens may expire (handle with retry or refresh)

### 2. Status Determination Logic

Status should be derived from platform state, not stored separately. See:
- `internal/woocommerce/transform.go` — `determineCartStatus()` checks items, errors, buyer email, shipping selection
- `internal/wix/transform.go` — `determineStatus()` checks items, buyer info, then returns `requires_escalation` (escalation flow)

**Key insight:** Billing address comes with the payment instrument at complete time (per UCP spec).
Only buyer email is required upfront for order notifications. Full billing address is provided
via `PaymentInstrument.BillingAddress` when calling complete.

Status transitions are:
- `incomplete` → building cart, missing required fields (email, shipping if physical)
- `ready_for_complete` → can call CompleteCheckout with payment instrument
- `requires_escalation` → must redirect to browser checkout
- `completed` → order finalized

### 3. Transform Layer Isolation

Keep platform types and UCP types strictly separated:
- `types.go` — Platform API types matching their docs exactly
- `transform.go` — Conversion functions like `CartToUCP()`, `AddressFromUCP()`

See `internal/woocommerce/types.go` and `internal/woocommerce/transform.go` for examples.

**Benefits:**
- Tests don't need HTTP mocking
- API changes isolated to types.go + transform.go
- Clear audit trail for field mappings

### 4. Authentication Strategies

Different platforms require different auth approaches:

**WooCommerce** — Nonce per mutation. Every write operation requires a fresh nonce fetched via GET request. See `internal/woocommerce/client.go` for `fetchNonce()` and `doCheckoutRequest()`.

**Wix** — OAuth token embedded in checkout ID. Token obtained during `CreateCheckout`, extracted and used for all subsequent API calls. See `internal/wix/adapter.go`.

### 5. Error Mapping

Map platform errors to UCP error types consistently. Standard error constructors are defined in `internal/model/errors.go`: `NewValidationError`, `NewNotFoundError`, `NewUpstreamError`, `NewRateLimitError`.

Each adapter's client maps HTTP status codes and platform-specific error responses to these types. See `internal/woocommerce/client.go` for an example of `parseErrorResponse()`.

### 6. Payment Flow Strategies

**Direct processing** (WooCommerce + Stripe) — Payment token submitted server-side, with 3DS escalation handling. See `internal/woocommerce/client.go` for `CompleteCheckout()`.

**Browser handoff** (Wix) — Current implementation always returns `requires_escalation` with a redirect URL to Wix's hosted checkout. Programmatic payment to be added. See `internal/wix/adapter.go` for `CompleteCheckout()`.

### 7. Totals Calculation

UCP expects specific total types. See `internal/woocommerce/transform.go` for `buildTotals()` implementation.

**Required totals:** `subtotal`, `total`
**Optional:** `items_discount`, `discount`, `fulfillment`, `tax`, `fee`

---

## Capability Negotiation (UCP Spec Section 5)

The proxy implements capability negotiation per UCP spec. This ensures agents only receive capabilities they can handle and merchants only expose capabilities they support.

### Architecture

Negotiation is **transport-agnostic**. The core logic lives in `internal/negotiation/` with transport-specific extraction:

```
┌────────────────────────────────────────────────────────────────┐
│                  NEGOTIATION CORE (shared)                      │
│  internal/negotiation/                                          │
│                                                                 │
│  - ProfileFetcher: fetch + cache agent profiles (HTTP)          │
│  - Intersect(business, agent) → NegotiatedContext               │
│  - NegotiatedContext stored in request context                  │
└────────────────────────────────────────────────────────────────┘
                   ▲                           ▲
                   │ profileURL                │ profileURL
┌──────────────────┴──────────┐   ┌───────────┴────────────────┐
│       REST TRANSPORT        │   │       MCP TRANSPORT        │
│   (middleware pattern)      │   │   (per-invocation)         │
│                             │   │                            │
│  Extract from:              │   │  Extract from:             │
│  UCP-Agent header           │   │  meta.ucp-agent.profile    │
│  (RFC 8941 syntax)          │   │  (in request params)       │
└─────────────────────────────┘   └────────────────────────────┘
```

### REST Transport

Agents include profile URL in the `UCP-Agent` header (RFC 8941 Dictionary Structured Field):

```http
POST /checkout-sessions HTTP/1.1
UCP-Agent: profile="https://agent.example/profile"
Content-Type: application/json
```

The `NegotiationMiddleware` in `internal/negotiation/middleware.go`:
1. Parses `UCP-Agent` header
2. Fetches agent profile (with caching)
3. Computes capability intersection
4. Stores `NegotiatedContext` in request context

### MCP Transport

Agents include profile URL in `meta.ucp-agent.profile`:

```json
{
  "jsonrpc": "2.0",
  "method": "tools/call",
  "params": {
    "name": "create_checkout",
    "arguments": {
      "meta": {
        "ucp-agent": { "profile": "https://agent.example/profile" }
      },
      "checkout": {
        "line_items": [{"product_id": "123", "quantity": 1}]
      }
    }
  }
}
```

Each MCP handler extracts `meta` from tool arguments and calls the negotiation core.

### Profile Caching

The `HTTPProfileFetcher` implements HTTP-compliant caching:

- Respects `Cache-Control: max-age=N` headers
- Supports ETag-based conditional requests (`If-None-Match`)
- Falls back to cached data on fetch failures (stale-while-error)
- Default TTL: 5 minutes when no cache headers present
- LRU eviction when cache exceeds 1000 entries

### Capability Intersection Algorithm (5.7.3)

See `internal/negotiation/intersection.go`:

1. Include business capability if agent has matching `name`
2. Prune orphaned extensions (where parent not in intersection)
3. Repeat until stable

### Adapter Integration

Adapters use `getEffectiveConfig(ctx)` to get negotiated capabilities:

```go
func (c *Client) getEffectiveConfig(ctx context.Context) *model.TransformConfig {
    negotiated := negotiation.GetNegotiatedContext(ctx)
    if negotiated == nil {
        return c.transformConfig // No negotiation, use full config
    }
    cfg := *c.transformConfig
    if negotiated.Capabilities != nil {
        cfg.Capabilities = negotiated.Capabilities
    }
    if negotiated.PaymentHandlers != nil {
        cfg.PaymentHandlers = negotiated.PaymentHandlers
    }
    return &cfg
}
```

All transform calls use this helper to filter capabilities in responses.

### Testing with ucpclient

`ucpclient` hosts its own profile server for spec-compliant testing:

```bash
# ucpclient serves profile at localhost:9999/profile
ucpclient create -proxy http://localhost:8080 -product 60 \
  -profile-port 9999 -profile-file ./my-agent-profile.json
```

Flags:
- `-profile-port`: Port to serve profile (0 = auto-select)
- `-profile-file`: Path to agent profile JSON (uses default if not set)

---

## Testing Patterns

**Transform tests** — Test conversion logic without HTTP. Create platform structs, call transform functions, assert UCP output. See `internal/woocommerce/transform_test.go`.

**Integration tests** — Use `httptest.NewServer` to mock platform APIs. See `internal/woocommerce/client_test.go` for examples.

---

## Deployment

### GCP Cloud Run (Production)

```bash
# Build and deploy
gcloud run deploy ucp-proxy \
  --source . \
  --region us-central1 \
  --allow-unauthenticated \
  --set-env-vars "MERCHANT_ID=my-store,ADAPTER_TYPE=woocommerce"
```

### Environment Variables

| Variable       | Required | Description                         |
| -------------- | -------- | ----------------------------------- |
| `MERCHANT_ID`  | Yes      | Unique identifier for this merchant |
| `ADAPTER_TYPE` | Yes      | `demo`, `woocommerce`, or `wix`     |
| `PORT`         | No       | HTTP port (default: 8080)           |
| `CONFIG_FILE`  | No       | Path to JSON config file            |
| `GCP_PROJECT`  | Prod     | For Secret Manager integration      |

### Secrets (Production)

Use GCP Secret Manager for API credentials:

```bash
echo -n "ck_xxx" | gcloud secrets create woo-api-key --data-file=-
echo -n "cs_xxx" | gcloud secrets create woo-api-secret --data-file=-
```

The proxy loads secrets via `internal/config/secrets.go`.

---

## Checklist for New Adapters

- [ ] Implement `Adapter` interface in `adapter.go`
- [ ] Define platform types in `types.go` matching their API docs
- [ ] Build transform layer in `transform.go`
- [ ] Implement HTTP client in `client.go`
- [ ] Design checkout ID format with embedded session token
- [ ] Implement status determination logic
- [ ] Map platform errors to UCP error types
- [ ] Handle payment (direct or escalation)
- [ ] Write transform tests (no HTTP)
- [ ] Write integration tests (with HTTP mock)
- [ ] Add config loading in `internal/config/`
- [ ] Register adapter type in `cmd/proxy/main.go`
- [ ] Document platform-specific gotchas
