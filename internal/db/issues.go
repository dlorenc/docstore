package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/dlorenc/docstore/internal/model"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// Sentinel errors for issue operations.
var (
	ErrIssueNotFound        = fmt.Errorf("issue not found")
	ErrIssueCommentNotFound = fmt.Errorf("issue comment not found")
	ErrIssueRefExists       = fmt.Errorf("issue ref already exists")
)

// scanIssue scans one issues row into a model.Issue.
func scanIssue(row interface{ Scan(...any) error }) (*model.Issue, error) {
	var iss model.Issue
	var body sql.NullString
	var closeReason sql.NullString
	var closedBy sql.NullString
	var closedAt sql.NullTime
	if err := row.Scan(
		&iss.ID, &iss.Repo, &iss.Number, &iss.Title, &body,
		&iss.Author, &iss.State, &closeReason, &closedBy,
		pq.Array(&iss.Labels), &closedAt, &iss.CreatedAt, &iss.UpdatedAt,
	); err != nil {
		return nil, err
	}
	if body.Valid {
		iss.Body = body.String
	}
	if closeReason.Valid {
		r := model.IssueCloseReason(closeReason.String)
		iss.CloseReason = &r
	}
	if closedBy.Valid {
		iss.ClosedBy = &closedBy.String
	}
	if closedAt.Valid {
		iss.ClosedAt = &closedAt.Time
	}
	if iss.Labels == nil {
		iss.Labels = []string{}
	}
	return &iss, nil
}

const issueColumns = `id::text, repo, number, title, body, author, state::text,
	close_reason::text, closed_by, labels, closed_at, created_at, updated_at`

