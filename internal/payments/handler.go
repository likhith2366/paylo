package payments

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/likhith2366/paylo/internal/httpx"
	"github.com/likhith2366/paylo/internal/idempotency"
)

// maxBodyBytes caps request size. Without it, a single large POST can pin a
// pod's memory — and the body is fully read here to compute the idempotency
// hash, so it cannot be streamed past.
const maxBodyBytes = 1 << 20 // 1 MiB

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

func (h *Handler) Routes(r chi.Router) {
	r.Post("/v1/charges", h.createCharge)
	r.Get("/v1/charges/{id}", h.getCharge)
}

type createChargeRequest struct {
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
	// A vault token, never a card number. This endpoint has no field capable of
	// carrying a PAN, which is what keeps the Payments API out of PCI scope
	// (§2.4) — a merchant literally cannot send us one by mistake.
	PaymentToken string         `json:"payment_token"`
	Description  string         `json:"description"`
	Metadata     map[string]any `json:"metadata"`
}

func (h *Handler) createCharge(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	merchantID, ok := httpx.MerchantID(ctx)
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, httpx.TypeAuthentication,
			"missing_merchant", "Could not resolve a merchant for this request.")
		return
	}

	// Required, not optional. Stripe allows omitting it; we do not. A payments
	// API whose exactly-once guarantee is opt-in does not really have one, and
	// making it mandatory means no merchant can accidentally double-charge a
	// customer by forgetting a header.
	idemKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if idemKey == "" {
		httpx.Fail(w, http.StatusBadRequest, httpx.TypeIdempotency, "missing_idempotency_key",
			"An Idempotency-Key header is required for this endpoint.")
		return
	}
	if len(idemKey) > 255 {
		httpx.Fail(w, http.StatusBadRequest, httpx.TypeIdempotency, "idempotency_key_too_long",
			"Idempotency-Key must be 255 characters or fewer.")
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		httpx.Fail(w, http.StatusRequestEntityTooLarge, httpx.TypeInvalidRequest,
			"request_too_large", "Request body exceeds the 1 MiB limit.")
		return
	}

	var req createChargeRequest
	if err := json.Unmarshal(body, &req); err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.TypeInvalidRequest,
			"invalid_json", "Request body could not be parsed as JSON.")
		return
	}

	if req.Amount <= 0 {
		httpx.FailParam(w, http.StatusBadRequest, httpx.TypeInvalidRequest,
			"invalid_amount", "Amount must be a positive integer in the currency's minor units.", "amount")
		return
	}
	if len(req.Currency) != 3 {
		httpx.FailParam(w, http.StatusBadRequest, httpx.TypeInvalidRequest,
			"invalid_currency", "Currency must be a 3-letter ISO 4217 code.", "currency")
		return
	}
	if req.PaymentToken == "" {
		httpx.FailParam(w, http.StatusBadRequest, httpx.TypeInvalidRequest,
			"missing_payment_token",
			"A payment_token is required. Collect card details with the hosted "+
				"checkout widget, which exchanges them for a token.", "payment_token")
		return
	}
	// A merchant sending a raw PAN would drag their integration — and us — into
	// full PCI scope. Reject it loudly and tell them what to do instead, rather
	// than quietly ignoring the field.
	if looksLikeCardNumber(body) {
		httpx.Fail(w, http.StatusBadRequest, httpx.TypeInvalidRequest,
			"raw_card_data_rejected",
			"This endpoint does not accept raw card details. Use the hosted "+
				"checkout widget to obtain a payment_token.")
		return
	}

	requestHash, err := idempotency.HashRequest(body)
	if err != nil {
		slog.Error("payments: hash request", "error", err, "trace_id", httpx.TraceID(ctx))
		httpx.Fail(w, http.StatusInternalServerError, httpx.TypeAPI, "internal_error",
			"An unexpected error occurred.")
		return
	}

	in := ChargeInput{
		MerchantID:        merchantID,
		AmountCents:       req.Amount,
		Currency:          strings.ToUpper(req.Currency),
		PaymentToken:      req.PaymentToken,
		Description:       req.Description,
		Metadata:          req.Metadata,
		DeviceFingerprint: r.Header.Get("X-Device-Fingerprint"),
		IPAddress:         clientIP(r),
	}
	// Only honoured for test-mode keys, so a live merchant can never steer the
	// processor's behaviour from a header.
	if httpx.Mode(ctx) == "test" {
		in.SimulateOutcome = r.Header.Get("X-Simulate-Outcome")
	}

	charge, status, err := h.svc.CreateCharge(ctx, in, idemKey, requestHash, body)
	switch {
	case errors.Is(err, idempotency.ErrInFlight):
		// A concurrent request with this key is still running. 409 tells the
		// client to wait rather than to retry immediately.
		httpx.Fail(w, http.StatusConflict, httpx.TypeIdempotency, "request_in_flight",
			"A request with this Idempotency-Key is currently in progress.")
		return

	case errors.Is(err, idempotency.ErrKeyReused):
		httpx.Fail(w, http.StatusUnprocessableEntity, httpx.TypeIdempotency, "idempotency_key_reused",
			"This Idempotency-Key was already used with a different request body.")
		return

	case errors.Is(err, ErrMissingToken):
		httpx.FailParam(w, http.StatusBadRequest, httpx.TypeInvalidRequest,
			"missing_payment_token", "A payment_token is required.", "payment_token")
		return

	case errors.Is(err, ErrTokenUnusable):
		// Covers not-found, expired, and already-consumed alike. They are
		// reported identically so the endpoint cannot be used to probe which
		// tokens exist or have been spent.
		httpx.FailParam(w, http.StatusBadRequest, httpx.TypeCard, "invalid_payment_token",
			"The payment token is invalid, expired, or has already been used.",
			"payment_token")
		return

	case errors.Is(err, ErrInvalidAmount):
		httpx.FailParam(w, http.StatusBadRequest, httpx.TypeInvalidRequest,
			"invalid_amount", err.Error(), "amount")
		return

	case err != nil:
		slog.Error("payments: create charge failed", "error", err,
			"merchant_id", merchantID, "trace_id", httpx.TraceID(ctx))
		httpx.Fail(w, http.StatusInternalServerError, httpx.TypeAPI, "internal_error",
			"An unexpected error occurred.")
		return
	}

	httpx.JSON(w, status, charge)
}

