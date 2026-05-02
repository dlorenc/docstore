package server

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/dlorenc/docstore/internal/model"
	"github.com/dlorenc/docstore/internal/store"
)

// ---------------------------------------------------------------------------
// File handlers
// ---------------------------------------------------------------------------

// handleTree implements GET /repos/:name/tree?branch=main&at=N&limit=N&after=cursor
func (s *server) handleTree(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	if !s.validateRepo(w, r, repo) {
		return
	}
	branch := r.URL.Query().Get("branch")
	if branch == "" {
		branch = "main"
	}

	var atSequence *int64
	if v := r.URL.Query().Get("at"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			// Try as release name.
			rel, relErr := s.commitStore.GetRelease(r.Context(), repo, v)
			if relErr != nil {
				writeError(w, http.StatusBadRequest, "invalid 'at' parameter: not a sequence number or release name")
				return
			}
			atSequence = &rel.Sequence
		} else {
			atSequence = &n
		}
	}

	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "invalid 'limit' parameter")
			return
		}
		limit = n
	}

	afterPath := r.URL.Query().Get("after")

	entries, err := s.readStore.MaterializeTree(r.Context(), repo, branch, atSequence, limit, afterPath)
	if err != nil {
		slog.Error("internal error", "op", "tree", "repo", repo, "branch", branch, "error", err)
		writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "query failed")
		return
	}
	if entries == nil {
		entries = []store.TreeEntry{}
	}
	writeJSON(w, http.StatusOK, entries)
}

// handleFile implements:
//   - GET /repos/:name/file/{path...}          → file content
//   - GET /repos/:name/file/{path...}/history  → file change history
//
// Query params: branch (default "main"), at (sequence), limit, after (cursor).
func (s *server) handleFile(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	if !s.validateRepo(w, r, repo) {
		return
	}
	fullPath := r.PathValue("path")

	// Check for /history suffix.
	if filePath, ok := strings.CutSuffix(fullPath, "/history"); ok {
		s.handleFileHistory(w, r, repo, filePath)
		return
	}

	s.handleFileContent(w, r, repo, fullPath)
}

func (s *server) handleFileContent(w http.ResponseWriter, r *http.Request, repo, path string) {
	branch := r.URL.Query().Get("branch")
	if branch == "" {
		branch = "main"
	}

	var atSequence *int64
	if v := r.URL.Query().Get("at"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			// Try as release name.
			rel, relErr := s.commitStore.GetRelease(r.Context(), repo, v)
			if relErr != nil {
				writeError(w, http.StatusBadRequest, "invalid 'at' parameter: not a sequence number or release name")
				return
			}
			atSequence = &rel.Sequence
		} else {
			atSequence = &n
		}
	}

	fc, err := s.readStore.GetFile(r.Context(), repo, branch, path, atSequence)
	if err != nil {
		slog.Error("internal error", "op", "file_content", "repo", repo, "branch", branch, "path", path, "error", err)
		writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "query failed")
		return
	}
	if fc == nil {
		writeAPIError(w, ErrCodeNotFound, http.StatusNotFound, "file not found")
		return
	}

	if r.URL.Query().Get("format") == "raw" {
		ct := fc.ContentType
		if ct == "" {
			ct = "application/octet-stream"
		}
		w.Header().Set("Content-Type", ct)
		w.WriteHeader(http.StatusOK)
		w.Write(fc.Content) //nolint:errcheck
		return
	}

	writeJSON(w, http.StatusOK, fc)
}

func (s *server) handleFileHistory(w http.ResponseWriter, r *http.Request, repo, path string) {
	branch := r.URL.Query().Get("branch")
	if branch == "" {
		branch = "main"
	}

	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "invalid 'limit' parameter")
			return
		}
		limit = n
	}

	var afterSeq *int64
	if v := r.URL.Query().Get("after"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid 'after' parameter")
			return
		}
		afterSeq = &n
	}

	entries, err := s.readStore.GetFileHistory(r.Context(), repo, branch, path, limit, afterSeq)
	if err != nil {
		slog.Error("internal error", "op", "file_history", "repo", repo, "branch", branch, "path", path, "error", err)
		writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "query failed")
		return
	}
	if entries == nil {
		entries = []store.FileHistoryEntry{}
	}
	writeJSON(w, http.StatusOK, entries)
}

// ---------------------------------------------------------------------------
// Commit handlers
// ---------------------------------------------------------------------------

// handleGetCommit implements GET /repos/:name/commit/{sequence}
func (s *server) handleGetCommit(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	seqStr := r.PathValue("sequence")
	seq, err := strconv.ParseInt(seqStr, 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid sequence number")
		return
	}

	detail, err := s.readStore.GetCommit(r.Context(), repo, seq)
	if err != nil {
		slog.Error("internal error", "op", "get_commit", "repo", repo, "seq", seq, "error", err)
		writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "query failed")
		return
	}
	if detail == nil {
		writeAPIError(w, ErrCodeNotFound, http.StatusNotFound, "commit not found")
		return
	}

	writeJSON(w, http.StatusOK, detail)
}

