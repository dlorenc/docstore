package ui

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"html/template"
	"net/http"
)

type csrfKeyType struct{}

var csrfKey csrfKeyType

const (
	csrfCookieName = "__Host-csrf"
	csrfFieldName  = "csrf_token"
)

// csrfTokenFromCtx retrieves the CSRF token stored in the request context by
// CSRFMiddleware. Returns an empty string if not present.
func csrfTokenFromCtx(ctx context.Context) string {
	v, _ := ctx.Value(csrfKey).(string)
	return v
}

// newCSRFToken generates a cryptographically random 32-byte hex token.
func newCSRFToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// csrfFieldHTML returns the hidden input HTML for the given token.
func csrfFieldHTML(token string) template.HTML {
	return template.HTML(`<input type="hidden" name="` + csrfFieldName + `" value="` + template.HTMLEscapeString(token) + `">`)
}

// CSRFMiddleware implements the double-submit cookie CSRF protection pattern.
//
//   - On every request: if the __Host-csrf cookie is absent, a new token is
//     generated and set as a cookie, then stored in the request context.
//   - On mutating requests (everything except GET and HEAD): the form field
//     csrf_token must match the cookie value; a mismatch returns 403.
//
// The secure flag on the cookie is controlled by the secure parameter; pass
// false in dev mode (HTTP) and true in production (HTTPS).
func CSRFMiddleware(secure bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Retrieve or create the token.
		token := ""
		if c, err := r.Cookie(csrfCookieName); err == nil {
			token = c.Value
		}
		if token == "" {
			var err error
			token, err = newCSRFToken()
			if err != nil {
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}
			http.SetCookie(w, &http.Cookie{
				Name:     csrfCookieName,
				Value:    token,
				Path:     "/",
				SameSite: http.SameSiteStrictMode,
				Secure:   secure,
				HttpOnly: true,
			})
		}

		// Store token in context for downstream handlers / template rendering.
		ctx := context.WithValue(r.Context(), csrfKey, token)
		r = r.WithContext(ctx)

		// Validate on mutating methods.
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			// Safe methods — no validation needed.
		default:
			if err := r.ParseForm(); err != nil {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			formToken := r.FormValue(csrfFieldName)
			if formToken == "" || formToken != token {
				http.Error(w, "invalid CSRF token", http.StatusForbidden)
				return
			}
		}

		next.ServeHTTP(w, r)
	})
}
