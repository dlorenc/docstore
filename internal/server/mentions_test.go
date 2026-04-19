package server

import (
	"reflect"
	"testing"
)

func TestParseMentions(t *testing.T) {
	validUUID := "550e8400-e29b-41d4-a716-446655440000"

	tests := []struct {
		name           string
		body           string
		wantProposals  []string
		wantCommits    []int64
		wantIssues     []int64
	}{
		{
			name:          "proposal mention",
			body:          "proposal:" + validUUID + " body text",
			wantProposals: []string{validUUID},
			wantCommits:   nil,
			wantIssues:    nil,
		},
		{
			name:          "commit mention",
			body:          "commit:42 body text",
			wantProposals: nil,
			wantCommits:   []int64{42},
			wantIssues:    nil,
		},
		{
			name:          "issue mention",
			body:          "see #5 for context",
			wantProposals: nil,
			wantCommits:   nil,
			wantIssues:    []int64{5},
		},
		{
			name:          "color hex does not extract issue number",
			body:          "color: #123456",
			wantProposals: nil,
			wantCommits:   nil,
			wantIssues:    nil,
		},
		{
			name: "multiple mentions",
			body: "proposal:" + validUUID + " and commit:7 also see #3 and #10",
			wantProposals: []string{validUUID},
			wantCommits:   []int64{7},
			wantIssues:    []int64{3, 10},
		},
		{
			name:          "empty body",
			body:          "",
			wantProposals: nil,
			wantCommits:   nil,
			wantIssues:    nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotProposals, gotCommits, gotIssues := parseMentions(tc.body)

			if !reflect.DeepEqual(gotProposals, tc.wantProposals) {
				t.Errorf("proposals: got %v, want %v", gotProposals, tc.wantProposals)
			}
			if !reflect.DeepEqual(gotCommits, tc.wantCommits) {
				t.Errorf("commits: got %v, want %v", gotCommits, tc.wantCommits)
			}
			if !reflect.DeepEqual(gotIssues, tc.wantIssues) {
				t.Errorf("issues: got %v, want %v", gotIssues, tc.wantIssues)
			}
		})
	}
}
