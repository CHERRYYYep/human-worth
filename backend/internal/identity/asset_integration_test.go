//go:build integration

package identity

import (
	"net/http"
	"testing"

	assetpb "github.com/KDZZZZZZ/human-worth/backend/gen/humanworth/asset/v1"
	pb "github.com/KDZZZZZZ/human-worth/backend/gen/humanworth/identity/v1"
	"google.golang.org/grpc/codes"
)

func TestAssetIdentityAuthorization(t *testing.T) {
	l := newIdentityLab(t)
	session, me := l.login(t, "asset-user", nil)
	headers := map[string]string{"Origin": l.origin, "X-CSRF-Token": me.CsrfToken, "Content-Type": "application/json"}
	created := l.request(t, 0, "POST", "/api/me/mcp-tokens", []*http.Cookie{session}, `{"name":"asset denied MCP","createRequestId":"asset-auth-check"}`, headers)
	httpStatus(t, created, http.StatusCreated)
	var token struct{ Token string }
	readJSON(t, created, &token)

	assetIdentity := l.client(t, 1, "asset", l.pki)
	contentIdentity := l.client(t, 1, "content", l.pki)
	methods := []struct {
		name   string
		method string
		write  bool
	}{
		{"create", assetpb.AssetService_CreateUploadSession_FullMethodName, true},
		{"upload", assetpb.AssetService_UploadAsset_FullMethodName, true},
		{"complete", assetpb.AssetService_CompleteUpload_FullMethodName, true},
		{"abort", assetpb.AssetService_AbortUpload_FullMethodName, true},
		{"get_session", assetpb.AssetService_GetUploadSession_FullMethodName, false},
		{"get_metadata", assetpb.AssetService_GetAssetMetadata_FullMethodName, false},
		{"list", assetpb.AssetService_ListMyAssets_FullMethodName, false},
		{"ownership", assetpb.AssetService_CheckAssetOwnership_FullMethodName, false},
		{"download", assetpb.AssetService_DownloadAsset_FullMethodName, false},
	}
	var revocable string
	for _, operation := range methods {
		t.Run(operation.name, func(t *testing.T) {
			request := &pb.ResolvePrincipalRequest{Credential: &pb.ResolvePrincipalRequest_SessionCookie{SessionCookie: session.Value}, Audience: "asset", FullMethod: operation.method}
			if operation.write {
				_, err := l.clients[0].ResolvePrincipal(t.Context(), request)
				grpcCode(t, err, codes.PermissionDenied)
				request.Origin, request.CsrfToken = l.origin, me.CsrfToken
			}
			actor, err := l.clients[0].ResolvePrincipal(t.Context(), request)
			must(t, err)
			verified, err := assetIdentity.VerifyActor(t.Context(), &pb.VerifyActorRequest{ActorAssertion: actor.ActorAssertion, FullMethod: operation.method})
			must(t, err)
			if verified.Principal.AccountId != me.Account.Id || verified.Principal.ClientKind != pb.ClientKind_CLIENT_KIND_WEB_SESSION {
				t.Fatal("Asset assertion changed the authenticated web account")
			}
			wrongMethod := assetpb.AssetService_ListMyAssets_FullMethodName
			if wrongMethod == operation.method {
				wrongMethod = assetpb.AssetService_GetAssetMetadata_FullMethodName
			}
			_, err = assetIdentity.VerifyActor(t.Context(), &pb.VerifyActorRequest{ActorAssertion: actor.ActorAssertion, FullMethod: wrongMethod})
			grpcCode(t, err, codes.Unauthenticated)
			_, err = contentIdentity.VerifyActor(t.Context(), &pb.VerifyActorRequest{ActorAssertion: actor.ActorAssertion, FullMethod: operation.method})
			grpcCode(t, err, codes.PermissionDenied)

			request.Audience = "content"
			_, err = l.clients[0].ResolvePrincipal(t.Context(), request)
			grpcCode(t, err, codes.PermissionDenied)
			request.Audience = "asset"
			request.Credential = &pb.ResolvePrincipalRequest_McpToken{McpToken: token.Token}
			_, err = l.clients[0].ResolvePrincipal(t.Context(), request)
			grpcCode(t, err, codes.PermissionDenied)
			request.Credential = nil
			_, err = l.clients[0].ResolvePrincipal(t.Context(), request)
			grpcCode(t, err, codes.Unauthenticated)
			if operation.method == assetpb.AssetService_ListMyAssets_FullMethodName {
				revocable = actor.ActorAssertion
			}
		})
	}

	httpStatus(t, l.request(t, 0, "POST", "/api/auth/logout", []*http.Cookie{session}, "", headers), http.StatusNoContent)
	_, err := assetIdentity.VerifyActor(t.Context(), &pb.VerifyActorRequest{ActorAssertion: revocable, FullMethod: assetpb.AssetService_ListMyAssets_FullMethodName})
	grpcCode(t, err, codes.Unauthenticated)
}
