package docstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/dlorenc/docstore/api"
)

// Sentinel errors for common HTTP failure modes. Match with errors.Is; the
// underlying *Error still carries the full response context.
var (
	ErrUnauthorized = errors.New("docstore: unauthorized")
	ErrForbidden    = errors.New("docstore: forbidden")
	ErrNotFound     = errors.New("docstore: not found")
	ErrConflict     = errors.New("docstore: conflict")
)

// Error is the structured form of a non-2xx HTTP response from the server.
type Error struct {
	StatusCode int    // HTTP status code
	Message    string // server's ErrorResponse.Error field, if present
	Body       []byte // raw response body for diagnostics
}

func (e *Error) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("docstore: %s (status %d)", e.Message, e.StatusCode)
	}
	return fmt.Sprintf("docstore: HTTP %d", e.StatusCode)
}

// Is lets errors.Is(err, ErrNotFound) etc. match on status code.
func (e *Error) Is(target error) bool {
	switch target {
	case ErrUnauthorized:
		return e.StatusCode == http.StatusUnauthorized
	case ErrForbidden:
		return e.StatusCode == http.StatusForbidden
	case ErrNotFound:
		return e.StatusCode == http.StatusNotFound
	case ErrConflict:
		return e.StatusCode == http.StatusConflict
	}
	return false
}

// ConflictError wraps a 409 response from /merge or /rebase that carries a
// structured list of file conflicts. Unwrap with errors.As; the HTTP status
// and raw body remain accessible via the embedded Base.
type ConflictError struct {
	Base      *Error
	Conflicts []api.ConflictEntry
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("docstore: %d conflict(s) (status %d)", len(e.Conflicts), e.Base.StatusCode)
}

func (e *ConflictError) Unwrap() error { return e.Base }

// PolicyError wraps a 403 response from /merge that carries failing policy
// evaluation results. Unwrap with errors.As.
type PolicyError struct {
	Base     *Error
	Policies []api.PolicyResult
}

func (e *PolicyError) Error() string {
	return fmt.Sprintf("docstore: merge blocked by %d policy result(s) (status %d)", len(e.Policies), e.Base.StatusCode)
}

func (e *PolicyError) Unwrap() error { return e.Base }

// decodeError reads a non-2xx response body and returns the richest typed
// error it can reconstruct. The caller is responsible for closing the body.
func decodeError(resp *http.Response) error {
	body, _ := io.ReadAll(resp.Body)
	base := &Error{StatusCode: resp.StatusCode, Body: body}

	// First try policy-denial (503/403): wraps a non-empty Policies slice.
	if len(body) > 0 {
		var pe api.MergePolicyError
		if err := json.Unmarshal(body, &pe); err == nil && len(pe.Policies) > 0 {
			base.Message = "merge policy denial"
			return &PolicyError{Base: base, Policies: pe.Policies}
		}
		// Then conflicts (409 from merge/rebase).
		var ce api.MergeConflictError
		if err := json.Unmarshal(body, &ce); err == nil && len(ce.Conflicts) > 0 {
			base.Message = "merge conflict"
			return &ConflictError{Base: base, Conflicts: ce.Conflicts}
		}
		// Fallback: generic {"error": "..."}.
		var er api.ErrorResponse
		if err := json.Unmarshal(body, &er); err == nil && er.Error != "" {
			base.Message = er.Error
		}
	}
	return base
}
