package asset

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	pb "github.com/KDZZZZZZ/human-worth/backend/gen/humanworth/asset/v1"
	identitypb "github.com/KDZZZZZZ/human-worth/backend/gen/humanworth/identity/v1"
	"github.com/KDZZZZZZ/human-worth/backend/internal/platform"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	maxAssetBytes    int64 = 32 << 20
	maxChunkBytes          = 256 << 10
	maxFilenameBytes       = 255
	maxAssetCursor         = 2048
	assetPageLimit         = 100
	assetTTL               = 24 * time.Hour
)

var (
	keyPattern     = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)
	uploadPattern  = regexp.MustCompile(`^upl_[a-f0-9]{32}$`)
	assetPattern   = regexp.MustCompile(`^ast_[a-f0-9]{32}$`)
	attemptPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)
)

type Server struct {
	pb.UnimplementedAssetServiceServer
	DB       *pgxpool.Pool
	Identity identitypb.IdentityServiceClient
	Storage  Storage
}

func invalid(reason string) error { return status.Error(codes.InvalidArgument, reason) }
func unavailable(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return status.Error(codes.DeadlineExceeded, "deadline_exceeded")
	}
	if errors.Is(err, context.Canceled) {
		return status.Error(codes.Canceled, "request_cancelled")
	}
	return status.Error(codes.Unavailable, "asset_unavailable")
}
func storageReady(s *Server) error {
	if s.Storage == nil {
		return status.Error(codes.Unavailable, "storage_unavailable")
	}
	return nil
}
func assetPeer(ctx context.Context, method string) error {
	name, err := platform.ServiceName(ctx)
	if err != nil {
		return status.Error(codes.Unauthenticated, "service_identity_required")
	}
	switch name {
	case "gateway":
		return nil
	case "content":
		if method == pb.AssetService_CheckAssetOwnership_FullMethodName {
			return nil
		}
	case "challenge":
		if method == pb.AssetService_GetAsset_FullMethodName || method == pb.AssetService_ReadAsset_FullMethodName || method == pb.AssetService_UploadAsset_FullMethodName {
			return nil
		}
	}
	return status.Error(codes.PermissionDenied, "service_forbidden")
}
func (s *Server) actor(ctx context.Context, assertion, method string) (string, error) {
	if err := assetPeer(ctx, method); err != nil {
		return "", err
	}
	if s.Identity == nil {
		return "", status.Error(codes.Unavailable, "identity_unavailable")
	}
	if values := metadata.ValueFromIncomingContext(ctx, "x-request-id"); len(values) == 1 {
		ctx = metadata.AppendToOutgoingContext(ctx, "x-request-id", values[0])
	}
	response, err := s.Identity.VerifyActor(ctx, &identitypb.VerifyActorRequest{ActorAssertion: assertion, FullMethod: method})
	if err != nil {
		return "", err
	}
	principal := response.GetPrincipal()
	if principal.GetClientKind() != identitypb.ClientKind_CLIENT_KIND_WEB_SESSION || principal.GetAccountId() == "" {
		return "", status.Error(codes.PermissionDenied, "web_session_required")
	}
	return principal.AccountId, nil
}

func Authorization(ctx context.Context, request any, info *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
	if err := assetPeer(ctx, info.FullMethod); err != nil {
		return nil, err
	}
	return next(ctx, request)
}
func AuthorizationStream(service any, stream grpc.ServerStream, info *grpc.StreamServerInfo, next grpc.StreamHandler) error {
	if err := assetPeer(stream.Context(), info.FullMethod); err != nil {
		return err
	}
	return next(service, stream)
}

func cleanFilename(value string) (string, error) {
	if !utf8.ValidString(value) || value == "" || len(value) > maxFilenameBytes {
		return "", invalid("invalid_filename")
	}
	value = strings.ReplaceAll(value, "\\", "/")
	if i := strings.LastIndexByte(value, '/'); i >= 0 {
		value = value[i+1:]
	}
	var b strings.Builder
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			continue
		}
		b.WriteRune(r)
	}
	value = strings.TrimSpace(b.String())
	if value == "" || value == "." || value == ".." || len(value) > maxFilenameBytes {
		return "", invalid("invalid_filename")
	}
	return value, nil
}

var allowedTypes = map[string]bool{"image/png": true, "image/jpeg": true, "image/webp": true, "application/pdf": true, "text/plain": true}

func mediaType(value string) (string, error) {
	parsed, _, err := mimeParse(value)
	if err != nil || !allowedTypes[parsed] {
		return "", invalid("unsupported_media_type")
	}
	return parsed, nil
}
func mimeParse(value string) (string, map[string]string, error) {
	parsed, params, err := mime.ParseMediaType(value)
	return strings.ToLower(parsed), params, err
}

func fingerprint(r *pb.CreateUploadSessionRequest, filename, declared string) []byte {
	h := sha256.New()
	fmt.Fprintf(h, "%d\x00%s\x00%s\x00%s", r.ExpectedBytes, r.ExpectedSha256, filename, declared)
	return h.Sum(nil)
}
func newID(prefix string) (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(raw[:]), nil
}

type assetScan interface{ Scan(...any) error }
type assetRow struct {
	ID, Owner, RunID, AttemptID, UploadID, State, Filename, Declared, Detected, StagingKey, FinalKey string
	Expected, Received                                                                               int64
	ExpectedHash, ActualHash                                                                         []byte
	Created, Completed                                                                               *time.Time
}