// CreateIssue inserts a new issue, allocating a per-repo issue number atomically.
func (s *Store) CreateIssue(ctx context.Context, repo, title, body, author string, labels []string) (*model.Issue, error) {
	if labels == nil {
		labels = []string{}
	}
	id := uuid.New().String()
	var nullBody sql.NullString
	if body != "" {
		nullBody = sql.NullString{String: body, Valid: true}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("create issue: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	// Lock the repo row to serialize number allocation.
	_, err = tx.ExecContext(ctx, `SELECT 1 FROM repos WHERE name = $1 FOR UPDATE`, repo)
	if err != nil {
		return nil, fmt.Errorf("create issue: lock repo: %w", err)
	}

	var number int64
	err = tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(number), 0) + 1 FROM issues WHERE repo = $1`,
		repo,
	).Scan(&number)
	if err != nil {
		return nil, fmt.Errorf("create issue: allocate number: %w", err)
	}

	var iss model.Issue
	iss.ID = id
	iss.Repo = repo
	iss.Number = number
	iss.Title = title
	iss.Body = body
	iss.Author = author
	iss.State = model.IssueStateOpen
	iss.Labels = labels

	err = tx.QueryRowContext(ctx,
		`INSERT INTO issues (id, repo, number, title, body, author, labels)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 RETURNING created_at, updated_at`,
		id, repo, number, title, nullBody, author, pq.Array(labels),
	).Scan(&iss.CreatedAt, &iss.UpdatedAt)
	if err != nil {
		if isForeignKeyViolation(err) {
			return nil, ErrRepoNotFound
		}
		return nil, fmt.Errorf("create issue: insert: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("create issue: commit: %w", err)
	}
	return &iss, nil
}

// GetIssue returns a single issue by repo and issue number.
// Returns ErrIssueNotFound if no matching issue exists.
func (s *Store) GetIssue(ctx context.Context, repo string, number int64) (*model.Issue, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+issueColumns+` FROM issues WHERE repo = $1 AND number = $2`,
		repo, number,
	)
	iss, err := scanIssue(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrIssueNotFound
		}
		return nil, fmt.Errorf("get issue: %w", err)
	}
	return iss, nil
}

// ListIssues returns issues for a repo ordered by number DESC.
// stateFilter, if non-empty, restricts to that state. authorFilter, if non-empty, restricts to that author.
func (s *Store) ListIssues(ctx context.Context, repo, stateFilter, authorFilter string) ([]model.Issue, error) {
	q := `SELECT ` + issueColumns + ` FROM issues WHERE repo = $1`
	args := []any{repo}
	if stateFilter != "" {
		args = append(args, stateFilter)
		q += fmt.Sprintf(" AND state = $%d::issue_state", len(args))
	}
	if authorFilter != "" {
		args = append(args, authorFilter)
		q += fmt.Sprintf(" AND author = $%d", len(args))
	}
	q += " ORDER BY number DESC"

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list issues: %w", err)
	}
	defer rows.Close()

	var issues []model.Issue
	for rows.Next() {
		iss, err := scanIssue(rows)
		if err != nil {
			return nil, fmt.Errorf("list issues: scan: %w", err)
		}
		issues = append(issues, *iss)
	}
	if issues == nil {
		issues = []model.Issue{}
	}
	return issues, rows.Err()
}

// UpdateIssue updates the title, body, and/or labels of an issue.
// Returns ErrIssueNotFound if no matching issue exists.
func (s *Store) UpdateIssue(ctx context.Context, repo string, number int64, title, body *string, labels *[]string) (*model.Issue, error) {
	if title == nil && body == nil && labels == nil {
		return s.GetIssue(ctx, repo, number)
	}

	setClauses := []string{"updated_at = now()"}
	args := []any{}
	argIdx := 1

	if title != nil {
		setClauses = append(setClauses, fmt.Sprintf("title = $%d", argIdx))
		args = append(args, *title)
		argIdx++
	}
	if body != nil {
		setClauses = append(setClauses, fmt.Sprintf("body = $%d", argIdx))
		args = append(args, *body)
		argIdx++
	}
	if labels != nil {
		setClauses = append(setClauses, fmt.Sprintf("labels = $%d", argIdx))
		args = append(args, pq.Array(*labels))
		argIdx++
	}

	setSQL := strings.Join(setClauses, ", ")

	args = append(args, repo, number)
	repoIdx := argIdx
	numIdx := argIdx + 1

	row := s.db.QueryRowContext(ctx,
		fmt.Sprintf(`UPDATE issues SET %s WHERE repo = $%d AND number = $%d RETURNING `+issueColumns, setSQL, repoIdx, numIdx),
		args...,
	)
	iss, err := scanIssue(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrIssueNotFound
		}
		return nil, fmt.Errorf("update issue: %w", err)
	}
	return iss, nil
}

// CloseIssue sets the issue state to 'closed' with a reason and the closer's identity.
// Returns ErrIssueNotFound if no matching issue exists.
func (s *Store) CloseIssue(ctx context.Context, repo string, number int64, reason model.IssueCloseReason, closedBy string) (*model.Issue, error) {
	row := s.db.QueryRowContext(ctx,
		`UPDATE issues
		 SET state = 'closed', close_reason = $3::issue_close_reason, closed_by = $4, closed_at = now(), updated_at = now()
		 WHERE repo = $1 AND number = $2
		 RETURNING `+issueColumns,
		repo, number, string(reason), closedBy,
	)
	iss, err := scanIssue(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrIssueNotFound
		}
		return nil, fmt.Errorf("close issue: %w", err)
	}
	return iss, nil
}

// ReopenIssue sets the issue state back to 'open', clearing close_reason and closed_by.
// Returns ErrIssueNotFound if no matching issue exists.
func (s *Store) ReopenIssue(ctx context.Context, repo string, number int64) (*model.Issue, error) {
	row := s.db.QueryRowContext(ctx,
		`UPDATE issues
		 SET state = 'open', close_reason = NULL, closed_by = NULL, closed_at = NULL, updated_at = now()
		 WHERE repo = $1 AND number = $2
		 RETURNING `+issueColumns,
		repo, number,
	)
	iss, err := scanIssue(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrIssueNotFound
		}
		return nil, fmt.Errorf("reopen issue: %w", err)
	}
	return iss, nil
}

// ---------------------------------------------------------------------------
// Issue comments
// ---------------------------------------------------------------------------

const issueCommentColumns = `id::text, issue_id::text, repo, body, author, created_at, edited_at`

func scanIssueComment(row interface{ Scan(...any) error }) (*model.IssueComment, error) {
	var c model.IssueComment
	var editedAt sql.NullTime
	if err := row.Scan(&c.ID, &c.IssueID, &c.Repo, &c.Body, &c.Author, &c.CreatedAt, &editedAt); err != nil {
		return nil, err
	}
	if editedAt.Valid {
		c.EditedAt = &editedAt.Time
	}
	return &c, nil
}

// CreateIssueComment inserts a new comment on an issue identified by repo+number.
// Returns ErrIssueNotFound if the issue does not exist.
func (s *Store) CreateIssueComment(ctx context.Context, repo string, number int64, body, author string) (*model.IssueComment, error) {
	// Resolve issue_id from repo+number.
	var issueID string
	err := s.db.QueryRowContext(ctx,
		`SELECT id::text FROM issues WHERE repo = $1 AND number = $2`,
		repo, number,
	).Scan(&issueID)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrIssueNotFound
		}
		return nil, fmt.Errorf("create issue comment: resolve issue: %w", err)
	}

	id := uuid.New().String()
	row := s.db.QueryRowContext(ctx,
		`INSERT INTO issue_comments (id, issue_id, repo, body, author)
		 VALUES ($1, $2::uuid, $3, $4, $5)
		 RETURNING `+issueCommentColumns,
		id, issueID, repo, body, author,
	)
	c, err := scanIssueComment(row)
	if err != nil {
		return nil, fmt.Errorf("create issue comment: %w", err)
	}
	return c, nil
}

// GetIssueComment returns a single comment by ID within the given repo.
// Returns ErrIssueCommentNotFound if no matching comment exists.
func (s *Store) GetIssueComment(ctx context.Context, repo, id string) (*model.IssueComment, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+issueCommentColumns+` FROM issue_comments WHERE repo = $1 AND id = $2::uuid`,
		repo, id,
	)
	c, err := scanIssueComment(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrIssueCommentNotFound
		}
		return nil, fmt.Errorf("get issue comment: %w", err)
	}
	return c, nil
}

// ListIssueComments returns all comments on an issue identified by repo+number, ordered by created_at ASC.
func (s *Store) ListIssueComments(ctx context.Context, repo string, number int64) ([]model.IssueComment, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+issueCommentColumns+`
		 FROM issue_comments
		 WHERE repo = $1 AND issue_id = (SELECT id FROM issues WHERE repo = $1 AND number = $2)
		 ORDER BY created_at ASC`,
		repo, number,
	)
	if err != nil {
		return nil, fmt.Errorf("list issue comments: %w", err)
	}
	defer rows.Close()

	var comments []model.IssueComment
	for rows.Next() {
		c, err := scanIssueComment(rows)
		if err != nil {
			return nil, fmt.Errorf("list issue comments: scan: %w", err)
		}
		comments = append(comments, *c)
	}
	if comments == nil {
		comments = []model.IssueComment{}
	}
	return comments, rows.Err()
}

// UpdateIssueComment updates the body of a comment and sets edited_at = now().
// Returns ErrIssueCommentNotFound if no matching comment exists.
func (s *Store) UpdateIssueComment(ctx context.Context, repo, id, body string) (*model.IssueComment, error) {
	row := s.db.QueryRowContext(ctx,
		`UPDATE issue_comments SET body = $3, edited_at = now()
		 WHERE repo = $1 AND id = $2::uuid
		 RETURNING `+issueCommentColumns,
		repo, id, body,
	)
	c, err := scanIssueComment(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrIssueCommentNotFound
		}
		return nil, fmt.Errorf("update issue comment: %w", err)
	}
	return c, nil
}

// DeleteIssueComment deletes a comment by ID within the given repo.
// Returns ErrIssueCommentNotFound if no matching comment exists.
func (s *Store) DeleteIssueComment(ctx context.Context, repo, id string) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM issue_comments WHERE repo = $1 AND id = $2::uuid`,
		repo, id,
	)
	if err != nil {
		return fmt.Errorf("delete issue comment: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete issue comment: rows affected: %w", err)
	}
	if n == 0 {
		return ErrIssueCommentNotFound
	}
	return nil
}

