package vault

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/likhith2366/paylo/internal/httpx"
)

// maxBodyBytes is small on purpose. A tokenize request is a card number and two
// integers; anything larger is malformed or hostile.
const maxBodyBytes = 8 << 10

type Handler struct {
	svc *Service
	// internalSecret gates /vault/detokenize. It is defence in depth, not the
	// primary control — the real protection is that the endpoint is unreachable
	// from outside the vault's network segment (§2.4). A shared secret alone
	// would not be sufficient for this endpoint.
	internalSecret string
}

func NewHandler(svc *Service, internalSecret string) *Handler {
	return &Handler{svc: svc, internalSecret: internalSecret}
}

func (h *Handler) Routes(r chi.Router) {
	// Public: called by the hosted checkout iframe, straight from the browser.
	r.Post("/vault/tokenize", h.tokenize)
	// Public: safe by construction — cannot return a card number.
	r.Get("/vault/tokens/{token}", h.metadata)
	// Internal only.
	r.Post("/vault/detokenize", h.requireInternal(h.detokenize))
}

type tokenizeRequest struct {
	Number     string `json:"number"`
	ExpMonth   int    `json:"exp_month"`
	ExpYear    int    `json:"exp_year"`
	MerchantID string `json:"merchant_id,omitempty"`
	SingleUse  *bool  `json:"single_use,omitempty"`
}

func (h *Handler) tokenize(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		httpx.Fail(w, http.StatusRequestEntityTooLarge, httpx.TypeInvalidRequest,
			"request_too_large", "Request body is too large.")
		return
	}

	var req tokenizeRequest
	if err := json.Unmarshal(body, &req); err != nil {
		// The body contains a PAN, so it must not appear in the error or the
		// log — hence a generic message and no echo of the input.
		httpx.Fail(w, http.StatusBadRequest, httpx.TypeInvalidRequest,
			"invalid_json", "Request body could not be parsed as JSON.")
		return
	}

	in := TokenizeInput{
		Number:    req.Number,
		ExpMonth:  req.ExpMonth,
		ExpYear:   req.ExpYear,
		SingleUse: true, // safe default: a checkout token is used once
	}
	if req.SingleUse != nil {
		in.SingleUse = *req.SingleUse
	}
	if req.MerchantID != "" {
		id, err := uuid.Parse(req.MerchantID)
		if err != nil {
			httpx.FailParam(w, http.StatusBadRequest, httpx.TypeInvalidRequest,
				"invalid_merchant_id", "merchant_id is not a valid UUID.", "merchant_id")
			return
		}
		in.MerchantID = &id
	}

	token, err := h.svc.Tokenize(r.Context(), in)
	switch {
	case errors.Is(err, ErrInvalidCard):
		httpx.FailParam(w, http.StatusBadRequest, httpx.TypeCard, "invalid_number",
			"The card number is not valid.", "number")
		return
	case errors.Is(err, ErrCardExpired):
		httpx.FailParam(w, http.StatusBadRequest, httpx.TypeCard, "expired_card",
			"The card has expired.", "exp_year")
		return
	case err != nil:
		// Log the error but never the request body.
		slog.Error("vault: tokenize failed", "error", err, "trace_id", httpx.TraceID(r.Context()))
		httpx.Fail(w, http.StatusInternalServerError, httpx.TypeAPI, "internal_error",
			"An unexpected error occurred.")
		return
	}

	httpx.JSON(w, http.StatusCreated, token)
}

func (h *Handler) metadata(w http.ResponseWriter, r *http.Request) {
	token, err := h.svc.Metadata(r.Context(), chi.URLParam(r, "token"))
	switch {
	case errors.Is(err, ErrTokenNotFound), errors.Is(err, ErrTokenConsumed):
		// Consumed and missing are reported identically so the endpoint can't
		// be used to probe which tokens have been used.
		httpx.Fail(w, http.StatusNotFound, httpx.TypeInvalidRequest,
			"resource_missing", "No such token.")
		return
	case err != nil:
		slog.Error("vault: metadata lookup failed", "error", err)
		httpx.Fail(w, http.StatusInternalServerError, httpx.TypeAPI, "internal_error",
			"An unexpected error occurred.")
		return
	}
	httpx.JSON(w, http.StatusOK, token)
}

type detokenizeRequest struct {
	Token  string `json:"token"`
	Caller string `json:"caller"`
	Reason string `json:"reason"`
}

func (h *Handler) detokenize(w http.ResponseWriter, r *http.Request) {
	var req detokenizeRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&req); err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.TypeInvalidRequest,
			"invalid_json", "Request body could not be parsed as JSON.")
		return
	}
	if req.Caller == "" || req.Reason == "" {
		// Required so the audit log is actually useful. An access record that
		// doesn't say who asked or why is not an audit trail.
		httpx.Fail(w, http.StatusBadRequest, httpx.TypeInvalidRequest,
			"missing_audit_fields", "caller and reason are required.")
		return
	}

	pan, err := h.svc.Detokenize(r.Context(), req.Token, req.Caller, req.Reason)
	switch {
	case errors.Is(err, ErrTokenNotFound):
		httpx.Fail(w, http.StatusNotFound, httpx.TypeInvalidRequest,
			"resource_missing", "No such token.")
		return
	case errors.Is(err, ErrTokenExpired):
		httpx.Fail(w, http.StatusGone, httpx.TypeInvalidRequest,
			"token_expired", "This token has expired.")
		return
	case errors.Is(err, ErrTokenConsumed):
		httpx.Fail(w, http.StatusConflict, httpx.TypeInvalidRequest,
			"token_consumed", "This single-use token has already been used.")
		return
	case err != nil:
		slog.Error("vault: detokenize failed", "error", err, "token", req.Token)
		httpx.Fail(w, http.StatusInternalServerError, httpx.TypeAPI, "internal_error",
			"An unexpected error occurred.")
		return
	}

	// The one response in this system that carries a PAN. Marked no-store so it
	// cannot be cached by any intermediary.
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, http.StatusOK, map[string]string{"number": pan})
}

// requireInternal gates the detokenize endpoint on a shared secret.
func (h *Handler) requireInternal(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		presented := r.Header.Get("X-Vault-Internal-Secret")
		// Constant-time compare: a byte-by-byte comparison leaks the secret's
		// prefix through timing, one character at a time.
		if subtle.ConstantTimeCompare([]byte(presented), []byte(h.internalSecret)) != 1 {
			slog.Warn("vault: rejected unauthorized detokenize attempt",
				"remote_addr", r.RemoteAddr, "trace_id", httpx.TraceID(r.Context()))
			httpx.Fail(w, http.StatusForbidden, httpx.TypeAuthentication,
				"forbidden", "This endpoint is not publicly accessible.")
			return
		}
		next(w, r)
	}
}