func scanAsset(row assetScan) (assetRow, error) {
	var result assetRow
	err := row.Scan(&result.ID, &result.Owner, &result.RunID, &result.AttemptID, &result.UploadID, &result.State, &result.Filename, &result.Declared, &result.Detected, &result.Expected, &result.Received, &result.ExpectedHash, &result.ActualHash, &result.StagingKey, &result.FinalKey, &result.Created, &result.Completed)
	return result, err
}
func assetContract(row assetRow) *pb.Asset {
	return &pb.Asset{Id: row.ID, Filename: row.Filename, MediaType: row.Detected, Bytes: row.Expected, Sha256: hex.EncodeToString(row.ActualHash), RunId: row.RunID, AttemptId: row.AttemptID}
}

func validateAccess(access *pb.Access) error {
	if access == nil || access.GetTaskId() == "" || access.GetRunId() == "" || access.GetAttemptId() == "" || access.GetPurpose() == "" {
		return invalid("invalid_access")
	}
	return nil
}

func assetMetadata(row assetRow) *pb.AssetMetadata {
	result := &pb.AssetMetadata{Id: row.ID, OwnerId: row.Owner, OriginalFilename: row.Filename, MediaType: row.Detected, Bytes: row.Expected, Sha256: hex.EncodeToString(row.ActualHash), State: assetState(row.State), CreatedAt: timestamppb.New(*row.Created)}
	if row.Detected == "" {
		result.MediaType = row.Declared
	}
	if row.Completed != nil {
		result.CompletedAt = timestamppb.New(*row.Completed)
	}
	return result
}
func assetState(value string) pb.AssetState {
	switch value {
	case "staging":
		return pb.AssetState_ASSET_STATE_STAGING
	case "ready":
		return pb.AssetState_ASSET_STATE_READY
	case "failed":
		return pb.AssetState_ASSET_STATE_FAILED
	case "deleted":
		return pb.AssetState_ASSET_STATE_DELETED
	}
	return pb.AssetState_ASSET_STATE_UNSPECIFIED
}
func sessionState(value string) pb.UploadSessionState {
	switch value {
	case "active":
		return pb.UploadSessionState_UPLOAD_SESSION_STATE_ACTIVE
	case "completed":
		return pb.UploadSessionState_UPLOAD_SESSION_STATE_COMPLETED
	case "aborted":
		return pb.UploadSessionState_UPLOAD_SESSION_STATE_ABORTED
	case "expired":
		return pb.UploadSessionState_UPLOAD_SESSION_STATE_EXPIRED
	}
	return pb.UploadSessionState_UPLOAD_SESSION_STATE_UNSPECIFIED
}

const assetSelect = `SELECT a.id,a.owner_id,a.run_id,a.attempt_id,a.upload_id,a.state,a.original_filename,a.declared_media_type,a.detected_media_type,a.expected_bytes,a.received_bytes,a.expected_sha256,a.actual_sha256,a.staging_key,a.final_key,a.created_at,a.completed_at`