// ---------------------------------------------------------------------------
// Issue refs
// ---------------------------------------------------------------------------

const issueRefColumns = `id::text, issue_id::text, repo, ref_type::text, ref_id, created_at`

func scanIssueRef(row interface{ Scan(...any) error }) (*model.IssueRef, error) {
	var r model.IssueRef
	if err := row.Scan(&r.ID, &r.IssueID, &r.Repo, &r.RefType, &r.RefID, &r.CreatedAt); err != nil {
		return nil, err
	}
	return &r, nil
}

// CreateIssueRef attaches a cross-reference to an issue identified by repo+number.
// Returns ErrIssueNotFound if the issue does not exist.
// Returns ErrIssueRefExists if the (issue_id, ref_type, ref_id) combination already exists.
func (s *Store) CreateIssueRef(ctx context.Context, repo string, number int64, refType model.IssueRefType, refID string) (*model.IssueRef, error) {
	var issueID string
	err := s.db.QueryRowContext(ctx,
		`SELECT id::text FROM issues WHERE repo = $1 AND number = $2`,
		repo, number,
	).Scan(&issueID)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrIssueNotFound
		}
		return nil, fmt.Errorf("create issue ref: resolve issue: %w", err)
	}

	id := uuid.New().String()
	row := s.db.QueryRowContext(ctx,
		`INSERT INTO issue_refs (id, issue_id, repo, ref_type, ref_id)
		 VALUES ($1, $2::uuid, $3, $4::issue_ref_type, $5)
		 RETURNING `+issueRefColumns,
		id, issueID, repo, string(refType), refID,
	)
	r, err := scanIssueRef(row)
	if err != nil {
		if isDuplicateKeyError(err) {
			return nil, ErrIssueRefExists
		}
		return nil, fmt.Errorf("create issue ref: %w", err)
	}
	return r, nil
}

