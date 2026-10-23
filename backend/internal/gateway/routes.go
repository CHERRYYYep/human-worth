package gateway

import "net/http"

// httpHandlers keeps the security-sensitive handler binding explicit. The
// shared contract owns route metadata; startup checks both sides match exactly.
func (h *Handler) httpHandlers() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"getTask":                  h.getPublicTask,
		"listApprovedEntries":      h.listPublicEntries,
		"requestTaskReview":        h.requestTaskReview,
		"listReviews":              h.listReviews,
		"getReview":                h.getReview,
		"decideReview":             h.decideReview,
		"downloadReviewAsset":      h.downloadReviewAsset,
		"createTaskDraft":          h.createDraft,
		"listMySubmissions":        h.listSubmissions,
		"createAssetUploadSession": h.createAssetUploadSession,
		"uploadAsset":              h.uploadAsset,
		"completeAsset":            h.completeAsset,
		"abortAsset":               h.abortAsset,
		"getAssetUploadSession":    h.getAssetUploadSession,
		"listMyAssets":             h.listMyAssets,
		"getAssetMetadata":         h.getAssetMetadata,
		"downloadAsset":            h.downloadAsset,
		"downloadPublicAsset":      h.downloadPublicAsset,
		"getMyTaskSubmission":      h.getDraft,
		"replaceTaskDraft":         h.replaceDraft,
		"startGoogleLogin":         h.start,
		"completeGoogleLogin":      h.callback,
		"getCurrentSession":        h.me,
		"logoutCurrentSession":     h.logout,
		"createMcpToken":           h.createToken,
		"listMyMcpTokens":          h.listTokens,
		"revokeMcpToken":           h.revokeToken,
		"startChallengeRun":        h.startChallenge,
		"listChallengeRuns":        h.listChallenges,
		"getChallengeRun":          h.getChallenge,
		"cancelChallengeRun":       h.cancelChallenge,
		"restartChallengeRun":      h.restartChallenge,
		"registerCandidate":        h.registerChallenge,
	}
}