func (s *Server) CreateUploadSession(ctx context.Context, r *pb.CreateUploadSessionRequest) (*pb.CreateUploadSessionResponse, error) {
	owner, err := s.actor(ctx, r.GetActorAssertion(), pb.AssetService_CreateUploadSession_FullMethodName)
	if err != nil {
		return nil, err
	}
	if err = storageReady(s); err != nil {
		return nil, err
	}
	if !keyPattern.MatchString(r.GetIdempotencyKey()) {
		return nil, invalid("invalid_idempotency_key")
	}
	filename, err := cleanFilename(r.GetOriginalFilename())
	if err != nil {
		return nil, err
	}
	declared, err := mediaType(r.GetDeclaredMediaType())
	if err != nil {
		return nil, err
	}
	if r.ExpectedBytes < 1 || r.ExpectedBytes > maxAssetBytes {
		return nil, invalid("invalid_file_size")
	}
	hash, err := hex.DecodeString(r.GetExpectedSha256())
	if err != nil || len(hash) != sha256.Size {
		return nil, invalid("invalid_sha256")
	}
	requestHash := fingerprint(r, filename, declared)
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return nil, unavailable(err)
	}
	defer tx.Rollback(ctx)
	var id, assetID, state string
	var existingHash []byte
	var expires time.Time
	err = tx.QueryRow(ctx, `SELECT id,asset_id,state,request_fingerprint,expires_at FROM asset.upload_sessions WHERE owner_id=$1 AND idempotency_key=$2`, owner, r.IdempotencyKey).Scan(&id, &assetID, &state, &existingHash, &expires)
	if err == nil {
		if !bytesEqual(existingHash, requestHash) {
			return nil, status.Error(codes.AlreadyExists, "idempotency_key_conflict")
		}
		row, scanErr := scanAsset(tx.QueryRow(ctx, assetSelect+` FROM asset.assets WHERE id=$1`, assetID))
		if scanErr != nil {
			return nil, unavailable(scanErr)
		}
		if err = tx.Commit(ctx); err != nil {
			return nil, unavailable(err)
		}
		return &pb.CreateUploadSessionResponse{Asset: assetMetadata(row), UploadSession: &pb.UploadSession{Id: id, AssetId: assetID, ReceivedBytes: row.Received, ExpectedBytes: row.Expected, State: sessionState(state), ExpiresAt: timestamppb.New(expires)}}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, unavailable(err)
	}
	id, err = newID("upl_")
	if err != nil {
		return nil, status.Error(codes.Internal, "internal_error")
	}
	assetID, err = newID("ast_")
	if err != nil {
		return nil, status.Error(codes.Internal, "internal_error")
	}
	staging := "assets/" + assetID + "/staging/" + id
	final := "assets/" + assetID + "/content"
	expires = time.Now().UTC().Add(assetTTL)
	_, err = tx.Exec(ctx, `INSERT INTO asset.assets(id,owner_id,state,original_filename,declared_media_type,expected_bytes,expected_sha256,staging_key,final_key) VALUES($1,$2,'staging',$3,$4,$5,$6,$7,$8)`, assetID, owner, filename, declared, r.ExpectedBytes, hash, staging, final)
	if err != nil {
		return nil, unavailable(err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO asset.upload_sessions(id,asset_id,owner_id,idempotency_key,request_fingerprint,state,expires_at) VALUES($1,$2,$3,$4,$5,'active',$6)`, id, assetID, owner, r.IdempotencyKey, requestHash, expires)
	if err != nil {
		return nil, unavailable(err)
	}
	row := assetRow{ID: assetID, Owner: owner, State: "staging", Filename: filename, Declared: declared, Expected: r.ExpectedBytes, Created: ptrTime(time.Now().UTC())}
	if err = tx.Commit(ctx); err != nil {
		return nil, unavailable(err)
	}
	return &pb.CreateUploadSessionResponse{Asset: assetMetadata(row), UploadSession: &pb.UploadSession{Id: id, AssetId: assetID, ExpectedBytes: r.ExpectedBytes, State: pb.UploadSessionState_UPLOAD_SESSION_STATE_ACTIVE, ExpiresAt: timestamppb.New(expires)}}, nil
}

func ptrTime(value time.Time) *time.Time { return &value }
func bytesEqual(a, b []byte) bool {
	return len(a) == len(b) && subtle.ConstantTimeCompare(a, b) == 1
}
func (s *Server) UploadAsset(stream pb.AssetService_UploadAssetServer) error {
	first, err := stream.Recv()
	if err != nil {
		return invalid("upload_metadata_required")
	}
	if first.GetActorAssertion() != "" || first.GetUploadSessionId() != "" {
		return s.uploadBrowser(stream, first)
	}
	return s.uploadChallenge(stream, first)
}

func (s *Server) uploadBrowser(stream pb.AssetService_UploadAssetServer, first *pb.UploadAssetRequest) error {
	if !uploadPattern.MatchString(first.GetUploadSessionId()) || !attemptPattern.MatchString(first.GetUploadAttemptId()) || first.GetOffset() < 0 || first.GetActorAssertion() == "" {
		return invalid("invalid_upload_start")
	}
	owner, err := s.actor(stream.Context(), first.GetActorAssertion(), pb.AssetService_UploadAsset_FullMethodName)
	if err != nil {
		return err
	}
	if err = storageReady(s); err != nil {
		return err
	}
	var next int64 = first.GetOffset()
	for {
		message, recvErr := stream.Recv()
		if errors.Is(recvErr, io.EOF) {
			return stream.SendAndClose(&pb.UploadAssetResponse{UploadSessionId: first.GetUploadSessionId(), ReceivedBytes: next, NextOffset: next, State: pb.UploadSessionState_UPLOAD_SESSION_STATE_ACTIVE})
		}
		if recvErr != nil {
			return unavailable(recvErr)
		}
		if len(message.GetChunk()) == 0 || len(message.GetChunk()) > maxChunkBytes || message.GetOffset() != next {
			return invalid("invalid_upload_chunk")
		}
		if message.GetChunkSha256() != "" {
			sum := sha256.Sum256(message.GetChunk())
			if !strings.EqualFold(hex.EncodeToString(sum[:]), message.GetChunkSha256()) {
				return status.Error(codes.FailedPrecondition, "chunk_checksum_mismatch")
			}
		}
		tx, txErr := s.DB.Begin(stream.Context())
		if txErr != nil {
			return unavailable(txErr)
		}
		var assetID, state, key, sessionOwner string
		var expected, received int64
		var expires time.Time
		txErr = tx.QueryRow(stream.Context(), `SELECT s.asset_id,s.owner_id,s.state,s.expires_at,a.staging_key,a.expected_bytes,s.received_bytes FROM asset.upload_sessions s JOIN asset.assets a ON a.id=s.asset_id WHERE s.id=$1 FOR UPDATE`, first.GetUploadSessionId()).Scan(&assetID, &sessionOwner, &state, &expires, &key, &expected, &received)
		if errors.Is(txErr, pgx.ErrNoRows) {
			tx.Rollback(stream.Context())
			return status.Error(codes.NotFound, "upload_session_not_found")
		}
		if txErr != nil {
			tx.Rollback(stream.Context())
			return unavailable(txErr)
		}
		if sessionOwner != owner {
			tx.Rollback(stream.Context())
			return status.Error(codes.NotFound, "upload_session_not_found")
		}
		if state != "active" || time.Now().After(expires) {
			tx.Rollback(stream.Context())
			return status.Error(codes.FailedPrecondition, "upload_session_not_active")
		}
		if received != next {
			tx.Rollback(stream.Context())
			return status.Error(codes.Aborted, "upload_offset_conflict")
		}
		if received+int64(len(message.GetChunk())) > expected {
			tx.Rollback(stream.Context())
			return invalid("upload_too_large")
		}
		if txErr = s.Storage.WriteAt(stream.Context(), key, received, message.GetChunk()); txErr != nil {
			tx.Rollback(stream.Context())
			return unavailable(txErr)
		}
		_, txErr = tx.Exec(stream.Context(), `UPDATE asset.assets SET received_bytes=$2,updated_at=clock_timestamp() WHERE id=$1`, assetID, received+int64(len(message.GetChunk())))
		if txErr == nil {
			_, txErr = tx.Exec(stream.Context(), `UPDATE asset.upload_sessions SET received_bytes=$2,updated_at=clock_timestamp() WHERE id=$1`, first.GetUploadSessionId(), received+int64(len(message.GetChunk())))
		}
		if txErr == nil {
			txErr = tx.Commit(stream.Context())
		} else {
			tx.Rollback(stream.Context())
		}
		if txErr != nil {
			return unavailable(txErr)
		}
		next = received + int64(len(message.GetChunk()))
	}
}

var serviceUploadPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func (s *Server) uploadChallenge(stream pb.AssetService_UploadAssetServer, first *pb.UploadAssetRequest) error {
	if err := assetPeer(stream.Context(), pb.AssetService_UploadAsset_FullMethodName); err != nil {
		return err
	}
	if err := storageReady(s); err != nil {
		return err
	}
	if err := validateAccess(first.GetAccess()); err != nil || !serviceUploadPattern.MatchString(first.GetUploadId()) {
		return invalid("invalid_upload")
	}
	filename, err := cleanFilename(first.GetFilename())
	if err != nil {
		return err
	}
	declared, err := mediaType(first.GetMediaType())
	if err != nil {
		return err
	}
	if first.GetBytes() < 1 || first.GetBytes() > maxAssetBytes || len(first.GetSha256()) != sha256.Size*2 {
		return invalid("invalid_upload")
	}
	expectedHash, err := hex.DecodeString(first.GetSha256())
	if err != nil {
		return invalid("invalid_upload")
	}
	access := first.GetAccess()
	ctx := stream.Context()
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return unavailable(err)
	}
	defer tx.Rollback(ctx)
	var existing assetRow
	existing, err = scanAsset(tx.QueryRow(ctx, assetSelect+` FROM asset.assets WHERE run_id=$1 AND attempt_id=$2 AND upload_id=$3`, access.GetRunId(), access.GetAttemptId(), first.GetUploadId()))
	if err == nil {
		if existing.State != "ready" || existing.Expected != first.GetBytes() || !bytesEqual(existing.ExpectedHash, expectedHash) {
			return status.Error(codes.Aborted, "upload_conflict")
		}
		if err = tx.Commit(ctx); err != nil {
			return unavailable(err)
		}
		return stream.SendAndClose(&pb.UploadAssetResponse{Asset: assetContract(existing)})
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return unavailable(err)
	}
	assetID, err := newID("ast_")
	if err != nil {
		return status.Error(codes.Internal, "internal_error")
	}
	staging := "assets/" + assetID + "/challenge/" + first.GetUploadId()
	final := "assets/" + assetID + "/content"
	_, err = tx.Exec(ctx, `INSERT INTO asset.assets(id,owner_id,run_id,attempt_id,upload_id,state,original_filename,declared_media_type,expected_bytes,expected_sha256,staging_key,final_key) VALUES($1,'challenge',$2,$3,$4,'staging',$5,$6,$7,$8,$9,$10)`, assetID, access.GetRunId(), access.GetAttemptId(), first.GetUploadId(), filename, declared, first.GetBytes(), expectedHash, staging, final)
	if err != nil {
		return unavailable(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return unavailable(err)
	}
	digest := sha256.New()
	var received int64
	current := first
	for {
		if len(current.GetChunk()) > maxChunkBytes || received+int64(len(current.GetChunk())) > first.GetBytes() {
			return invalid("invalid_upload_size")
		}
		if current != first && (current.GetAccess() != nil || current.GetUploadId() != "" || current.GetFilename() != "" || current.GetMediaType() != "" || current.GetBytes() != 0 || current.GetSha256() != "") {
			return invalid("duplicate_upload_metadata")
		}
		if len(current.GetChunk()) > 0 {
			if err = s.Storage.WriteAt(ctx, staging, received, current.GetChunk()); err != nil {
				return unavailable(err)
			}
			_, _ = digest.Write(current.GetChunk())
			received += int64(len(current.GetChunk()))
		}
		next, recvErr := stream.Recv()
		if errors.Is(recvErr, io.EOF) {
			break
		}
		if recvErr != nil {
			return unavailable(recvErr)
		}
		current = next
	}
	if received != first.GetBytes() || !strings.EqualFold(hex.EncodeToString(digest.Sum(nil)), first.GetSha256()) {
		return status.Error(codes.FailedPrecondition, "upload_digest_mismatch")
	}
	if err = s.Storage.Move(ctx, staging, final); err != nil {
		return unavailable(err)
	}
	_, err = s.DB.Exec(ctx, `UPDATE asset.assets SET state='ready',detected_media_type=$2,received_bytes=$3,actual_sha256=$4,completed_at=clock_timestamp(),updated_at=clock_timestamp() WHERE id=$1`, assetID, declared, received, digest.Sum(nil))
	if err != nil {
		return unavailable(err)
	}
	row, err := scanAsset(s.DB.QueryRow(ctx, assetSelect+` FROM asset.assets WHERE id=$1`, assetID))
	if err != nil {
		return unavailable(err)
	}
	return stream.SendAndClose(&pb.UploadAssetResponse{Asset: assetContract(row)})
}

func (s *Server) CompleteUpload(ctx context.Context, r *pb.CompleteUploadRequest) (*pb.CompleteUploadResponse, error) {
	owner, err := s.actor(ctx, r.GetActorAssertion(), pb.AssetService_CompleteUpload_FullMethodName)
	if err != nil {
		return nil, err
	}
	if err = storageReady(s); err != nil {
		return nil, err
	}
	if !uploadPattern.MatchString(r.GetUploadSessionId()) {
		return nil, status.Error(codes.NotFound, "upload_session_not_found")
	}
	if r.GetAssetId() != "" && !assetPattern.MatchString(r.GetAssetId()) {
		return nil, status.Error(codes.NotFound, "asset_not_found")
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return nil, unavailable(err)
	}
	defer tx.Rollback(ctx)
	var sessionOwner, state, staging, final, declared string
	var assetID, assetState string
	var expected, received int64
	var expectedHash []byte
	var expires time.Time
	err = tx.QueryRow(ctx, `SELECT s.owner_id,s.state,s.expires_at,s.asset_id,a.state,a.staging_key,a.final_key,a.declared_media_type,a.expected_bytes,a.received_bytes,a.expected_sha256 FROM asset.upload_sessions s JOIN asset.assets a ON a.id=s.asset_id WHERE s.id=$1 FOR UPDATE`, r.UploadSessionId).Scan(&sessionOwner, &state, &expires, &assetID, &assetState, &staging, &final, &declared, &expected, &received, &expectedHash)
	if errors.Is(err, pgx.ErrNoRows) || sessionOwner != owner {
		return nil, status.Error(codes.NotFound, "upload_session_not_found")
	}
	if err != nil {
		return nil, unavailable(err)
	}
	if r.GetAssetId() != "" && r.GetAssetId() != assetID {
		return nil, status.Error(codes.NotFound, "asset_not_found")
	}
	if state == "completed" && assetState == "ready" {
		row, scanErr := scanAsset(tx.QueryRow(ctx, assetSelect+` FROM asset.assets WHERE id=$1`, assetID))
		if scanErr != nil {
			return nil, unavailable(scanErr)
		}
		return &pb.CompleteUploadResponse{Asset: assetMetadata(row)}, tx.Commit(ctx)
	}
	if state != "active" {
		return nil, status.Error(codes.FailedPrecondition, "upload_session_not_active")
	}
	if time.Now().After(expires) {
		_, _ = tx.Exec(ctx, `UPDATE asset.upload_sessions SET state='expired',updated_at=clock_timestamp() WHERE id=$1`, r.UploadSessionId)
		_ = tx.Commit(ctx)
		return nil, status.Error(codes.FailedPrecondition, "upload_session_expired")
	}
	if received != expected {
		return nil, status.Error(codes.FailedPrecondition, "upload_incomplete")
	}
	key := staging
	if size, statErr := s.Storage.Stat(ctx, staging); statErr != nil || size != expected {
		if finalSize, finalErr := s.Storage.Stat(ctx, final); finalErr == nil && finalSize == expected {
			key = final
		} else {
			return nil, status.Error(codes.FailedPrecondition, "upload_incomplete")
		}
	}
	reader, err := s.Storage.Open(ctx, key)
	if err != nil {
		return nil, unavailable(err)
	}
	hash := sha256.New()
	var sniff [512]byte
	n, readErr := io.ReadFull(reader, sniff[:])
	if readErr != nil && !errors.Is(readErr, io.ErrUnexpectedEOF) && !errors.Is(readErr, io.EOF) {
		reader.Close()
		return nil, unavailable(readErr)
	}
	if n == 0 {
		reader.Close()
		return nil, status.Error(codes.FailedPrecondition, "upload_incomplete")
	}
	if _, err = hash.Write(sniff[:n]); err == nil {
		_, err = io.Copy(hash, reader)
	}
	closeErr := reader.Close()
	if err != nil || closeErr != nil {
		return nil, unavailable(firstErr(err, closeErr))
	}
	actual := hash.Sum(nil)
	if !bytesEqual(actual, expectedHash) {
		_, _ = tx.Exec(ctx, `UPDATE asset.assets SET state='failed',updated_at=clock_timestamp() WHERE id=$1`, assetID)
		_, _ = tx.Exec(ctx, `UPDATE asset.upload_sessions SET state='aborted',updated_at=clock_timestamp() WHERE id=$1`, r.UploadSessionId)
		_ = tx.Commit(ctx)
		_ = s.Storage.Delete(ctx, staging)
		return nil, status.Error(codes.FailedPrecondition, "sha256_mismatch")
	}
	detected, _, _ := mimeParse(http.DetectContentType(sniff[:n]))
	if !allowedTypes[detected] {
		_, _ = tx.Exec(ctx, `UPDATE asset.assets SET state='failed',detected_media_type=$2,updated_at=clock_timestamp() WHERE id=$1`, assetID, detected)
		_, _ = tx.Exec(ctx, `UPDATE asset.upload_sessions SET state='aborted',updated_at=clock_timestamp() WHERE id=$1`, r.UploadSessionId)
		_ = tx.Commit(ctx)
		_ = s.Storage.Delete(ctx, staging)
		return nil, invalid("unsupported_media_type")
	}
	if detected != declared {
		_, _ = tx.Exec(ctx, `UPDATE asset.assets SET state='failed',detected_media_type=$2,updated_at=clock_timestamp() WHERE id=$1`, assetID, detected)
		_, _ = tx.Exec(ctx, `UPDATE asset.upload_sessions SET state='aborted',updated_at=clock_timestamp() WHERE id=$1`, r.UploadSessionId)
		_ = tx.Commit(ctx)
		_ = s.Storage.Delete(ctx, staging)
		return nil, status.Error(codes.FailedPrecondition, "media_type_mismatch")
	}
	if err = s.Storage.Move(ctx, staging, final); err != nil && key == staging {
		return nil, unavailable(err)
	}
	_, err = tx.Exec(ctx, `UPDATE asset.assets SET state='ready',detected_media_type=$2,actual_sha256=$3,completed_at=clock_timestamp(),updated_at=clock_timestamp() WHERE id=$1`, assetID, detected, actual)
	if err == nil {
		_, err = tx.Exec(ctx, `UPDATE asset.upload_sessions SET state='completed',updated_at=clock_timestamp() WHERE id=$1`, r.UploadSessionId)
	}
	if err != nil {
		return nil, unavailable(err)
	}
	row, err := scanAsset(tx.QueryRow(ctx, assetSelect+` FROM asset.assets WHERE id=$1`, assetID))
	if err != nil {
		return nil, unavailable(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, unavailable(err)
	}
	return &pb.CompleteUploadResponse{Asset: assetMetadata(row)}, nil
}

func firstErr(a, b error) error {
	if a != nil {
		return a
	}
	return b
}

func (s *Server) AbortUpload(ctx context.Context, r *pb.AbortUploadRequest) (*pb.AbortUploadResponse, error) {
	owner, err := s.actor(ctx, r.GetActorAssertion(), pb.AssetService_AbortUpload_FullMethodName)
	if err != nil {
		return nil, err
	}
	if err = storageReady(s); err != nil {
		return nil, err
	}
	if !uploadPattern.MatchString(r.GetUploadSessionId()) {
		return nil, status.Error(codes.NotFound, "upload_session_not_found")
	}
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return nil, unavailable(err)
	}
	defer tx.Rollback(ctx)
	var assetID, sessionOwner, state, key string
	var received, expected int64
	var expires time.Time
	err = tx.QueryRow(ctx, `SELECT s.asset_id,s.owner_id,s.state,s.expires_at,a.staging_key,a.received_bytes,a.expected_bytes FROM asset.upload_sessions s JOIN asset.assets a ON a.id=s.asset_id WHERE s.id=$1 FOR UPDATE`, r.UploadSessionId).Scan(&assetID, &sessionOwner, &state, &expires, &key, &received, &expected)
	if errors.Is(err, pgx.ErrNoRows) || sessionOwner != owner {
		return nil, status.Error(codes.NotFound, "upload_session_not_found")
	}
	if err != nil {
		return nil, unavailable(err)
	}
	if state == "completed" {
		return nil, status.Error(codes.FailedPrecondition, "upload_already_completed")
	}
	_, err = tx.Exec(ctx, `UPDATE asset.upload_sessions SET state='aborted',updated_at=clock_timestamp() WHERE id=$1`, r.UploadSessionId)
	if err == nil {
		_, err = tx.Exec(ctx, `UPDATE asset.assets SET state='failed',updated_at=clock_timestamp() WHERE id=$1`, assetID)
	}
	if err != nil {
		return nil, unavailable(err)
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, unavailable(err)
	}
	_ = s.Storage.Delete(ctx, key)
	return &pb.AbortUploadResponse{UploadSession: &pb.UploadSession{Id: r.UploadSessionId, AssetId: assetID, ReceivedBytes: received, ExpectedBytes: expected, State: pb.UploadSessionState_UPLOAD_SESSION_STATE_ABORTED, ExpiresAt: timestamppb.New(expires)}}, nil
}

func (s *Server) GetUploadSession(ctx context.Context, r *pb.GetUploadSessionRequest) (*pb.GetUploadSessionResponse, error) {
	owner, err := s.actor(ctx, r.GetActorAssertion(), pb.AssetService_GetUploadSession_FullMethodName)
	if err != nil {
		return nil, err
	}
	if !uploadPattern.MatchString(r.GetUploadSessionId()) {
		return nil, status.Error(codes.NotFound, "upload_session_not_found")
	}
	var id, assetID, rowOwner, state string
	var received, expected int64
	var expires time.Time
	err = s.DB.QueryRow(ctx, `SELECT s.id,s.asset_id,s.owner_id,s.state,s.received_bytes,a.expected_bytes,s.expires_at FROM asset.upload_sessions s JOIN asset.assets a ON a.id=s.asset_id WHERE s.id=$1 AND s.owner_id=$2`, r.UploadSessionId, owner).Scan(&id, &assetID, &rowOwner, &state, &received, &expected, &expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, status.Error(codes.NotFound, "upload_session_not_found")
	}
	if err != nil {
		return nil, unavailable(err)
	}
	return &pb.GetUploadSessionResponse{UploadSession: &pb.UploadSession{Id: id, AssetId: assetID, ReceivedBytes: received, ExpectedBytes: expected, State: sessionState(state), ExpiresAt: timestamppb.New(expires)}}, nil
}

func (s *Server) GetAssetMetadata(ctx context.Context, r *pb.GetAssetMetadataRequest) (*pb.GetAssetMetadataResponse, error) {
	owner, err := s.actor(ctx, r.GetActorAssertion(), pb.AssetService_GetAssetMetadata_FullMethodName)
	if err != nil {
		return nil, err
	}
	if !assetPattern.MatchString(r.GetAssetId()) {
		return nil, status.Error(codes.NotFound, "asset_not_found")
	}
	row, err := scanAsset(s.DB.QueryRow(ctx, assetSelect+` FROM asset.assets WHERE id=$1 AND owner_id=$2`, r.AssetId, owner))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, status.Error(codes.NotFound, "asset_not_found")
	}
	if err != nil {
		return nil, unavailable(err)
	}
	return &pb.GetAssetMetadataResponse{Asset: assetMetadata(row)}, nil
}

type assetCursor struct {
	V                  int `json:"v"`
	Scope, Created, ID string
}

func assetScope(owner string) string {
	sum := sha256.Sum256([]byte(owner + "|created_at_desc_id_desc"))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
func encodeAssetCursor(owner string, created time.Time, id string) string {
	data, _ := json.Marshal(assetCursor{1, assetScope(owner), created.UTC().Format(time.RFC3339Nano), id})
	return base64.RawURLEncoding.EncodeToString(data)
}
func decodeAssetCursor(raw, owner string) (time.Time, string, error) {
	if raw == "" {
		return time.Time{}, "", nil
	}
	if len(raw) > maxAssetCursor {
		return time.Time{}, "", invalid("invalid_cursor")
	}
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return time.Time{}, "", invalid("invalid_cursor")
	}
	var c assetCursor
	if json.Unmarshal(data, &c) != nil || c.V != 1 || c.Scope != assetScope(owner) || !assetPattern.MatchString(c.ID) {
		return time.Time{}, "", invalid("invalid_cursor")
	}
	t, err := time.Parse(time.RFC3339Nano, c.Created)
	if err != nil || c.Created != t.UTC().Format(time.RFC3339Nano) {
		return time.Time{}, "", invalid("invalid_cursor")
	}
	return t, c.ID, nil
}
func (s *Server) ListMyAssets(ctx context.Context, r *pb.ListMyAssetsRequest) (*pb.ListMyAssetsResponse, error) {
	owner, err := s.actor(ctx, r.GetActorAssertion(), pb.AssetService_ListMyAssets_FullMethodName)
	if err != nil {
		return nil, err
	}
	limit := r.Limit
	if limit == 0 {
		limit = 20
	}
	if limit < 1 || limit > assetPageLimit {
		return nil, invalid("invalid_request")
	}
	created, cursorID, err := decodeAssetCursor(r.Cursor, owner)
	if err != nil {
		return nil, err
	}
	query := assetSelect + ` FROM asset.assets WHERE owner_id=$1 AND state='ready'`
	args := []any{owner}
	if r.Cursor != "" {
		args = append(args, created, cursorID)
		query += fmt.Sprintf(` AND (created_at,id)<($%d,$%d)`, len(args)-1, len(args))
	}
	args = append(args, limit+1)
	query += fmt.Sprintf(` ORDER BY created_at DESC,id DESC LIMIT $%d`, len(args))
	rows, err := s.DB.Query(ctx, query, args...)
	if err != nil {
		return nil, unavailable(err)
	}
	defer rows.Close()
	result := &pb.ListMyAssetsResponse{Items: make([]*pb.AssetMetadata, 0, limit)}
	var last time.Time
	var lastID string
	more := false
	for rows.Next() {
		row, scanErr := scanAsset(rows)
		if scanErr != nil {
			return nil, unavailable(scanErr)
		}
		if len(result.Items) == int(limit) {
			more = true
			break
		}
		result.Items = append(result.Items, assetMetadata(row))
		last = *row.Created
		lastID = row.ID
	}
	if err = rows.Err(); err != nil {
		return nil, unavailable(err)
	}
	if more {
		result.NextCursor = encodeAssetCursor(owner, last, lastID)
	}
	return result, nil
}

func (s *Server) CheckAssetOwnership(ctx context.Context, r *pb.CheckAssetOwnershipRequest) (*pb.CheckAssetOwnershipResponse, error) {
	owner, err := s.actor(ctx, r.GetActorAssertion(), pb.AssetService_CheckAssetOwnership_FullMethodName)
	if err != nil {
		return nil, err
	}
	if len(r.AssetIds) == 0 || len(r.AssetIds) > 32 {
		return nil, invalid("invalid_asset_ids")
	}
	seen := map[string]bool{}
	for _, id := range r.AssetIds {
		if !assetPattern.MatchString(id) || seen[id] {
			return nil, invalid("invalid_asset_ids")
		}
		seen[id] = true
	}
	rows, err := s.DB.Query(ctx, assetSelect+` FROM asset.assets WHERE owner_id=$1 AND state='ready' AND id=ANY($2)`, owner, r.AssetIds)
	if err != nil {
		return nil, unavailable(err)
	}
	defer rows.Close()
	items := make(map[string]*pb.AssetMetadata)
	for rows.Next() {
		row, scanErr := scanAsset(rows)
		if scanErr != nil {
			return nil, unavailable(scanErr)
		}
		items[row.ID] = assetMetadata(row)
	}
	if err = rows.Err(); err != nil {
		return nil, unavailable(err)
	}
	result := &pb.CheckAssetOwnershipResponse{Assets: make([]*pb.AssetMetadata, 0, len(r.AssetIds))}
	for _, id := range r.AssetIds {
		item := items[id]
		if item == nil {
			return nil, status.Error(codes.FailedPrecondition, "asset_not_ready")
		}
		result.Assets = append(result.Assets, item)
	}
	return result, nil
}

func (s *Server) GetAsset(ctx context.Context, r *pb.GetAssetRequest) (*pb.GetAssetResponse, error) {
	if err := assetPeer(ctx, pb.AssetService_GetAsset_FullMethodName); err != nil {
		return nil, err
	}
	if err := validateAccess(r.GetAccess()); err != nil || !assetPattern.MatchString(r.GetAssetId()) {
		return nil, status.Error(codes.NotFound, "asset_not_found")
	}
	row, err := scanAsset(s.DB.QueryRow(ctx, assetSelect+` FROM asset.assets WHERE id=$1 AND state='ready'`, r.GetAssetId()))
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && row.RunID != "" && (row.RunID != r.GetAccess().GetRunId() || row.AttemptID != r.GetAccess().GetAttemptId())) {
		return nil, status.Error(codes.NotFound, "asset_not_found")
	}
	if err != nil {
		return nil, unavailable(err)
	}
	return &pb.GetAssetResponse{Asset: assetContract(row)}, nil
}

func (s *Server) ReadAsset(r *pb.ReadAssetRequest, stream pb.AssetService_ReadAssetServer) error {
	if err := assetPeer(stream.Context(), pb.AssetService_ReadAsset_FullMethodName); err != nil {
		return err
	}
	if err := validateAccess(r.GetAccess()); err != nil || !assetPattern.MatchString(r.GetAssetId()) || r.GetOffset() < 0 {
		return status.Error(codes.NotFound, "asset_not_found")
	}
	row, err := scanAsset(s.DB.QueryRow(stream.Context(), assetSelect+` FROM asset.assets WHERE id=$1 AND state='ready'`, r.GetAssetId()))
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && row.RunID != "" && (row.RunID != r.GetAccess().GetRunId() || row.AttemptID != r.GetAccess().GetAttemptId())) {
		return status.Error(codes.NotFound, "asset_not_found")
	}
	if err != nil {
		return unavailable(err)
	}
	if err := storageReady(s); err != nil {
		return err
	}
	reader, err := s.Storage.Open(stream.Context(), row.FinalKey)
	if err != nil {
		return unavailable(err)
	}
	defer reader.Close()
	if r.GetOffset() > 0 {
		if _, err = io.CopyN(io.Discard, reader, r.GetOffset()); err != nil {
			return status.Error(codes.InvalidArgument, "invalid_offset")
		}
	}
	buf := make([]byte, 64<<10)
	offset := r.GetOffset()
	for {
		n, readErr := reader.Read(buf)
		if n > 0 {
			if err = stream.Send(&pb.ReadAssetResponse{Chunk: append([]byte(nil), buf[:n]...), Offset: offset, Sha256: hex.EncodeToString(row.ActualHash)}); err != nil {
				return unavailable(err)
			}
			offset += int64(n)
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return unavailable(readErr)
		}
	}
}

func (s *Server) DownloadAsset(r *pb.DownloadAssetRequest, stream pb.AssetService_DownloadAssetServer) error {
	owner, err := s.actor(stream.Context(), r.GetActorAssertion(), pb.AssetService_DownloadAsset_FullMethodName)
	if err != nil {
		return err
	}
	if err = storageReady(s); err != nil {
		return err
	}
	if !assetPattern.MatchString(r.GetAssetId()) {
		return status.Error(codes.NotFound, "asset_not_found")
	}
	row, err := scanAsset(s.DB.QueryRow(stream.Context(), assetSelect+` FROM asset.assets WHERE id=$1 AND owner_id=$2 AND state='ready'`, r.AssetId, owner))
	if errors.Is(err, pgx.ErrNoRows) {
		return status.Error(codes.NotFound, "asset_not_found")
	}
	if err != nil {
		return unavailable(err)
	}
	reader, err := s.Storage.Open(stream.Context(), row.FinalKey)
	if err != nil {
		return unavailable(err)
	}
	defer reader.Close()
	buf := make([]byte, 64<<10)
	var offset int64
	for {
		n, readErr := reader.Read(buf)
		if n > 0 {
			data := append([]byte(nil), buf[:n]...)
			if err = stream.Send(&pb.DownloadAssetResponse{Offset: offset, Data: data}); err != nil {
				return unavailable(err)
			}
			offset += int64(n)
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return unavailable(readErr)
		}
	}
}
