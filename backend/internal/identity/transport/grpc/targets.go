package grpc

import (
	assetpb "github.com/KDZZZZZZ/human-worth/backend/gen/humanworth/asset/v1"
	challengepb "github.com/KDZZZZZZ/human-worth/backend/gen/humanworth/challenge/v1"
	contentpb "github.com/KDZZZZZZ/human-worth/backend/gen/humanworth/content/v1"
	pb "github.com/KDZZZZZZ/human-worth/backend/gen/humanworth/identity/v1"
	moderationpb "github.com/KDZZZZZZ/human-worth/backend/gen/humanworth/moderation/v1"
	"github.com/KDZZZZZZ/human-worth/backend/internal/identity/application"
	"github.com/KDZZZZZZ/human-worth/backend/internal/identity/domain"
)

// Targets binds wire method names to semantic operations. The domain owns policy.
func Targets() []application.Target {
	return []application.Target{
		{Operation: domain.RequestTaskReview, Audience: "content", Method: contentpb.ContentService_RequestTaskReview_FullMethodName},
		{Operation: domain.ListReviews, Audience: "moderation", Method: moderationpb.ModerationService_ListReviews_FullMethodName},
		{Operation: domain.GetReview, Audience: "moderation", Method: moderationpb.ModerationService_GetReview_FullMethodName},
		{Operation: domain.DecideReview, Audience: "moderation", Method: moderationpb.ModerationService_DecideReview_FullMethodName},
		{Operation: domain.DownloadReviewAsset, Audience: "asset", Method: assetpb.AssetService_DownloadReviewAsset_FullMethodName},
		{Operation: domain.CheckReviewAssetAccess, Audience: "content", Method: contentpb.ContentService_CheckReviewAssetAccess_FullMethodName},
		{Operation: domain.CreateDraft, Audience: "content", Method: contentpb.ContentService_CreateTaskDraft_FullMethodName},
		{Operation: domain.ReadOwnSubmission, Audience: "content", Method: contentpb.ContentService_GetMyTaskSubmission_FullMethodName},
		{Operation: domain.ListOwnSubmissions, Audience: "content", Method: contentpb.ContentService_ListMySubmissions_FullMethodName},
		{Operation: domain.ReplaceDraft, Audience: "content", Method: contentpb.ContentService_ReplaceTaskDraft_FullMethodName},
		{Operation: domain.CurrentSession, Audience: "identity", Method: pb.IdentityService_GetCurrentSession_FullMethodName},
		{Operation: domain.Logout, Audience: "identity", Method: pb.IdentityService_LogoutCurrentSession_FullMethodName},
		{Operation: domain.CreateMCPToken, Audience: "identity", Method: pb.IdentityService_CreateMcpToken_FullMethodName},
		{Operation: domain.ListMCPTokens, Audience: "identity", Method: pb.IdentityService_ListMyMcpTokens_FullMethodName},
		{Operation: domain.RevokeMCPToken, Audience: "identity", Method: pb.IdentityService_RevokeMcpToken_FullMethodName},
		{Operation: domain.ReadTask, Audience: "content", Method: "/humanworth.content.v1.ContentService/GetTask"},
		{Operation: domain.ListTasks, Audience: "discovery", Method: "/humanworth.discovery.v1.DiscoveryService/ListTasks"},
		{Operation: domain.ViewStatistics, Audience: "voting", Method: "/humanworth.voting.v1.VotingService/ViewTaskStatistics"},
		{Operation: domain.CastVote, Audience: "voting", Method: "/humanworth.voting.v1.VotingService/CastVote"},
		{Operation: domain.StartRun, Audience: "challenge", Method: challengepb.ChallengeService_StartRun_FullMethodName},
		{Operation: domain.GetRun, Audience: "challenge", Method: challengepb.ChallengeService_GetRun_FullMethodName},
		{Operation: domain.ListRuns, Audience: "challenge", Method: challengepb.ChallengeService_ListRuns_FullMethodName},
		{Operation: domain.GetRunSummary, Audience: "challenge", Method: challengepb.ChallengeService_GetRunSummary_FullMethodName},
		{Operation: domain.CancelRun, Audience: "challenge", Method: challengepb.ChallengeService_CancelRun_FullMethodName},
		{Operation: domain.RestartRun, Audience: "challenge", Method: challengepb.ChallengeService_RestartRun_FullMethodName},
		{Operation: domain.RegisterCandidate, Audience: "challenge", Method: challengepb.ChallengeService_RegisterCandidate_FullMethodName},
		{Operation: domain.CreateAssetUploadSession, Audience: "asset", Method: assetpb.AssetService_CreateUploadSession_FullMethodName},
		{Operation: domain.UploadAsset, Audience: "asset", Method: assetpb.AssetService_UploadAsset_FullMethodName},
		{Operation: domain.CompleteAssetUpload, Audience: "asset", Method: assetpb.AssetService_CompleteUpload_FullMethodName},
		{Operation: domain.AbortAssetUpload, Audience: "asset", Method: assetpb.AssetService_AbortUpload_FullMethodName},
		{Operation: domain.GetAssetUploadSession, Audience: "asset", Method: assetpb.AssetService_GetUploadSession_FullMethodName},
		{Operation: domain.GetAssetMetadata, Audience: "asset", Method: assetpb.AssetService_GetAssetMetadata_FullMethodName},
		{Operation: domain.ListMyAssets, Audience: "asset", Method: assetpb.AssetService_ListMyAssets_FullMethodName},
		{Operation: domain.CheckAssetOwnership, Audience: "asset", Method: assetpb.AssetService_CheckAssetOwnership_FullMethodName},
		{Operation: domain.DownloadAsset, Audience: "asset", Method: assetpb.AssetService_DownloadAsset_FullMethodName},
	}
}
