package gateway

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	pb "github.com/KDZZZZZZ/human-worth/backend/gen/humanworth/asset/v1"
	"github.com/KDZZZZZZ/human-worth/backend/internal/contract"
	"github.com/KDZZZZZZ/human-worth/backend/internal/platform"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func assetError(w http.ResponseWriter, err error) { rpcErrorFor(w, err, "asset_unavailable") }
func assetJSON(value *pb.AssetMetadata) map[string]any {
	if value == nil {
		return nil
	}
	state := map[pb.AssetState]string{pb.AssetState_ASSET_STATE_STAGING: "staging", pb.AssetState_ASSET_STATE_READY: "ready", pb.AssetState_ASSET_STATE_FAILED: "failed", pb.AssetState_ASSET_STATE_DELETED: "deleted"}[value.State]
	result := map[string]any{"id": value.Id, "filename": value.OriginalFilename, "mediaType": value.MediaType, "bytes": value.Bytes, "sha256": value.Sha256, "state": state, "createdAt": value.CreatedAt.AsTime().UTC().Format("2006-01-02T15:04:05.999999999Z07:00"), "completedAt": nil}
	if value.CompletedAt != nil {
		result["completedAt"] = value.CompletedAt.AsTime().UTC().Format("2006-01-02T15:04:05.999999999Z07:00")
	}
	return result
}
func uploadSessionJSON(value *pb.UploadSession) map[string]any {
	return map[string]any{"id": value.Id, "assetId": value.AssetId, "receivedBytes": value.ReceivedBytes, "expectedBytes": value.ExpectedBytes, "state": strings.TrimPrefix(strings.ToLower(value.State.String()), "upload_session_state_"), "expiresAt": value.ExpiresAt.AsTime().UTC().Format("2006-01-02T15:04:05.999999999Z07:00")}
}
func (h *Handler) createAssetUploadSession(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.assetActor(w, r, contract.CreateAssetUploadSession)
	if !ok {
		return
	}
	if len(r.Header.Values("Idempotency-Key")) != 1 {
		problem(w, 400, "invalid_idempotency_key")
		return
	}
	var body struct {
		Filename      string `json:"filename"`
		MediaType     string `json:"mediaType"`
		ExpectedBytes int64  `json:"expectedBytes"`
		Sha256        string `json:"sha256"`
	}
	if !decode(w, r, &body) {
		return
	}
	response, err := h.options.Asset.CreateUploadSession(r.Context(), &pb.CreateUploadSessionRequest{ActorAssertion: actor, IdempotencyKey: r.Header.Get("Idempotency-Key"), OriginalFilename: body.Filename, DeclaredMediaType: body.MediaType, ExpectedBytes: body.ExpectedBytes, ExpectedSha256: body.Sha256})
	if err != nil {
		assetError(w, err)
		return
	}
	jsonResponse(w, 201, map[string]any{"asset": assetJSON(response.Asset), "uploadSession": uploadSessionJSON(response.UploadSession)})
}

var contentRangePattern = regexp.MustCompile(`^bytes ([0-9]+)-([0-9]+)/([0-9]+)$`)

