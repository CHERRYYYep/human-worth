package gateway

import (
	"net/http"
	"net/url"
	"strconv"
	"time"

	assetpb "github.com/KDZZZZZZ/human-worth/backend/gen/humanworth/asset/v1"
	contentpb "github.com/KDZZZZZZ/human-worth/backend/gen/humanworth/content/v1"
	pb "github.com/KDZZZZZZ/human-worth/backend/gen/humanworth/moderation/v1"
)

func noQuery(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.RawQuery != "" {
		problem(w, 400, "invalid_request")
		return false
	}
	return true
}
func (h *Handler) requestTaskReview(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.contentActor(w, r, contentpb.ContentService_RequestTaskReview_FullMethodName)
	if !ok {
		return
	}
	if !noQuery(w, r) {
		return
	}
	var body struct {
		ExpectedRevision int64 `json:"expectedRevision"`
	}
	if !decode(w, r, &body) {
		return
	}
	// Revalidate all stored file references; gateway never trusts submitted IDs or
	// fetches a detail per entry. The dedicated assertion carries the same session.
	assertion := ""
	if h.options.Asset != nil {
		assertion, ok = h.assetActor(w, r, assetpb.AssetService_CheckAssetOwnership_FullMethodName)
		if !ok {
			return
		}
	}
	out, err := h.options.Content.RequestTaskReview(r.Context(), &contentpb.RequestTaskReviewRequest{ActorAssertion: actor, TaskId: r.PathValue("taskId"), ExpectedRevision: body.ExpectedRevision, AssetActorAssertion: assertion})
	if err != nil {
		contentError(w, err)
		return
	}
	taskResponse(w, 200, out.Submission)
}
func (h *Handler) moderationActor(w http.ResponseWriter, r *http.Request, method string) (string, bool) {
	if h.options.Moderation == nil {
		problem(w, 503, "moderation_unavailable")
		return "", false
	}
	return h.actorFor(w, r, "moderation", method)
}
func reviewJSON(r *pb.Review) (map[string]any, error) {
	sub, err := taskJSON(r.GetSnapshot().GetSubmission())
	if err != nil {
		return nil, err
	}
	var reason, reviewer, decided, decision, applicationRevision any
	if r.Reason != nil {
		reason = *r.Reason
	}
	if r.ReviewerId != nil {
		reviewer = *r.ReviewerId
	}
	if r.DecidedAt != nil {
		decided = r.DecidedAt.AsTime().UTC().Format(time.RFC3339Nano)
	}
	switch r.Decision {
	case contentpb.ReviewOutcome_REVIEW_OUTCOME_APPROVE:
		decision = "approve"
	case contentpb.ReviewOutcome_REVIEW_OUTCOME_REJECT:
		decision = "reject"
	}
	if r.Receipt != nil {
		applicationRevision = r.Receipt.Revision
	}
	delivery := map[pb.DeliveryState]string{pb.DeliveryState_DELIVERY_STATE_UNDECIDED: "undecided", pb.DeliveryState_DELIVERY_STATE_PENDING: "pending", pb.DeliveryState_DELIVERY_STATE_APPLIED: "applied", pb.DeliveryState_DELIVERY_STATE_BLOCKED: "blocked"}[r.DeliveryState]
	return map[string]any{"id": r.Id, "submission": sub, "status": r.Status, "reason": reason, "reviewerId": reviewer, "createdAt": r.CreatedAt.AsTime().UTC().Format(time.RFC3339Nano), "decidedAt": decided, "decision": decision, "deliveryState": delivery, "entryIds": r.GetSnapshot().GetEntryIds(), "applicationRevision": applicationRevision}, nil
}
func reviewResponse(w http.ResponseWriter, r *pb.Review) {
	value, err := reviewJSON(r)
	if err != nil {
		problem(w, 500, "internal_error")
		return
	}
	code := 200
	if r.DeliveryState == pb.DeliveryState_DELIVERY_STATE_PENDING {
		code = 202
	}
	jsonResponse(w, code, value)
}
func (h *Handler) listReviews(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.moderationActor(w, r, pb.ModerationService_ListReviews_FullMethodName)
	if !ok {
		return
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		problem(w, 400, "invalid_request")
		return
	}
	for k, v := range q {
		if (k != "cursor" && k != "limit" && k != "status") || len(v) != 1 {
			problem(w, 400, "invalid_request")
			return
		}
	}
	request := &pb.ListReviewsRequest{ActorAssertion: actor, Limit: 20, Status: "pending", Cursor: q.Get("cursor")}
	if len(request.Cursor) > 2048 {
		problem(w, 400, "invalid_cursor")
		return
	}
	if q.Has("status") {
		request.Status = q.Get("status")
		if request.Status != "pending" && request.Status != "approved" && request.Status != "rejected" {
			problem(w, 400, "invalid_request")
			return
		}
	}
	if q.Has("limit") {
		limit, e := strconv.Atoi(q.Get("limit"))
		if e != nil || limit < 1 || limit > 100 {
			problem(w, 400, "invalid_request")
			return
		}
		request.Limit = int32(limit)
	}
	out, err := h.options.Moderation.ListReviews(r.Context(), request)
	if err != nil {
		rpcErrorFor(w, err, "moderation_unavailable")
		return
	}
	items := make([]map[string]any, 0, len(out.Items))
	for _, r := range out.Items {
		v, e := reviewJSON(r)
		if e != nil {
			problem(w, 500, "internal_error")
			return
		}
		items = append(items, v)
	}
	var cursor any
	if out.NextCursor != "" {
		cursor = out.NextCursor
	}
	jsonResponse(w, 200, map[string]any{"items": items, "nextCursor": cursor})
}
func (h *Handler) getReview(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.moderationActor(w, r, pb.ModerationService_GetReview_FullMethodName)
	if !ok {
		return
	}
	if !noQuery(w, r) {
		return
	}
	out, err := h.options.Moderation.GetReview(r.Context(), &pb.GetReviewRequest{ActorAssertion: actor, ReviewId: r.PathValue("reviewId")})
	if err != nil {
		rpcErrorFor(w, err, "moderation_unavailable")
		return
	}
	value, err := reviewJSON(out.Review)
	if err != nil {
		problem(w, 500, "internal_error")
		return
	}
	jsonResponse(w, 200, value)
}
func (h *Handler) decideReview(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.moderationActor(w, r, pb.ModerationService_DecideReview_FullMethodName)
	if !ok {
		return
	}
	if !noQuery(w, r) {
		return
	}
	var body struct {
		Decision string  `json:"decision"`
		Reason   *string `json:"reason"`
	}
	if !decode(w, r, &body) {
		return
	}
	decision := contentpb.ReviewOutcome_REVIEW_OUTCOME_UNSPECIFIED
	switch body.Decision {
	case "approve":
		decision = contentpb.ReviewOutcome_REVIEW_OUTCOME_APPROVE
	case "reject":
		decision = contentpb.ReviewOutcome_REVIEW_OUTCOME_REJECT
	default:
		problem(w, 400, "invalid_review_decision")
		return
	}
	out, err := h.options.Moderation.DecideReview(r.Context(), &pb.DecideReviewRequest{ActorAssertion: actor, ReviewId: r.PathValue("reviewId"), Decision: decision, Reason: body.Reason})
	if err != nil {
		rpcErrorFor(w, err, "moderation_unavailable")
		return
	}
	reviewResponse(w, out.Review)
}
func (h *Handler) downloadReviewAsset(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.assetActor(w, r, assetpb.AssetService_DownloadReviewAsset_FullMethodName)
	if !ok {
		return
	}
	if !noQuery(w, r) {
		return
	}
	assertion, ok := h.contentActor(w, r, contentpb.ContentService_CheckReviewAssetAccess_FullMethodName)
	if !ok {
		return
	}
	stream, err := h.options.Asset.DownloadReviewAsset(r.Context(), &assetpb.DownloadReviewAssetRequest{ActorAssertion: actor, ContentActorAssertion: assertion, ReviewId: r.PathValue("reviewId"), AssetId: r.PathValue("assetId")})
	if err != nil {
		assetError(w, err)
		return
	}
	h.serveAssetStream(w, r, func() (*assetpb.DownloadPublicAssetResponse, error) {
		frame, err := stream.Recv()
		if err != nil {
			return nil, err
		}
		out := &assetpb.DownloadPublicAssetResponse{Offset: frame.Offset}
		switch v := frame.Value.(type) {
		case *assetpb.DownloadReviewAssetResponse_Metadata:
			out.Value = &assetpb.DownloadPublicAssetResponse_Metadata{Metadata: v.Metadata}
		case *assetpb.DownloadReviewAssetResponse_Data:
			out.Value = &assetpb.DownloadPublicAssetResponse_Data{Data: v.Data}
		}
		return out, nil
	})
}