func (h *Handler) getCharge(w http.ResponseWriter, r *http.Request) {
	merchantID, ok := httpx.MerchantID(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, httpx.TypeAuthentication,
			"missing_merchant", "Could not resolve a merchant for this request.")
		return
	}

	chargeID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.TypeInvalidRequest,
			"invalid_id", "The charge ID is not a valid UUID.")
		return
	}

	charge, err := h.svc.GetCharge(r.Context(), merchantID, chargeID)
	if err != nil {
		slog.Error("payments: get charge failed", "error", err, "trace_id", httpx.TraceID(r.Context()))
		httpx.Fail(w, http.StatusInternalServerError, httpx.TypeAPI, "internal_error",
			"An unexpected error occurred.")
		return
	}
	if charge == nil {
		// 404 rather than 403 for another merchant's charge — confirming a
		// charge exists but belongs to someone else leaks information.
		httpx.Fail(w, http.StatusNotFound, httpx.TypeInvalidRequest,
			"resource_missing", "No such charge.")
		return
	}
	httpx.JSON(w, http.StatusOK, charge)
}

// looksLikeCardNumber reports whether a request body appears to contain a PAN.
//
// A guard, not a security control — a determined caller can evade it. Its job
// is to catch the honest mistake of a merchant posting card details directly,
// and to fail that request before the data reaches a log or the database.
//
// Deliberately checks for a Luhn-valid run of digits rather than just the
// presence of a "number" field, so an order ID or a phone number in metadata
// doesn't trip it.
func looksLikeCardNumber(body []byte) bool {
	var run []byte
	for i := 0; i <= len(body); i++ {
		if i < len(body) && body[i] >= '0' && body[i] <= '9' {
			run = append(run, body[i])
			continue
		}
		// Separators inside a formatted card number shouldn't break the run.
		if i < len(body) && (body[i] == ' ' || body[i] == '-') && len(run) > 0 {
			continue
		}
		if len(run) >= 13 && len(run) <= 19 && Luhn(string(run)) {
			return true
		}
		run = run[:0]
	}
	return false
}

// clientIP prefers the left-most X-Forwarded-For entry, which is the original
// client when the ALB appends its own hops.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if first, _, found := strings.Cut(xff, ","); found {
			return strings.TrimSpace(first)
		}
		return strings.TrimSpace(xff)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
