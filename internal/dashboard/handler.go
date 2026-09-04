package dashboard

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/likhith2366/paylo/internal/httpx"
)

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

func (h *Handler) Routes(r chi.Router) {
	r.Get("/v1/dashboard/summary", h.summary)
	r.Get("/v1/dashboard/volume", h.volume)
	r.Get("/v1/dashboard/balances", h.balances)
	r.Get("/v1/dashboard/charges", h.charges)
	r.Get("/v1/dashboard/charges/{id}", h.charge)
	r.Get("/v1/dashboard/disputes", h.disputes)
	r.Get("/v1/dashboard/payouts", h.payouts)
	r.Get("/v1/dashboard/webhook_deliveries", h.deliveries)
}

// merchant resolves the caller, or writes the error response and reports false.
func (h *Handler) merchant(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, ok := httpx.MerchantID(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, httpx.TypeAuthentication,
			"missing_merchant", "Could not resolve a merchant for this request.")
		return uuid.Nil, false
	}
	return id, true
}

func params(r *http.Request) ListParams {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	return ListParams{
		Limit:  limit,
		Cursor: q.Get("cursor"),
		Status: q.Get("status"),
	}
}

// fail logs the detail and returns a generic message. A database error's text
// can reveal schema and query shape, which is not something to hand a caller.
func (h *Handler) fail(w http.ResponseWriter, r *http.Request, op string, err error) {
	slog.Error("dashboard: "+op, "error", err, "trace_id", httpx.TraceID(r.Context()))
	httpx.Fail(w, http.StatusInternalServerError, httpx.TypeAPI,
		"internal_error", "An unexpected error occurred.")
}

func (h *Handler) summary(w http.ResponseWriter, r *http.Request) {
	merchantID, ok := h.merchant(w, r)
	if !ok {
		return
	}
	days, _ := strconv.Atoi(r.URL.Query().Get("days"))

	result, err := h.svc.Summary(r.Context(), merchantID, days)
	if err != nil {
		h.fail(w, r, "summary", err)
		return
	}
	httpx.JSON(w, http.StatusOK, result)
}

func (h *Handler) volume(w http.ResponseWriter, r *http.Request) {
	merchantID, ok := h.merchant(w, r)
	if !ok {
		return
	}
	days, _ := strconv.Atoi(r.URL.Query().Get("days"))

	points, err := h.svc.Volume(r.Context(), merchantID, days)
	if err != nil {
		h.fail(w, r, "volume", err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"object": "list", "data": points})
}

func (h *Handler) balances(w http.ResponseWriter, r *http.Request) {
	merchantID, ok := h.merchant(w, r)
	if !ok {
		return
	}
	balances, err := h.svc.Balances(r.Context(), merchantID)
	if err != nil {
		h.fail(w, r, "balances", err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"object": "list", "data": balances})
}

func (h *Handler) charges(w http.ResponseWriter, r *http.Request) {
	merchantID, ok := h.merchant(w, r)
	if !ok {
		return
	}
	page, err := h.svc.ListCharges(r.Context(), merchantID, params(r))
	if errors.Is(err, ErrBadCursor) {
		httpx.FailParam(w, http.StatusBadRequest, httpx.TypeInvalidRequest,
			"invalid_cursor", "The pagination cursor is not valid.", "cursor")
		return
	}
	if err != nil {
		h.fail(w, r, "list charges", err)
		return
	}
	httpx.JSON(w, http.StatusOK, page)
}

func (h *Handler) charge(w http.ResponseWriter, r *http.Request) {
	merchantID, ok := h.merchant(w, r)
	if !ok {
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
		h.fail(w, r, "get charge", err)
		return
	}
	if charge == nil {
		// 404 rather than 403 for another merchant's charge — confirming it
		// exists but belongs to someone else leaks information.
		httpx.Fail(w, http.StatusNotFound, httpx.TypeInvalidRequest,
			"resource_missing", "No such charge.")
		return
	}
	httpx.JSON(w, http.StatusOK, charge)
}

func (h *Handler) disputes(w http.ResponseWriter, r *http.Request) {
	merchantID, ok := h.merchant(w, r)
	if !ok {
		return
	}
	page, err := h.svc.ListDisputes(r.Context(), merchantID, params(r))
	if err != nil {
		h.fail(w, r, "list disputes", err)
		return
	}
	httpx.JSON(w, http.StatusOK, page)
}

func (h *Handler) payouts(w http.ResponseWriter, r *http.Request) {
	merchantID, ok := h.merchant(w, r)
	if !ok {
		return
	}
	page, err := h.svc.ListPayouts(r.Context(), merchantID, params(r))
	if err != nil {
		h.fail(w, r, "list payouts", err)
		return
	}
	httpx.JSON(w, http.StatusOK, page)
}

func (h *Handler) deliveries(w http.ResponseWriter, r *http.Request) {
	merchantID, ok := h.merchant(w, r)
	if !ok {
		return
	}
	page, err := h.svc.ListWebhookDeliveries(r.Context(), merchantID, params(r))
	if err != nil {
		h.fail(w, r, "list webhook deliveries", err)
		return
	}
	httpx.JSON(w, http.StatusOK, page)
}
