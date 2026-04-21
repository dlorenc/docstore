package model

import "github.com/dlorenc/docstore/api"

// HTTP request/response wire types are defined in github.com/dlorenc/docstore/api
// and re-exported here as aliases for backward compatibility with existing
// server and CLI code. New code should import the api package directly.

type PaginationParams = api.PaginationParams

type TreeEntry = api.TreeEntry
type TreeResponse = api.TreeResponse

type FileHistoryEntry = api.FileHistoryEntry
type FileHistoryResponse = api.FileHistoryResponse

type FileResponse = api.FileResponse

type DiffEntry = api.DiffEntry
type ConflictEntry = api.ConflictEntry
type DiffResponse = api.DiffResponse

type CommitDetail = api.CommitDetail
type GetCommitResponse = api.GetCommitResponse

type BranchesResponse = api.BranchesResponse

type PolicyResult = api.PolicyResult
type BranchStatusResponse = api.BranchStatusResponse

type FileChange = api.FileChange
type CommitRequest = api.CommitRequest
type CommitFileResult = api.CommitFileResult
type CommitResponse = api.CommitResponse

type CreateBranchRequest = api.CreateBranchRequest
type CreateBranchResponse = api.CreateBranchResponse
type UpdateBranchRequest = api.UpdateBranchRequest
type UpdateBranchResponse = api.UpdateBranchResponse
type DeleteBranchResponse = api.DeleteBranchResponse

type MergeRequest = api.MergeRequest
type MergeResponse = api.MergeResponse
type MergeConflictError = api.MergeConflictError
type MergePolicyError = api.MergePolicyError

type RebaseRequest = api.RebaseRequest
type RebaseResponse = api.RebaseResponse
type RebaseConflictError = api.RebaseConflictError

type CreateReviewRequest = api.CreateReviewRequest
type CreateReviewResponse = api.CreateReviewResponse

type CreateReviewCommentRequest = api.CreateReviewCommentRequest
type CreateReviewCommentResponse = api.CreateReviewCommentResponse

type CreateCheckRunRequest = api.CreateCheckRunRequest
type CreateCheckRunResponse = api.CreateCheckRunResponse
type RetryChecksRequest = api.RetryChecksRequest
type RetryChecksResponse = api.RetryChecksResponse

type CreateOrgRequest = api.CreateOrgRequest
type CreateOrgResponse = api.CreateOrgResponse
type ListOrgsResponse = api.ListOrgsResponse

type CreateRepoRequest = api.CreateRepoRequest
type ReposResponse = api.ReposResponse

type SetRoleRequest = api.SetRoleRequest
type RolesResponse = api.RolesResponse

type PurgeRequest = api.PurgeRequest
type PurgeResponse = api.PurgeResponse

type AddOrgMemberRequest = api.AddOrgMemberRequest
type OrgMembersResponse = api.OrgMembersResponse

type CreateInviteRequest = api.CreateInviteRequest
type CreateInviteResponse = api.CreateInviteResponse
type OrgInvitesResponse = api.OrgInvitesResponse

type CreateReleaseRequest = api.CreateReleaseRequest
type ListReleasesResponse = api.ListReleasesResponse

type CreateSubscriptionRequest = api.CreateSubscriptionRequest
type ListSubscriptionsResponse = api.ListSubscriptionsResponse

type CreateProposalRequest = api.CreateProposalRequest
type CreateProposalResponse = api.CreateProposalResponse
type UpdateProposalRequest = api.UpdateProposalRequest

type CreateIssueRequest = api.CreateIssueRequest
type CreateIssueResponse = api.CreateIssueResponse
type UpdateIssueRequest = api.UpdateIssueRequest
type CloseIssueRequest = api.CloseIssueRequest
type ReopenIssueRequest = api.ReopenIssueRequest
type ListIssuesResponse = api.ListIssuesResponse
type IssueResponse = api.IssueResponse
type CreateIssueCommentRequest = api.CreateIssueCommentRequest
type CreateIssueCommentResponse = api.CreateIssueCommentResponse
type UpdateIssueCommentRequest = api.UpdateIssueCommentRequest
type ListIssueCommentsResponse = api.ListIssueCommentsResponse
type AddIssueRefRequest = api.AddIssueRefRequest
type AddIssueRefResponse = api.AddIssueRefResponse
type ListIssueRefsResponse = api.ListIssueRefsResponse

type AgentContextResponse = api.AgentContextResponse

type ErrorResponse = api.ErrorResponse
