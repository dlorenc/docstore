package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/dlorenc/docstore/internal/db"
	"github.com/dlorenc/docstore/internal/model"
)

// ---------------------------------------------------------------------------
// Event subscription handlers (delegated auth: admin for global scope, creator for own subscriptions)
// ---------------------------------------------------------------------------

// handleCreateSubscription implements POST /subscriptions.
// Global admin may create subscriptions of any scope. Non-admin users may create
// repo-scoped subscriptions only if they have at least reader access to that repo.
func (s *server) handleCreateSubscription(w http.ResponseWriter, r *http.Request) {
	var req model.CreateSubscriptionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Backend == "" {
		writeError(w, http.StatusBadRequest, "backend is required")
		return
	}
	if req.Backend != "webhook" {
		writeError(w, http.StatusBadRequest, "only backend='webhook' is supported")
		return
	}
	if len(req.Config) == 0 {
		writeError(w, http.StatusBadRequest, "config is required")
		return
	}

	// Validate webhook config: url must be present and an http/https URL.
	var webhookConfig map[string]string
	if err := json.Unmarshal(req.Config, &webhookConfig); err != nil {
		writeError(w, http.StatusBadRequest, "config must be a JSON object")
		return
	}
	webhookURL, ok := webhookConfig["url"]
	if !ok || webhookURL == "" {
		writeError(w, http.StatusBadRequest, "config.url is required for webhook backend")
		return
	}
	parsedURL, err := url.ParseRequestURI(webhookURL)
	if err != nil || (parsedURL.Scheme != "http" && parsedURL.Scheme != "https") {
		writeError(w, http.StatusBadRequest, "config.url must be a valid http or https URL")
		return
	}
	if webhookConfig["secret"] == "" {
		slog.Warn("subscription created without webhook secret", "url", webhookURL)
	}

	identity := IdentityFromContext(r.Context())
	isAdmin := s.globalAdmin != "" && identity == s.globalAdmin
	if !isAdmin {
		// Non-admin may only create repo-scoped subscriptions for repos they can read.
		if req.Repo == nil || *req.Repo == "" {
			writeError(w, http.StatusForbidden, "forbidden: global admin required for global subscriptions")
			return
		}
		if !s.requireRepoReadAccess(w, r, *req.Repo) {
			return
		}
	}

	req.CreatedBy = identity

	sub, err := s.commitStore.CreateSubscription(r.Context(), req)
	if err != nil {
		slog.Error("internal error", "op", "create_subscription", "error", err)
		writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "internal server error")
		return
	}

	slog.Info("subscription created", "id", sub.ID, "backend", sub.Backend, "by", req.CreatedBy)
	writeJSON(w, http.StatusCreated, sub)
}

// handleListSubscriptions implements GET /subscriptions.
// Global admin sees all subscriptions. Non-admin users see only subscriptions
// they created (queried directly by created_by at the store level).
func (s *server) handleListSubscriptions(w http.ResponseWriter, r *http.Request) {
	identity := IdentityFromContext(r.Context())
	isAdmin := s.globalAdmin != "" && identity == s.globalAdmin

	var subs []model.EventSubscription
	var err error
	if isAdmin {
		subs, err = s.commitStore.ListSubscriptions(r.Context())
	} else {
		subs, err = s.commitStore.ListSubscriptionsByCreator(r.Context(), identity)
	}
	if err != nil {
		slog.Error("internal error", "op", "list_subscriptions", "error", err)
		writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "internal server error")
		return
	}
	if subs == nil {
		subs = []model.EventSubscription{}
	}
	writeJSON(w, http.StatusOK, model.ListSubscriptionsResponse{Subscriptions: subs})
}

