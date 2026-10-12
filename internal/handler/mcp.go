// MCP transport handler for UCP proxy using the official MCP Go SDK.
// Exposes checkout operations as MCP tools per UCP spec.
package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ucp-proxy/internal/adapter"
	"ucp-proxy/internal/model"
	"ucp-proxy/internal/negotiation"
)

// === MCP Meta Types ===
// Per spec: meta contains request metadata, maps to HTTP headers.
// - UCP-Agent header → meta["ucp-agent"]
// - Idempotency-Key header → meta["idempotency-key"]

// MCPMeta represents request metadata in MCP requests.
type MCPMeta struct {
	UCPAgent       *UCPAgentMeta `json:"ucp-agent"`
	IdempotencyKey string        `json:"idempotency-key,omitempty"`
}

// UCPAgentMeta contains agent identification per UCP spec Section 5.
type UCPAgentMeta struct {
	Profile string `json:"profile"`
}

// === MCP Tool Input/Output Types ===
// Per UCP spec: Params structure is {meta, id?, checkout}
// - meta: request metadata (required on all requests)
// - id: resource identifier (required for get/update/complete/cancel)
// - checkout: domain payload (required for create/update/complete)

// CreateCheckoutInput is the input schema for create_checkout tool.
type CreateCheckoutInput struct {
	Meta     MCPMeta               `json:"meta" jsonschema:"request metadata,required"`
	Checkout CreateCheckoutPayload `json:"checkout" jsonschema:"checkout data,required"`
}

// CreateCheckoutPayload contains the checkout creation data.
type CreateCheckoutPayload struct {
	CartToken       string               `json:"cart_token,omitempty" jsonschema:"existing cart token to use"`
	LineItems       []LineItemInput      `json:"line_items,omitempty" jsonschema:"line items to add to checkout"`
	ShippingAddress *model.PostalAddress `json:"shipping_address,omitempty" jsonschema:"shipping address"`
	BillingAddress  *model.PostalAddress `json:"billing_address,omitempty" jsonschema:"billing address"`
}

// LineItemInput represents a line item in create checkout.
type LineItemInput struct {
	ProductID string `json:"product_id" jsonschema:"product ID,required"`
	Quantity  int    `json:"quantity" jsonschema:"quantity,required"`
}

// GetCheckoutInput is the input schema for get_checkout tool.
type GetCheckoutInput struct {
	Meta MCPMeta `json:"meta" jsonschema:"request metadata,required"`
	ID   string  `json:"id" jsonschema:"checkout ID,required"`
}

// UpdateCheckoutInput is the input schema for update_checkout tool.
// Uses full PUT semantics: client must send complete desired state.
type UpdateCheckoutInput struct {
	Meta     MCPMeta               `json:"meta" jsonschema:"request metadata,required"`
	ID       string                `json:"id" jsonschema:"checkout ID,required"`
	Checkout UpdateCheckoutPayload `json:"checkout" jsonschema:"checkout data,required"`
}

// UpdateCheckoutPayload contains the checkout update data.
type UpdateCheckoutPayload struct {
	LineItems           []model.LineItemRequest `json:"line_items" jsonschema:"complete line items (required),required"`
	DiscountCodes       []string                `json:"discount_codes" jsonschema:"discount codes (empty array = none),required"`
	ShippingAddress     *model.PostalAddress    `json:"shipping_address,omitempty" jsonschema:"shipping address"`
	BillingAddress      *model.PostalAddress    `json:"billing_address,omitempty" jsonschema:"billing address"`
	FulfillmentOptionID string                  `json:"fulfillment_option_id,omitempty" jsonschema:"selected fulfillment option ID"`
	Buyer               *model.Buyer            `json:"buyer,omitempty" jsonschema:"buyer information"`
}

// CompleteCheckoutInput is the input schema for complete_checkout tool.
// Per spec: idempotency-key is required in meta for complete_checkout.
type CompleteCheckoutInput struct {
	Meta     MCPMeta                 `json:"meta" jsonschema:"request metadata (idempotency-key required),required"`
	ID       string                  `json:"id" jsonschema:"checkout ID,required"`
	Checkout CompleteCheckoutPayload `json:"checkout" jsonschema:"checkout data,required"`
}

// CompleteCheckoutPayload contains the checkout completion data.
type CompleteCheckoutPayload struct {
	Payment *model.Payment `json:"payment,omitempty" jsonschema:"payment instruments"`
}

