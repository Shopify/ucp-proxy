package handler

import (
	"log/slog"
	"net/http"

	"ucp-proxy/internal/adapter"
	"ucp-proxy/internal/model"
)

// handleCreateCheckout creates a new checkout session.
// POST /checkout-sessions
func (h *Handler) handleCreateCheckout(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var req adapter.CreateCheckoutRequest
	if err := decodeJSON(r, &req); err != nil {
		h.writeCheckoutError(w, "", err) // No ID yet for creation errors
		return
	}

	h.logger.InfoContext(ctx, "creating checkout",
		slog.Int("line_items", len(req.LineItems)),
		slog.Bool("has_cart_token", req.CartToken != ""),
	)

	checkout, err := h.adapter.CreateCheckout(ctx, &req)
	if err != nil {
		h.writeCheckoutError(w, "", err)
		return
	}

	h.writeJSON(w, http.StatusCreated, checkout)
}

// handleGetCheckout retrieves an existing checkout.
// GET /checkout-sessions/{id}
func (h *Handler) handleGetCheckout(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	checkoutID := r.PathValue("id")

	if checkoutID == "" {
		h.writeCheckoutError(w, "", model.NewValidationError("id", "checkout ID required"))
		return
	}

	// Checkout IDs contain bearer tokens; log the operation, not the ID.
	h.logger.InfoContext(ctx, "getting checkout")

	checkout, err := h.adapter.GetCheckout(ctx, checkoutID)
	if err != nil {
		h.writeCheckoutError(w, checkoutID, err)
		return
	}

	h.writeJSON(w, http.StatusOK, checkout)
}

// handleUpdateCheckout modifies checkout details.
// PUT /checkout-sessions/{id}
func (h *Handler) handleUpdateCheckout(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	checkoutID := r.PathValue("id")

	if checkoutID == "" {
		h.writeCheckoutError(w, "", model.NewValidationError("id", "checkout ID required"))
		return
	}

	var req model.CheckoutUpdateRequest
	if err := decodeJSON(r, &req); err != nil {
		h.writeCheckoutError(w, checkoutID, err)
		return
	}

	h.logger.InfoContext(ctx, "updating checkout",
		slog.Int("line_items", len(req.LineItems)),
		slog.Int("discount_codes", len(req.DiscountCodes)),
		slog.Bool("has_shipping", req.ShippingAddress != nil),
		slog.Bool("has_billing", req.BillingAddress != nil),
		slog.Bool("has_shipping_option", req.FulfillmentOptionID != ""),
	)

	checkout, err := h.adapter.UpdateCheckout(ctx, checkoutID, &req)
	if err != nil {
		h.writeCheckoutError(w, checkoutID, err)
		return
	}

	h.writeJSON(w, http.StatusOK, checkout)
}

// handleCompleteCheckout submits payment and finalizes the checkout.
// POST /checkout-sessions/{id}/complete
func (h *Handler) handleCompleteCheckout(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	checkoutID := r.PathValue("id")

	if checkoutID == "" {
		h.writeCheckoutError(w, "", model.NewValidationError("id", "checkout ID required"))
		return
	}

	var req model.CheckoutSubmitRequest
	if err := decodeJSON(r, &req); err != nil {
		h.writeCheckoutError(w, checkoutID, err)
		return
	}

	// Validate payment submission: at least one instrument with selected=true
	if len(req.Payment.Instruments) == 0 {
		h.writeCheckoutError(w, checkoutID, model.NewValidationError("payment.instruments", "at least one instrument required"))
		return
	}
	selectedInstrument := req.Payment.SelectedInstrument()
	if selectedInstrument == nil {
		h.writeCheckoutError(w, checkoutID, model.NewValidationError("payment.instruments", "one instrument must have selected=true"))
		return
	}

	h.logger.InfoContext(ctx, "completing checkout",
		slog.Int("instruments_count", len(req.Payment.Instruments)),
	)

	checkout, err := h.adapter.CompleteCheckout(ctx, checkoutID, &req)
	if err != nil {
		h.writeCheckoutError(w, checkoutID, err)
		return
	}

	// Select HTTP status based on checkout state
	status := http.StatusOK
	if checkout.Status == model.StatusRequiresEscalation {
		status = http.StatusAccepted // 202 indicates further action needed
	}

	h.writeJSON(w, status, checkout)
}

// handleCancelCheckout cancels a checkout session.
// POST /checkout-sessions/{id}/cancel
func (h *Handler) handleCancelCheckout(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	checkoutID := r.PathValue("id")

	if checkoutID == "" {
		h.writeCheckoutError(w, "", model.NewValidationError("id", "checkout ID required"))
		return
	}

	h.logger.InfoContext(ctx, "canceling checkout")

	checkout, err := h.adapter.CancelCheckout(ctx, checkoutID)
	if err != nil {
		h.writeCheckoutError(w, checkoutID, err)
		return
	}

	h.writeJSON(w, http.StatusOK, checkout)
}
