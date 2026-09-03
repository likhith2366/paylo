package vault

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

// The checkout assets are embedded in the binary rather than read from disk, so
// the container has no writable asset directory an attacker could swap files
// into. A tampered iframe.html would be a direct card-harvesting vector, so
// making it immutable at build time is worth the small loss of flexibility.
//
//go:embed all:static
var checkoutAssets embed.FS

// CheckoutHandler serves the hosted card-input frame, card.js, and payflow.js.
// Mount it at /checkout.
//
// These must be served from the vault's own origin — that is what makes the
// same-origin policy the PCI boundary (§2.4). Serving them from the merchant's
// domain, or from the Payments API, would defeat the entire design.
func CheckoutHandler(allowedOrigins []string) (http.Handler, error) {
	sub, err := fs.Sub(checkoutAssets, "static")
	if err != nil {
		return nil, err
	}
	return checkoutHeaders(allowedOrigins, http.FileServer(http.FS(sub))), nil
}

// checkoutHeaders sets the security headers the iframe depends on.
func checkoutHeaders(allowedOrigins []string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()

		// This page is DESIGNED to be framed by merchants, so the usual
		// X-Frame-Options: DENY would break it. frame-ancestors names exactly
		// who may embed it — clickjacking protection without blocking the
		// legitimate use. In production this list comes from registered
		// merchant domains rather than configuration.
		h.Set("Content-Security-Policy", strings.Join([]string{
			"default-src 'none'",
			"script-src 'self'",
			"style-src 'unsafe-inline'",
			// The frame talks to the vault only. If a script were ever injected
			// here, this is what stops it exfiltrating a card number anywhere.
			"connect-src 'self'",
			"img-src 'self' data:",
			"frame-ancestors " + frameAncestors(allowedOrigins),
			"base-uri 'none'",
			"form-action 'none'",
		}, "; "))

		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		// The card frame must never be cached to disk on a shared machine.
		h.Set("Cache-Control", "no-store, max-age=0")

		next.ServeHTTP(w, r)
	})
}

func frameAncestors(origins []string) string {
	if len(origins) == 0 {
		// No registered merchants configured. Refusing to be framed at all is
		// the safe default — better a broken demo than an open frame.
		return "'none'"
	}
	return strings.Join(origins, " ")
}

// CORS allows the embedded frame to POST to /vault/tokenize.
//
// The frame is served from the vault's origin and calls back to it, so this is
// same-origin in the normal case. It exists for local development, where the
// frame may be served from a different port than the API.
func CORS(allowedOrigins []string) func(http.Handler) http.Handler {
	allowed := make(map[string]bool, len(allowedOrigins))
	for _, o := range allowedOrigins {
		allowed[o] = true
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")

			// Echo the specific origin rather than "*". A wildcard here would
			// let any site on the internet post cards to this endpoint.
			if origin != "" && allowed[origin] {
				h := w.Header()
				h.Set("Access-Control-Allow-Origin", origin)
				h.Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS")
				h.Set("Access-Control-Allow-Headers", "Content-Type")
				h.Set("Access-Control-Max-Age", "600")
				// Tells caches the response varies per origin, so one
				// merchant's CORS headers aren't served to another.
				h.Add("Vary", "Origin")
			}

			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