// CancelCheckoutInput is the input schema for cancel_checkout tool.
// Per spec: idempotency-key is required in meta for cancel_checkout.
type CancelCheckoutInput struct {
	Meta MCPMeta `json:"meta" jsonschema:"request metadata (idempotency-key required),required"`
	ID   string  `json:"id" jsonschema:"checkout ID,required"`
}

// NewMCPServer creates an MCP server with checkout tools registered.
// The server exposes the same operations as the REST API but via MCP protocol.
func (h *Handler) NewMCPServer() *mcp.Server {
	server := mcp.NewServer(
		&mcp.Implementation{
			Name:    "ucp-proxy",
			Version: "1.0.0",
		},
		&mcp.ServerOptions{
			Instructions: "UCP Proxy - Universal Commerce Protocol checkout operations. " +
				"Use these tools to create, update, and complete checkout sessions.",
		},
	)

	// Register checkout tools
	mcp.AddTool(server, &mcp.Tool{
		Name:        "create_checkout",
		Description: "Create a new checkout session. Optionally provide line items or an existing cart token.",
	}, h.mcpCreateCheckout)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_checkout",
		Description: "Get the current state of a checkout session.",
	}, h.mcpGetCheckout)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "update_checkout",
		Description: "Update a checkout session. Requires full state: line_items and discount_codes must always be provided.",
	}, h.mcpUpdateCheckout)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "complete_checkout",
		Description: "Complete a checkout session and place the order. Requires payment instruments.",
	}, h.mcpCompleteCheckout)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "cancel_checkout",
		Description: "Cancel a checkout session.",
	}, h.mcpCancelCheckout)

	return server
}

// NewMCPHandler returns an HTTP handler for the MCP endpoint.
// Mount this at /mcp on your mux.
func (h *Handler) NewMCPHandler() http.Handler {
	server := h.NewMCPServer()
	return mcp.NewStreamableHTTPHandler(
		func(r *http.Request) *mcp.Server { return server },
		nil,
	)
}

// === Tool Handlers ===

func (h *Handler) mcpCreateCheckout(
	ctx context.Context,
	req *mcp.CallToolRequest,
	input CreateCheckoutInput,
) (*mcp.CallToolResult, *model.Checkout, error) {
	// Negotiate capabilities per UCP spec Section 5
	_, err := h.mcpNegotiate(ctx, &input.Meta)
	if err != nil {
		return nil, nil, err
	}

	// Map to adapter request
	adapterReq := &adapter.CreateCheckoutRequest{
		CartToken:       input.Checkout.CartToken,
		ShippingAddress: input.Checkout.ShippingAddress,
		BillingAddress:  input.Checkout.BillingAddress,
	}

	// Convert MCP LineItemInput to model.LineItemRequest
	if len(input.Checkout.LineItems) > 0 {
		adapterReq.LineItems = make([]model.LineItemRequest, len(input.Checkout.LineItems))
		for i, li := range input.Checkout.LineItems {
			adapterReq.LineItems[i] = model.LineItemRequest{
				ProductID: li.ProductID,
				Quantity:  li.Quantity,
			}
		}
	}

	checkout, err := h.adapter.CreateCheckout(ctx, adapterReq)
	if err != nil {
		return nil, nil, h.mcpError(err)
	}

	return nil, checkout, nil
}

func (h *Handler) mcpGetCheckout(
	ctx context.Context,
	req *mcp.CallToolRequest,
	input GetCheckoutInput,
) (*mcp.CallToolResult, *model.Checkout, error) {
	// Negotiate capabilities per UCP spec Section 5
	_, err := h.mcpNegotiate(ctx, &input.Meta)
	if err != nil {
		return nil, nil, err
	}

	if input.ID == "" {
		return nil, nil, fmt.Errorf("id is required")
	}

	checkout, err := h.adapter.GetCheckout(ctx, input.ID)
	if err != nil {
		return nil, nil, h.mcpError(err)
	}

	return nil, checkout, nil
}