func contentRange(r *http.Request) (int64, int64, int64, error) {
	values := r.Header.Values("Content-Range")
	if len(values) != 1 {
		return 0, 0, 0, errors.New("content range")
	}
	match := contentRangePattern.FindStringSubmatch(values[0])
	if len(match) != 4 {
		return 0, 0, 0, errors.New("content range")
	}
	start, err := strconv.ParseInt(match[1], 10, 64)
	if err != nil {
		return 0, 0, 0, errors.New("content range")
	}
	end, err := strconv.ParseInt(match[2], 10, 64)
	if err != nil {
		return 0, 0, 0, errors.New("content range")
	}
	total, err := strconv.ParseInt(match[3], 10, 64)
	if err != nil || end < start || total <= end {
		return 0, 0, 0, errors.New("content range")
	}
	if start > 32<<20 || end > 32<<20 || total > 32<<20 || end-start+1 > platform.AssetUploadChunkBytes || (r.ContentLength >= 0 && end-start+1 != r.ContentLength) {
		return 0, 0, 0, errors.New("content range")
	}
	if start%platform.AssetUploadChunkBytes != 0 || (end-start+1 != platform.AssetUploadChunkBytes && end+1 != total) {
		return 0, 0, 0, errors.New("content range chunk size")
	}
	return start, end, total, nil
}
func (h *Handler) uploadAsset(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.assetActor(w, r, contract.UploadAsset)
	if !ok {
		return
	}
	select {
	case h.uploads <- struct{}{}:
		defer func() { <-h.uploads }()
	default:
		problem(w, 429, "upload_capacity_exhausted")
		return
	}
	offset, end, total, err := contentRange(r)
	if err != nil {
		problem(w, 400, "invalid_content_range")
		return
	}
	if total > 32<<20 {
		problem(w, 413, "request_too_large")
		return
	}
	media, _, mediaErr := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mediaErr != nil || media != "application/octet-stream" {
		problem(w, 415, "unsupported_media_type")
		return
	}
	stream, err := h.options.Asset.UploadAsset(r.Context())
	if err != nil {
		assetError(w, err)
		return
	}
	sessionID := r.PathValue("uploadSessionId")
	if err = stream.Send(&pb.UploadAssetRequest{ActorAssertion: actor, UploadSessionId: sessionID, Offset: offset, UploadAttemptId: requestID(r)}); err != nil {
		assetError(w, uploadSendError(stream, err))
		return
	}
	data := make([]byte, int(end-offset+1))
	_, readErr := io.ReadFull(r.Body, data)
	if readErr != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(readErr, &tooLarge) {
			problem(w, 413, "request_too_large")
			return
		}
		if errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrUnexpectedEOF) {
			problem(w, 400, "invalid_content_range")
		} else {
			assetError(w, readErr)
		}
		return
	}
	var extra [1]byte
	if n, err := r.Body.Read(extra[:]); n > 0 || err != io.EOF {
		problem(w, 400, "invalid_content_range")
		return
	}
	if err = stream.Send(&pb.UploadAssetRequest{Offset: offset, Chunk: data, ChunkSha256: sha256Hex(data)}); err != nil {
		assetError(w, uploadSendError(stream, err))
		return
	}
	response, err := stream.CloseAndRecv()
	if err != nil {
		assetError(w, err)
		return
	}
	jsonResponse(w, 200, map[string]any{"uploadSessionId": response.UploadSessionId, "receivedBytes": response.ReceivedBytes, "nextOffset": response.NextOffset, "state": strings.TrimPrefix(strings.ToLower(response.State.String()), "upload_session_state_")})
}

func uploadSendError(stream pb.AssetService_UploadAssetClient, err error) error {
	if errors.Is(err, io.EOF) {
		// Send EOF only signals that the server stopped reading. Receive its
		// final status so an early denial or deadline keeps its HTTP category.
		if _, terminal := stream.CloseAndRecv(); terminal != nil {
			return terminal
		}
		return io.ErrUnexpectedEOF
	}
	return err
}

