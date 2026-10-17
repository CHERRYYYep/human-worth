//go:build integration

package identity

import (
	"net/http"
	"testing"

	assetpb "github.com/KDZZZZZZ/human-worth/backend/gen/humanworth/asset/v1"
	contentpb "github.com/KDZZZZZZ/human-worth/backend/gen/humanworth/content/v1"
	pb "github.com/KDZZZZZZ/human-worth/backend/gen/humanworth/identity/v1"
	moderationpb "github.com/KDZZZZZZ/human-worth/backend/gen/humanworth/moderation/v1"
	"github.com/KDZZZZZZ/human-worth/backend/internal/identity/application"
	"github.com/KDZZZZZZ/human-worth/backend/internal/identity/application/dto"
	"github.com/KDZZZZZZ/human-worth/backend/internal/identity/repo/postgres"
	identitygrpc "github.com/KDZZZZZZ/human-worth/backend/internal/identity/transport/grpc"
	"google.golang.org/grpc/codes"
)

func TestModerationIdentityAuthorization(t *testing.T) {
	l := newIdentityLab(t)
	user, _ := l.login(t, "moderation-auth-user", nil)
	_, initial := l.login(t, "moderation-auth-admin", nil)
	ops := application.NewAdmin(postgres.New(l.db), randomToken)
	must(t, identitygrpc.Error(ops.ChangeAccount(t.Context(), "test-operator", dto.ChangeAccountInput{AccountID: initial.Account.Id, Role: "admin", State: "active", ExpectedVersion: 1})))
	admin, me := l.login(t, "moderation-auth-admin", nil)
	headers := map[string]string{"Origin": l.origin, "X-CSRF-Token": me.CsrfToken, "Content-Type": "application/json"}
	response := l.request(t, 0, "POST", "/api/me/mcp-tokens", []*http.Cookie{admin}, `{"name":"review forbidden MCP","createRequestId":"moderation-auth-check"}`, headers)
	httpStatus(t, response, 201)
	var token struct{ Token string }
	readJSON(t, response, &token)
	var revokeAssertion string
	for _, op := range []struct {
		name, audience, method string
		write                  bool
	}{
		{"list", "moderation", moderationpb.ModerationService_ListReviews_FullMethodName, false},
		{"get", "moderation", moderationpb.ModerationService_GetReview_FullMethodName, false},
		{"decide", "moderation", moderationpb.ModerationService_DecideReview_FullMethodName, true},
		{"download", "asset", assetpb.AssetService_DownloadReviewAsset_FullMethodName, false},
		{"access", "content", contentpb.ContentService_CheckReviewAssetAccess_FullMethodName, false},
	} {
		t.Run(op.name, func(t *testing.T) {
			req := &pb.ResolvePrincipalRequest{Credential: &pb.ResolvePrincipalRequest_SessionCookie{SessionCookie: admin.Value}, Audience: op.audience, FullMethod: op.method}
			if op.write {
				_, err := l.clients[0].ResolvePrincipal(t.Context(), req)
				grpcCode(t, err, codes.PermissionDenied)
				req.Origin = l.origin
				req.CsrfToken = me.CsrfToken
			}
			actor, err := l.clients[0].ResolvePrincipal(t.Context(), req)
			must(t, err)
			verifier := l.client(t, 0, op.audience, l.pki)
			_, err = verifier.VerifyActor(t.Context(), &pb.VerifyActorRequest{ActorAssertion: actor.ActorAssertion, FullMethod: op.method})
			must(t, err)
			_, err = verifier.VerifyActor(t.Context(), &pb.VerifyActorRequest{ActorAssertion: actor.ActorAssertion, FullMethod: contentpb.ContentService_GetMyTaskSubmission_FullMethodName})
			if err == nil {
				t.Fatal("cross-method assertion accepted")
			}
			_, err = verifier.VerifyActor(t.Context(), &pb.VerifyActorRequest{ActorAssertion: actor.ActorAssertion + "tamper", FullMethod: op.method})
			grpcCode(t, err, codes.Unauthenticated)
			req.Audience = "identity"
			_, err = l.clients[0].ResolvePrincipal(t.Context(), req)
			grpcCode(t, err, codes.PermissionDenied)
			req.Audience = op.audience
			req.Credential = &pb.ResolvePrincipalRequest_McpToken{McpToken: token.Token}
			_, err = l.clients[0].ResolvePrincipal(t.Context(), req)
			grpcCode(t, err, codes.PermissionDenied)
			req.Credential = &pb.ResolvePrincipalRequest_SessionCookie{SessionCookie: user.Value}
			req.Origin = ""
			req.CsrfToken = ""
			_, err = l.clients[0].ResolvePrincipal(t.Context(), req)
			grpcCode(t, err, codes.PermissionDenied)
			if op.name == "list" {
				revokeAssertion = actor.ActorAssertion
			}
		})
	}
	httpStatus(t, l.request(t, 0, "POST", "/api/auth/logout", []*http.Cookie{admin}, "", headers), 204)
	_, err := l.client(t, 0, "moderation", l.pki).VerifyActor(t.Context(), &pb.VerifyActorRequest{ActorAssertion: revokeAssertion, FullMethod: moderationpb.ModerationService_ListReviews_FullMethodName})
	grpcCode(t, err, codes.Unauthenticated)
}