func (h *Handler) mcpUpdateCheckout(
	ctx context.Context,
	req *mcp.CallToolRequest,
	input UpdateCheckoutInput,
) (*mcp.CallToolResult, *model.Checkout, error) {
	// Negotiate capabilities per UCP spec Section 5
	_, err := h.mcpNegotiate(ctx, &input.Meta)
	if err != nil {
		return nil, nil, err
	}

	if input.ID == "" {
		return nil, nil, fmt.Errorf("id is required")
	}

	// Map to adapter request (full PUT semantics)
	updateReq := &model.CheckoutUpdateRequest{
		LineItems:           input.Checkout.LineItems,
		DiscountCodes:       input.Checkout.DiscountCodes,
		ShippingAddress:     input.Checkout.ShippingAddress,
		BillingAddress:      input.Checkout.BillingAddress,
		FulfillmentOptionID: input.Checkout.FulfillmentOptionID,
		Buyer:               input.Checkout.Buyer,
	}

	checkout, err := h.adapter.UpdateCheckout(ctx, input.ID, updateReq)
	if err != nil {
		return nil, nil, h.mcpError(err)
	}

	return nil, checkout, nil
}

func (h *Handler) mcpCompleteCheckout(
	ctx context.Context,
	req *mcp.CallToolRequest,
	input CompleteCheckoutInput,
) (*mcp.CallToolResult, *model.Checkout, error) {
	// Negotiate capabilities per UCP spec Section 5
	_, err := h.mcpNegotiate(ctx, &input.Meta)
	if err != nil {
		return nil, nil, err
	}

	if input.ID == "" {
		return nil, nil, fmt.Errorf("id is required")
	}

	// Map to adapter request
	submitReq := &model.CheckoutSubmitRequest{}
	if input.Checkout.Payment != nil {
		submitReq.Payment = *input.Checkout.Payment
	}

	checkout, err := h.adapter.CompleteCheckout(ctx, input.ID, submitReq)
	if err != nil {
		return nil, nil, h.mcpError(err)
	}

	return nil, checkout, nil
}

func (h *Handler) mcpCancelCheckout(
	ctx context.Context,
	req *mcp.CallToolRequest,
	input CancelCheckoutInput,
) (*mcp.CallToolResult, *model.Checkout, error) {
	// Negotiate capabilities per UCP spec Section 5
	_, err := h.mcpNegotiate(ctx, &input.Meta)
	if err != nil {
		return nil, nil, err
	}

	if input.ID == "" {
		return nil, nil, fmt.Errorf("id is required")
	}

	checkout, err := h.adapter.CancelCheckout(ctx, input.ID)
	if err != nil {
		return nil, nil, h.mcpError(err)
	}

	return nil, checkout, nil
}

// mcpError converts adapter errors to MCP-friendly errors.
func (h *Handler) mcpError(err error) error {
	if apiErr, ok := err.(*model.APIError); ok {
		return fmt.Errorf("%s: %s", apiErr.Code, apiErr.Message)
	}
	// Don't leak internal error details
	h.logger.Error("mcp internal error", "error_type", fmt.Sprintf("%T", err))
	return fmt.Errorf("internal error")
}

// mcpNegotiate extracts profile URL from meta.ucp-agent.profile and performs negotiation.
// Returns negotiated context or error if negotiation fails/profile missing.
func (h *Handler) mcpNegotiate(ctx context.Context, meta *MCPMeta) (*negotiation.NegotiatedContext, error) {
	// Skip negotiation if negotiator not configured (backward compat/testing)
	if h.negotiator == nil {
		return nil, nil
	}

	// Extract profile URL from meta.ucp-agent.profile
	profileURL := ""
	if meta != nil && meta.UCPAgent != nil {
		profileURL = meta.UCPAgent.Profile
	}
	if profileURL == "" {
		return nil, fmt.Errorf("%s: meta.ucp-agent.profile is required in MCP requests", negotiation.UCPAgentRequired)
	}

	negotiated, err := h.negotiator.Negotiate(ctx, profileURL)
	if err != nil {
		// Check for version error
		var verErr *negotiation.VersionError
		if errors.As(err, &verErr) {
			return nil, fmt.Errorf("%s: %s", verErr.Code, verErr.Message)
		}
		return nil, fmt.Errorf("negotiation_failed: %v", err)
	}

	// Log if using degraded mode (fetch failed but using fallback)
	if negotiated.FetchError != nil {
		h.logger.Warn("using fallback profile due to fetch error",
			"error_type", fmt.Sprintf("%T", negotiated.FetchError))
	}

	return negotiated, nil
}