func requestID(r *http.Request) string {
	value := r.Header.Get("X-Request-ID")
	if len(value) >= 16 {
		return value
	}
	return "upload-attempt-0001"
}
func sha256Hex(data []byte) string { sum := sha256.Sum256(data); return fmt.Sprintf("%x", sum[:]) }
func (h *Handler) completeAsset(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.assetActor(w, r, contract.CompleteAssetUpload)
	if !ok {
		return
	}
	var body struct {
		UploadSessionID string `json:"uploadSessionId"`
	}
	if !decode(w, r, &body) {
		return
	}
	response, err := h.options.Asset.CompleteUpload(r.Context(), &pb.CompleteUploadRequest{ActorAssertion: actor, UploadSessionId: body.UploadSessionID, AssetId: r.PathValue("assetId")})
	if err != nil {
		assetError(w, err)
		return
	}
	jsonResponse(w, 200, assetJSON(response.Asset))
}
func (h *Handler) abortAsset(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.assetActor(w, r, contract.AbortAssetUpload)
	if !ok {
		return
	}
	response, err := h.options.Asset.AbortUpload(r.Context(), &pb.AbortUploadRequest{ActorAssertion: actor, UploadSessionId: r.PathValue("uploadSessionId")})
	if err != nil {
		assetError(w, err)
		return
	}
	_ = response
	w.WriteHeader(204)
}
func (h *Handler) getAssetUploadSession(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.assetActor(w, r, contract.GetAssetUploadSession)
	if !ok {
		return
	}
	response, err := h.options.Asset.GetUploadSession(r.Context(), &pb.GetUploadSessionRequest{ActorAssertion: actor, UploadSessionId: r.PathValue("uploadSessionId")})
	if err != nil {
		assetError(w, err)
		return
	}
	jsonResponse(w, 200, uploadSessionJSON(response.UploadSession))
}
func (h *Handler) getAssetMetadata(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.assetActor(w, r, contract.GetAssetMetadata)
	if !ok {
		return
	}
	response, err := h.options.Asset.GetAssetMetadata(r.Context(), &pb.GetAssetMetadataRequest{ActorAssertion: actor, AssetId: r.PathValue("assetId")})
	if err != nil {
		assetError(w, err)
		return
	}
	jsonResponse(w, 200, assetJSON(response.Asset))
}
func (h *Handler) listMyAssets(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.assetActor(w, r, contract.ListMyAssets)
	if !ok {
		return
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(query["limit"]) > 1 || len(query["cursor"]) > 1 || len(query) > 2 {
		problem(w, 400, "invalid_request")
		return
	}
	limit := int32(20)
	if query.Has("limit") {
		n, e := strconv.Atoi(query.Get("limit"))
		if e != nil || n < 1 || n > 100 {
			problem(w, 400, "invalid_request")
			return
		}
		limit = int32(n)
	}
	response, err := h.options.Asset.ListMyAssets(r.Context(), &pb.ListMyAssetsRequest{ActorAssertion: actor, Limit: limit, Cursor: query.Get("cursor")})
	if err != nil {
		assetError(w, err)
		return
	}
	items := make([]map[string]any, 0, len(response.Items))
	for _, item := range response.Items {
		items = append(items, assetJSON(item))
	}
	var cursor any
	if response.NextCursor != "" {
		cursor = response.NextCursor
	}
	jsonResponse(w, 200, map[string]any{"items": items, "nextCursor": cursor})
}
func (h *Handler) downloadAsset(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.assetActor(w, r, contract.GetAssetMetadata)
	if !ok {
		return
	}
	metadata, err := h.options.Asset.GetAssetMetadata(r.Context(), &pb.GetAssetMetadataRequest{ActorAssertion: actor, AssetId: r.PathValue("assetId")})
	if err != nil {
		assetError(w, err)
		return
	}
	if metadata.Asset.State != pb.AssetState_ASSET_STATE_READY {
		assetError(w, status.Error(codes.FailedPrecondition, "asset_not_ready"))
		return
	}
	downloadActor, ok := h.assetActor(w, r, contract.DownloadAsset)
	if !ok {
		return
	}
	stream, err := h.options.Asset.DownloadAsset(r.Context(), &pb.DownloadAssetRequest{ActorAssertion: downloadActor, AssetId: r.PathValue("assetId")})
	if err != nil {
		assetError(w, err)
		return
	}
	w.Header().Set("Content-Type", metadata.Asset.MediaType)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": metadata.Asset.OriginalFilename}))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	for {
		chunk, recvErr := stream.Recv()
		if errors.Is(recvErr, io.EOF) {
			return
		}
		if recvErr != nil {
			assetError(w, recvErr)
			return
		}
		if _, err = w.Write(chunk.Data); err != nil {
			return
		}
	}
}