// handleDiff implements GET /repos/:name/diff?branch=X
func (s *server) handleDiff(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	if !s.validateRepo(w, r, repo) {
		return
	}
	branch := r.URL.Query().Get("branch")
	if branch == "" {
		writeError(w, http.StatusBadRequest, "branch parameter is required")
		return
	}

	result, err := s.readStore.GetDiff(r.Context(), repo, branch)
	if err != nil {
		slog.Error("internal error", "op", "diff", "repo", repo, "branch", branch, "error", err)
		writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "query failed")
		return
	}
	if result == nil {
		writeAPIError(w, ErrCodeBranchNotFound, http.StatusNotFound, "branch not found")
		return
	}

	// Convert to API response type.
	resp := model.DiffResponse{
		BranchChanges: make([]model.DiffEntry, len(result.BranchChanges)),
		MainChanges:   make([]model.DiffEntry, len(result.MainChanges)),
	}
	for i, e := range result.BranchChanges {
		resp.BranchChanges[i] = model.DiffEntry{Path: e.Path, VersionID: e.VersionID, Binary: e.Binary}
	}
	for i, e := range result.MainChanges {
		resp.MainChanges[i] = model.DiffEntry{Path: e.Path, VersionID: e.VersionID, Binary: e.Binary}
	}
	for _, c := range result.Conflicts {
		resp.Conflicts = append(resp.Conflicts, model.ConflictEntry{
			Path:            c.Path,
			MainVersionID:   c.MainVersionID,
			BranchVersionID: c.BranchVersionID,
		})
	}

	writeJSON(w, http.StatusOK, resp)
}

// writeArchive streams all files for the given repo/branch as a tar archive to w.
// If atSequence is 0, the latest sequence is used.
func writeArchive(ctx context.Context, rs readStore, w io.Writer, repo, branch string, atSequence int64) error {
	var seqPtr *int64
	if atSequence != 0 {
		seqPtr = &atSequence
	}
	tw := tar.NewWriter(w)
	defer tw.Close()
	afterPath := ""
	for {
		entries, err := rs.MaterializeTree(ctx, repo, branch, seqPtr, 100, afterPath)
		if err != nil {
			return fmt.Errorf("materialize tree: %w", err)
		}
		for _, entry := range entries {
			fc, err := rs.GetFile(ctx, repo, branch, entry.Path, seqPtr)
			if err != nil || fc == nil {
				slog.Error("archive: get file error", "repo", repo, "branch", branch, "path", entry.Path, "error", err)
				continue
			}
			hdr := &tar.Header{
				Name:     entry.Path,
				Size:     int64(len(fc.Content)),
				Mode:     0644,
				Typeflag: tar.TypeReg,
			}
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			if _, err := tw.Write(fc.Content); err != nil {
				return err
			}
		}
		if len(entries) < 100 {
			break
		}
		afterPath = entries[len(entries)-1].Path
	}
	return nil
}

// handleArchive implements GET /repos/:name/-/archive?branch=X
// Streams all files for the given branch as a tar archive.
func (s *server) handleArchive(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	if !s.validateRepo(w, r, repo) {
		return
	}
	if s.readStore == nil {
		writeAPIError(w, ErrCodeServiceUnavailable, http.StatusServiceUnavailable, "read store not available")
		return
	}

	branch := r.URL.Query().Get("branch")
	if branch == "" {
		writeError(w, http.StatusBadRequest, "branch parameter is required")
		return
	}

	var atSequence int64
	if v := r.URL.Query().Get("at"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid 'at' parameter")
			return
		}
		atSequence = n
	}

	// Verify the branch exists before we start streaming.
	bi, err := s.readStore.GetBranch(r.Context(), repo, branch)
	if err != nil {
		slog.Error("internal error", "op", "archive", "repo", repo, "branch", branch, "error", err)
		writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "query failed")
		return
	}
	if bi == nil {
		writeAPIError(w, ErrCodeBranchNotFound, http.StatusNotFound, "branch not found")
		return
	}

	w.Header().Set("Content-Type", "application/x-tar")
	if err := writeArchive(r.Context(), s.readStore, w, repo, branch, atSequence); err != nil {
		slog.Error("archive: write error", "repo", repo, "branch", branch, "error", err)
	}
}

// handleChain implements GET /repos/:name/-/chain?from=N&to=N
// Returns commit metadata for sequences in [from, to] with commit hashes and file content hashes.
func (s *server) handleChain(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("name")
	if !s.validateRepo(w, r, repo) {
		return
	}
	if s.readStore == nil {
		writeAPIError(w, ErrCodeServiceUnavailable, http.StatusServiceUnavailable, "read store not available")
		return
	}

	fromStr := r.URL.Query().Get("from")
	toStr := r.URL.Query().Get("to")
	if fromStr == "" || toStr == "" {
		writeError(w, http.StatusBadRequest, "from and to query parameters are required")
		return
	}
	from, err := strconv.ParseInt(fromStr, 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid 'from' parameter")
		return
	}
	to, err := strconv.ParseInt(toStr, 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid 'to' parameter")
		return
	}
	if from > to {
		writeError(w, http.StatusBadRequest, "'from' must be <= 'to'")
		return
	}

	entries, err := s.readStore.GetChain(r.Context(), repo, from, to)
	if err != nil {
		slog.Error("internal error", "op", "chain", "repo", repo, "error", err)
		writeAPIError(w, ErrCodeInternalError, http.StatusInternalServerError, "query failed")
		return
	}
	if entries == nil {
		entries = []store.ChainEntry{}
	}
	writeJSON(w, http.StatusOK, entries)
}
