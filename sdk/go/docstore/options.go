package docstore

import (
	"strconv"

	"github.com/dlorenc/docstore/api"
)

// FileOption narrows a file read to a specific branch head, sequence, or
// named release.
type FileOption func(*reqParams)

// AtHead reads the file at the head of the given branch. Passing "" selects
// the server default ("main").
func AtHead(branch string) FileOption {
	return func(p *reqParams) {
		if branch != "" {
			p.set("branch", branch)
		}
	}
}

// AtSequence reads the file as of a specific commit sequence.
func AtSequence(seq int64) FileOption {
	return func(p *reqParams) { p.set("at", strconv.FormatInt(seq, 10)) }
}

// AtRelease reads the file at the sequence that was pinned by the named
// release.
func AtRelease(name string) FileOption {
	return func(p *reqParams) { p.set("at", name) }
}

// TreeOption narrows a tree materialisation.
type TreeOption func(*reqParams)

func TreeAtHead(branch string) TreeOption {
	return func(p *reqParams) {
		if branch != "" {
			p.set("branch", branch)
		}
	}
}
func TreeAtSequence(seq int64) TreeOption {
	return func(p *reqParams) { p.set("at", strconv.FormatInt(seq, 10)) }
}
func TreeAtRelease(name string) TreeOption {
	return func(p *reqParams) { p.set("at", name) }
}
func TreeLimit(n int) TreeOption {
	return func(p *reqParams) { p.set("limit", strconv.Itoa(n)) }
}
func TreeAfter(path string) TreeOption {
	return func(p *reqParams) { p.set("after", path) }
}

// HistoryOption narrows a file-history listing.
type HistoryOption func(*reqParams)

func HistoryOnBranch(branch string) HistoryOption {
	return func(p *reqParams) {
		if branch != "" {
			p.set("branch", branch)
		}
	}
}
func HistoryLimit(n int) HistoryOption {
	return func(p *reqParams) { p.set("limit", strconv.Itoa(n)) }
}
func HistoryAfter(seq int64) HistoryOption {
	return func(p *reqParams) { p.set("after", strconv.FormatInt(seq, 10)) }
}

// BranchListOption filters a branch listing.
type BranchListOption func(*reqParams)

// BranchStatusFilter restricts to branches in a given status.
func BranchStatusFilter(s api.BranchStatus) BranchListOption {
	return func(p *reqParams) { p.set("status", string(s)) }
}

// BranchDraftOnly returns only draft branches.
func BranchDraftOnly() BranchListOption {
	return func(p *reqParams) { p.set("draft", "true") }
}

// BranchIncludeDraft includes draft branches in the listing (in addition to
// non-draft).
func BranchIncludeDraft() BranchListOption {
	return func(p *reqParams) { p.set("include_draft", "true") }
}

// ReviewListOption narrows a review listing.
type ReviewListOption func(*reqParams)

func ReviewsAt(seq int64) ReviewListOption {
	return func(p *reqParams) { p.set("at", strconv.FormatInt(seq, 10)) }
}

// CheckListOption narrows a check-run listing.
type CheckListOption func(*reqParams)

func ChecksAt(seq int64) CheckListOption {
	return func(p *reqParams) { p.set("at", strconv.FormatInt(seq, 10)) }
}

// ReleaseListOption narrows a release listing.
type ReleaseListOption func(*reqParams)

func ReleasesLimit(n int) ReleaseListOption {
	return func(p *reqParams) { p.set("limit", strconv.Itoa(n)) }
}
func ReleasesAfter(id string) ReleaseListOption {
	return func(p *reqParams) { p.set("after", id) }
}

// buildParams applies the option functions to a fresh params struct and
// returns the resulting url.Values.
func buildParams[F ~func(*reqParams)](opts []F) *reqParams {
	p := &reqParams{}
	for _, o := range opts {
		o(p)
	}
	return p
}
