// Package httpx holds shared HTTP plumbing: error shapes, JSON helpers, and
// middleware used across services.
package httpx

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// ErrorBody mirrors Stripe's error envelope. Merchants integrate against this
// shape, so it is a frozen contract from the first release (§22.2).
type ErrorBody struct {
	Error ErrorDetail `json:"error"`
}

type ErrorDetail struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Param   string `json:"param,omitempty"`
}

// Error types, mirroring Stripe's taxonomy.
const (
	TypeInvalidRequest = "invalid_request_error"
	TypeAuthentication = "authentication_error"
	TypeIdempotency    = "idempotency_error"
	TypeRateLimit      = "rate_limit_error"
	TypeCard           = "card_error"
	TypeAPI            = "api_error"
)

func JSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if body != nil {
		if err := json.NewEncoder(w).Encode(body); err != nil {
			slog.Error("httpx: encode response", "error", err)
		}
	}
}

// Raw writes an already-serialized body. Used to replay a stored idempotent
// response byte-for-byte, which matters: a retry must be indistinguishable
// from the original response, not merely equivalent to it (§4.2).
func Raw(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Idempotent-Replay", "true")
	w.WriteHeader(status)
	if _, err := w.Write(body); err != nil {
		slog.Error("httpx: write raw response", "error", err)
	}
}

func Fail(w http.ResponseWriter, status int, errType, code, message string) {
	JSON(w, status, ErrorBody{Error: ErrorDetail{Type: errType, Code: code, Message: message}})
}

func FailParam(w http.ResponseWriter, status int, errType, code, message, param string) {
	JSON(w, status, ErrorBody{Error: ErrorDetail{
		Type: errType, Code: code, Message: message, Param: param,
	}})
}