// ListIssueRefs returns all refs attached to an issue identified by repo+number,
// ordered by created_at ASC.
func (s *Store) ListIssueRefs(ctx context.Context, repo string, number int64) ([]model.IssueRef, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+issueRefColumns+`
		 FROM issue_refs
		 WHERE repo = $1 AND issue_id = (SELECT id FROM issues WHERE repo = $1 AND number = $2)
		 ORDER BY created_at ASC`,
		repo, number,
	)
	if err != nil {
		return nil, fmt.Errorf("list issue refs: %w", err)
	}
	defer rows.Close()

	var refs []model.IssueRef
	for rows.Next() {
		r, err := scanIssueRef(rows)
		if err != nil {
			return nil, fmt.Errorf("list issue refs: scan: %w", err)
		}
		refs = append(refs, *r)
	}
	if refs == nil {
		refs = []model.IssueRef{}
	}
	return refs, rows.Err()
}

// ListIssuesByRef returns issues in a repo that have a ref matching (refType, refID),
// ordered by issue number ASC.
func (s *Store) ListIssuesByRef(ctx context.Context, repo string, refType model.IssueRefType, refID string) ([]model.Issue, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+issueColumns+`
		 FROM issues
		 WHERE id IN (
		     SELECT issue_id FROM issue_refs
		     WHERE repo = $1 AND ref_type = $2::issue_ref_type AND ref_id = $3
		 )
		 ORDER BY number ASC`,
		repo, string(refType), refID,
	)
	if err != nil {
		return nil, fmt.Errorf("list issues by ref: %w", err)
	}
	defer rows.Close()

	var issues []model.Issue
	for rows.Next() {
		iss, err := scanIssue(rows)
		if err != nil {
			return nil, fmt.Errorf("list issues by ref: scan: %w", err)
		}
		issues = append(issues, *iss)
	}
	if issues == nil {
		issues = []model.Issue{}
	}
	return issues, rows.Err()
}