// handleDeleteSubscription implements DELETE /subscriptions/{id}.
// Global admin may delete any subscription. The subscription creator may also delete it.
func (s *server) handleDeleteSubscription(w http.ResponseWriter, r *http.Request) {
	identity := IdentityFromContext(r.Context())
	isAdmin := s.globalAdmin != "" && identity == s.globalAdmin

	id := r.PathValue("id")

	if !isAdmin {
		sub, err := s.commitStore.GetSubscription(r.Context(), id)
		if err != nil {
			switch {
			case errors.Is(err, db.ErrSubscriptionNotFound):
				writeAPIError(w, ErrCodeSubscriptionNotFound, http.StatusNotFound, "subscription not found")
			default:
				slog.Error("internal error", "op", "get_subscription", "id", id, "error", err)
				writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "internal server error")
			}
			return
		}
		if sub.CreatedBy != identity {
			writeAPIError(w, ErrCodeForbidden, http.StatusForbidden, "forbidden: not subscription creator")
			return
		}
	}

	if err := s.commitStore.DeleteSubscription(r.Context(), id); err != nil {
		switch {
		case errors.Is(err, db.ErrSubscriptionNotFound):
			writeAPIError(w, ErrCodeSubscriptionNotFound, http.StatusNotFound, "subscription not found")
		default:
			slog.Error("internal error", "op", "delete_subscription", "id", id, "error", err)
			writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	slog.Info("subscription deleted", "id", id, "by", identity)
	w.WriteHeader(http.StatusNoContent)
}

// handleResumeSubscription implements POST /subscriptions/{id}/resume.
// Global admin may resume any subscription. The subscription creator may also resume it.
func (s *server) handleResumeSubscription(w http.ResponseWriter, r *http.Request) {
	identity := IdentityFromContext(r.Context())
	isAdmin := s.globalAdmin != "" && identity == s.globalAdmin

	id := r.PathValue("id")

	if !isAdmin {
		sub, err := s.commitStore.GetSubscription(r.Context(), id)
		if err != nil {
			switch {
			case errors.Is(err, db.ErrSubscriptionNotFound):
				writeAPIError(w, ErrCodeSubscriptionNotFound, http.StatusNotFound, "subscription not found")
			default:
				slog.Error("internal error", "op", "get_subscription", "id", id, "error", err)
				writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "internal server error")
			}
			return
		}
		if sub.CreatedBy != identity {
			writeAPIError(w, ErrCodeForbidden, http.StatusForbidden, "forbidden: not subscription creator")
			return
		}
	}

	if err := s.commitStore.ResumeSubscription(r.Context(), id); err != nil {
		switch {
		case errors.Is(err, db.ErrSubscriptionNotFound):
			writeAPIError(w, ErrCodeSubscriptionNotFound, http.StatusNotFound, "subscription not found")
		default:
			slog.Error("internal error", "op", "resume_subscription", "id", id, "error", err)
			writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "internal server error")
		}
		return
	}

	slog.Info("subscription resumed", "id", id, "by", identity)
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// SSE streaming handlers
// ---------------------------------------------------------------------------

// handleSSERepoEvents implements GET /repos/{name}/-/events
// Streams CloudEvents for a specific repo. Optional ?types= comma-separated
// full event type filter (e.g. "com.docstore.commit.created").
func (s *server) handleSSERepoEvents(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	if !s.validateRepo(w, r, repo) {
		return
	}
	s.streamSSE(w, r, repo)
}

// handleSSEGlobalEvents implements GET /events (admin only)
// Streams CloudEvents for all repos or a specific repo via ?repo= filter.
func (s *server) handleSSEGlobalEvents(w http.ResponseWriter, r *http.Request) {
	if !s.requireGlobalAdmin(w, r) {
		return
	}
	// Optional repo filter.
	repo := r.URL.Query().Get("repo")
	if repo == "" {
		repo = "*" // wildcard: receive events from all repos
	}
	s.streamSSE(w, r, repo)
}

// streamSSE is the shared SSE streaming implementation.
// repo is the repo to stream events for (or "*" for global admin stream).
// Clients may pass ?since_seq=N to replay events from a known position,
// enabling reconnect without missing events.
func (s *server) streamSSE(w http.ResponseWriter, r *http.Request, repo string) {
	if s.broker == nil {
		writeAPIError(w, ErrCodeServiceUnavailable, http.StatusServiceUnavailable, "event streaming not available")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "streaming not supported")
		return
	}

	// Parse optional type filter.
	var types []string
	if q := r.URL.Query().Get("types"); q != "" {
		for _, t := range strings.Split(q, ",") {
			t = strings.TrimSpace(t)
			if t != "" {
				types = append(types, t)
			}
		}
	}

	ctx := r.Context()

	// Determine starting sequence. If the client supplies ?since_seq, replay
	// from that position. Otherwise start from the current tail so the client
	// only sees new events going forward.
	var sinceSeq int64
	if raw := r.URL.Query().Get("since_seq"); raw != "" {
		sinceSeq, _ = strconv.ParseInt(raw, 10, 64)
	} else {
		seq, err := s.broker.CurrentSeq(ctx)
		if err != nil {
			slog.Error("SSE: CurrentSeq failed", "error", err)
			writeAPIError(w, ErrCodeServiceUnavailable, http.StatusServiceUnavailable, "event streaming temporarily unavailable")
			return
		}
		sinceSeq = seq
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	for {
		evs, err := s.broker.Poll(ctx, repo, sinceSeq, types)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Error("SSE: poll failed", "error", err)
			time.Sleep(2 * time.Second)
		}
		for _, ev := range evs {
			fmt.Fprintf(w, "id: %d\ndata: %s\n\n", ev.Seq, ev.Payload)
			flusher.Flush()
			sinceSeq = ev.Seq
		}

		// Wait for the next notification (or keepalive timeout).
		waitCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		waitErr := s.broker.WaitForEvent(waitCtx)
		cancel()

		if ctx.Err() != nil {
			return
		}
		if errors.Is(waitErr, context.DeadlineExceeded) {
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}
