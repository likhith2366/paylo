package payments

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/likhith2366/paylo/internal/httpx"
	"github.com/likhith2366/paylo/internal/idempotency"
)

type createRefundRequest struct {
	Charge string `json:"charge"`
	// Omitted or zero means refund whatever remains on the charge.
	Amount   int64          `json:"amount"`
	Reason   string         `json:"reason"`
	Metadata map[string]any `json:"metadata"`
}

func (h *Handler) createRefund(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	merchantID, ok := httpx.MerchantID(ctx)
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, httpx.TypeAuthentication,
			"missing_merchant", "Could not resolve a merchant for this request.")
		return
	}

	// Required here for the same reason as on charges: without a key, a retried
	// refund refunds twice (§17).
	idemKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if idemKey == "" {
		httpx.Fail(w, http.StatusBadRequest, httpx.TypeIdempotency, "missing_idempotency_key",
			"An Idempotency-Key header is required for this endpoint.")
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		httpx.Fail(w, http.StatusRequestEntityTooLarge, httpx.TypeInvalidRequest,
			"request_too_large", "Request body exceeds the 1 MiB limit.")
		return
	}

	var req createRefundRequest
	if err := json.Unmarshal(body, &req); err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.TypeInvalidRequest,
			"invalid_json", "Request body could not be parsed as JSON.")
		return
	}

	chargeID, err := uuid.Parse(req.Charge)
	if err != nil {
		httpx.FailParam(w, http.StatusBadRequest, httpx.TypeInvalidRequest,
			"invalid_charge", "A valid charge ID is required.", "charge")
		return
	}
	if req.Amount < 0 {
		httpx.FailParam(w, http.StatusBadRequest, httpx.TypeInvalidRequest,
			"invalid_amount", "Amount must be a positive integer, or omitted for a full refund.", "amount")
		return
	}

	requestHash, err := idempotency.HashRequest(body)
	if err != nil {
		slog.Error("payments: hash refund request", "error", err, "trace_id", httpx.TraceID(ctx))
		httpx.Fail(w, http.StatusInternalServerError, httpx.TypeAPI, "internal_error",
			"An unexpected error occurred.")
		return
	}

	refund, status, err := h.svc.CreateRefund(ctx, RefundInput{
		MerchantID:      merchantID,
		ChargeID:        chargeID,
		AmountCents:     req.Amount,
		Reason:          req.Reason,
		Metadata:        req.Metadata,
		SimulateOutcome: simulateOutcome(ctx, r),
	}, idemKey, requestHash)

	switch {
	case errors.Is(err, idempotency.ErrInFlight):
		httpx.Fail(w, http.StatusConflict, httpx.TypeIdempotency, "request_in_flight",
			"A request with this Idempotency-Key is currently in progress.")
	case errors.Is(err, idempotency.ErrKeyReused):
		httpx.Fail(w, http.StatusUnprocessableEntity, httpx.TypeIdempotency, "idempotency_key_reused",
			"This Idempotency-Key was already used with a different request body.")
	case errors.Is(err, ErrChargeNotFound):
		httpx.Fail(w, http.StatusNotFound, httpx.TypeInvalidRequest,
			"resource_missing", "No such charge.")
	case errors.Is(err, ErrChargeNotRefundable):
		httpx.FailParam(w, http.StatusUnprocessableEntity, httpx.TypeInvalidRequest,
			"charge_not_refundable", "Only a succeeded charge can be refunded.", "charge")
	case errors.Is(err, ErrRefundExceedsCharge):
		httpx.FailParam(w, http.StatusUnprocessableEntity, httpx.TypeInvalidRequest,
			"refund_exceeds_charge",
			"The refund would exceed the remaining refundable amount on this charge.", "amount")
	case err != nil:
		slog.Error("payments: create refund failed", "error", err,
			"merchant_id", merchantID, "trace_id", httpx.TraceID(ctx))
		httpx.Fail(w, http.StatusInternalServerError, httpx.TypeAPI, "internal_error",
			"An unexpected error occurred.")
	default:
		httpx.JSON(w, status, refund)
	}
}

func (h *Handler) getDispute(w http.ResponseWriter, r *http.Request) {
	merchantID, ok := httpx.MerchantID(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, httpx.TypeAuthentication,
			"missing_merchant", "Could not resolve a merchant for this request.")
		return
	}

	disputeID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.TypeInvalidRequest,
			"invalid_id", "The dispute ID is not a valid UUID.")
		return
	}

	dispute, err := h.svc.GetDispute(r.Context(), merchantID, disputeID)
	if errors.Is(err, ErrDisputeNotFound) {
		httpx.Fail(w, http.StatusNotFound, httpx.TypeInvalidRequest,
			"resource_missing", "No such dispute.")
		return
	}
	if err != nil {
		slog.Error("payments: get dispute failed", "error", err, "trace_id", httpx.TraceID(r.Context()))
		httpx.Fail(w, http.StatusInternalServerError, httpx.TypeAPI, "internal_error",
			"An unexpected error occurred.")
		return
	}
	httpx.JSON(w, http.StatusOK, dispute)
}

func (h *Handler) submitDisputeEvidence(w http.ResponseWriter, r *http.Request) {
	merchantID, ok := httpx.MerchantID(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, httpx.TypeAuthentication,
			"missing_merchant", "Could not resolve a merchant for this request.")
		return
	}

	disputeID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.TypeInvalidRequest,
			"invalid_id", "The dispute ID is not a valid UUID.")
		return
	}

	var evidence map[string]any
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&evidence); err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.TypeInvalidRequest,
			"invalid_json", "Request body could not be parsed as JSON.")
		return
	}

	err = h.svc.SubmitEvidence(r.Context(), merchantID, disputeID, evidence)
	switch {
	case errors.Is(err, ErrDisputeNotOpen):
		httpx.Fail(w, http.StatusUnprocessableEntity, httpx.TypeInvalidRequest,
			"dispute_not_open",
			"This dispute is not accepting evidence — it may already be resolved.")
	case err != nil:
		slog.Error("payments: submit evidence failed", "error", err,
			"trace_id", httpx.TraceID(r.Context()))
		httpx.Fail(w, http.StatusInternalServerError, httpx.TypeAPI, "internal_error",
			"An unexpected error occurred.")
	default:
		dispute, err := h.svc.GetDispute(r.Context(), merchantID, disputeID)
		if err != nil {
			httpx.JSON(w, http.StatusOK, map[string]string{"status": "under_review"})
			return
		}
		httpx.JSON(w, http.StatusOK, dispute)
	}
}

// simulateOutcome returns the test-mode outcome override, honoured only for
// test-mode keys so a live merchant cannot steer the processor from a header.
func simulateOutcome(ctx context.Context, r *http.Request) string {
	if httpx.Mode(ctx) == "test" {
		return r.Header.Get("X-Simulate-Outcome")
	}
	return ""
}
