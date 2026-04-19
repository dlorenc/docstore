package types

// IssueOpened is emitted when a new issue is opened.
type IssueOpened struct {
	Repo    string `json:"repo"`
	IssueID string `json:"issue_id"`
	Number  int64  `json:"number"`
	Author  string `json:"author"`
}

func (e IssueOpened) Type() string   { return "com.docstore.issue.opened" }
func (e IssueOpened) Source() string { return "/repos/" + e.Repo }
func (e IssueOpened) Data() any      { return e }

// IssueClosed is emitted when an issue is closed.
type IssueClosed struct {
	Repo     string `json:"repo"`
	IssueID  string `json:"issue_id"`
	Number   int64  `json:"number"`
	Reason   string `json:"reason"`
	ClosedBy string `json:"closed_by"`
}

func (e IssueClosed) Type() string   { return "com.docstore.issue.closed" }
func (e IssueClosed) Source() string { return "/repos/" + e.Repo }
func (e IssueClosed) Data() any      { return e }

// IssueReopened is emitted when an issue is reopened.
type IssueReopened struct {
	Repo    string `json:"repo"`
	IssueID string `json:"issue_id"`
	Number  int64  `json:"number"`
}

func (e IssueReopened) Type() string   { return "com.docstore.issue.reopened" }
func (e IssueReopened) Source() string { return "/repos/" + e.Repo }
func (e IssueReopened) Data() any      { return e }

// IssueUpdated is emitted when an issue is updated.
type IssueUpdated struct {
	Repo    string `json:"repo"`
	IssueID string `json:"issue_id"`
	Number  int64  `json:"number"`
}

func (e IssueUpdated) Type() string   { return "com.docstore.issue.updated" }
func (e IssueUpdated) Source() string { return "/repos/" + e.Repo }
func (e IssueUpdated) Data() any      { return e }

// IssueCommentCreated is emitted when a comment is added to an issue.
type IssueCommentCreated struct {
	Repo      string `json:"repo"`
	IssueID   string `json:"issue_id"`
	Number    int64  `json:"number"`
	CommentID string `json:"comment_id"`
	Author    string `json:"author"`
}

func (e IssueCommentCreated) Type() string   { return "com.docstore.issue.comment.created" }
func (e IssueCommentCreated) Source() string { return "/repos/" + e.Repo }
func (e IssueCommentCreated) Data() any      { return e }

// IssueCommentUpdated is emitted when a comment is updated.
type IssueCommentUpdated struct {
	Repo      string `json:"repo"`
	IssueID   string `json:"issue_id"`
	Number    int64  `json:"number"`
	CommentID string `json:"comment_id"`
}

func (e IssueCommentUpdated) Type() string   { return "com.docstore.issue.comment.updated" }
func (e IssueCommentUpdated) Source() string { return "/repos/" + e.Repo }
func (e IssueCommentUpdated) Data() any      { return e }
