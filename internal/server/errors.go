package server

import (
	"crypto/sha256"
	"fmt"
	"net/http"
)

// ErrorCode is a machine-readable code included in every API error response.
// Clients can switch on Code to handle specific error types programmatically
// without parsing the human-readable message string.
type ErrorCode string

const (
	// Not-found errors (HTTP 404)
	ErrCodeNotFound             ErrorCode = "NOT_FOUND"
	ErrCodeRepoNotFound         ErrorCode = "REPO_NOT_FOUND"
	ErrCodeBranchNotFound       ErrorCode = "BRANCH_NOT_FOUND"
	ErrCodeOrgNotFound          ErrorCode = "ORG_NOT_FOUND"
	ErrCodeInviteNotFound       ErrorCode = "INVITE_NOT_FOUND"
	ErrCodeReleaseNotFound      ErrorCode = "RELEASE_NOT_FOUND"
	ErrCodeProposalNotFound     ErrorCode = "PROPOSAL_NOT_FOUND"
	ErrCodeIssueNotFound        ErrorCode = "ISSUE_NOT_FOUND"
	ErrCodeCommentNotFound      ErrorCode = "COMMENT_NOT_FOUND"
	ErrCodeRoleNotFound         ErrorCode = "ROLE_NOT_FOUND"
	ErrCodeSubscriptionNotFound ErrorCode = "SUBSCRIPTION_NOT_FOUND"

	// Conflict errors (HTTP 409)
	ErrCodeConflict        ErrorCode = "CONFLICT"
	ErrCodeBranchExists    ErrorCode = "BRANCH_EXISTS"
	ErrCodeRepoExists      ErrorCode = "REPO_EXISTS"
	ErrCodeOrgExists       ErrorCode = "ORG_EXISTS"
	ErrCodeBranchNotActive ErrorCode = "BRANCH_NOT_ACTIVE"
	ErrCodeBranchDraft     ErrorCode = "BRANCH_DRAFT"
	ErrCodeProposalExists  ErrorCode = "PROPOSAL_EXISTS"

	// Auth errors
	ErrCodeUnauthorized ErrorCode = "UNAUTHORIZED"
	ErrCodeForbidden    ErrorCode = "FORBIDDEN"

	// Precondition errors (HTTP 412)
	ErrCodePreconditionFailed ErrorCode = "PRECONDITION_FAILED"

	// Client errors
	ErrCodeBadRequest       ErrorCode = "BAD_REQUEST"
	ErrCodeMethodNotAllowed ErrorCode = "METHOD_NOT_ALLOWED"
	ErrCodeGone             ErrorCode = "GONE" // e.g. expired invite

	// Server errors
	ErrCodeInternalError      ErrorCode = "INTERNAL_ERROR"
	ErrCodeNotImplemented     ErrorCode = "NOT_IMPLEMENTED"
	ErrCodeServiceUnavailable ErrorCode = "SERVICE_UNAVAILABLE"
)

// APIError is the structured error type returned by all HTTP handlers.
// The JSON key for Message is "error" to preserve backward compatibility
// with clients that already parse {"error": "..."} responses; the new
// "code" field is additive.
type APIError struct {
	Code    ErrorCode `json:"code"`
	Message string    `json:"error"`
	Status  int       `json:"-"`
}

// write serialises e as JSON and writes the HTTP status stored in e.Status.
func (e APIError) write(w http.ResponseWriter) {
	writeJSON(w, e.Status, e)
}

// writeAPIError writes a structured error response with an explicit code.
// Prefer this over writeError when a domain-specific code is available.
func writeAPIError(w http.ResponseWriter, code ErrorCode, status int, msg string) {
	APIError{Code: code, Message: msg, Status: status}.write(w)
}

// statusToCode maps an HTTP status to a generic ErrorCode.
// Use writeAPIError with a specific code for domain-specific errors.
func statusToCode(status int) ErrorCode {
	switch status {
	case http.StatusNotFound:
		return ErrCodeNotFound
	case http.StatusConflict:
		return ErrCodeConflict
	case http.StatusForbidden:
		return ErrCodeForbidden
	case http.StatusUnauthorized:
		return ErrCodeUnauthorized
	case http.StatusBadRequest:
		return ErrCodeBadRequest
	case http.StatusMethodNotAllowed:
		return ErrCodeMethodNotAllowed
	case http.StatusGone:
		return ErrCodeGone
	case http.StatusNotImplemented:
		return ErrCodeNotImplemented
	case http.StatusPreconditionFailed:
		return ErrCodePreconditionFailed
	case http.StatusServiceUnavailable:
		return ErrCodeServiceUnavailable
	default:
		return ErrCodeInternalError
	}
}

// computeETag returns a quoted ETag string by SHA-256-hashing the given parts
// joined with colons. The result is hex-encoded and wrapped in double quotes
// per the HTTP ETag format (RFC 9110).
func computeETag(parts ...string) string {
	h := sha256.New()
	for i, p := range parts {
		if i > 0 {
			h.Write([]byte(":"))
		}
		h.Write([]byte(p))
	}
	return fmt.Sprintf(`"%x"`, h.Sum(nil))
}
