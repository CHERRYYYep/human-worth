package grpc

import (
	"testing"

	contentpb "github.com/KDZZZZZZ/human-worth/backend/gen/humanworth/content/v1"
	moderationpb "github.com/KDZZZZZZ/human-worth/backend/gen/humanworth/moderation/v1"
	"github.com/KDZZZZZZ/human-worth/backend/internal/contract"
)

func TestAdditionalEntryAssertionTargets(t *testing.T) {
	want := map[string]contract.OperationID{
		contentpb.ContentService_SubmitAdditionalEntry_FullMethodName:  contract.SubmitAdditionalEntry,
		contentpb.ContentService_GetMyEntrySubmission_FullMethodName:   contract.ReadOwnEntrySubmission,
		contentpb.ContentService_ResubmitRejectedEntry_FullMethodName:  contract.ResubmitRejectedEntry,
		contentpb.ContentService_ListMyEntrySubmissions_FullMethodName: contract.ListOwnEntrySubmissions,
	}
	for _, target := range Targets() {
		switch target.Method {
		case contentpb.ContentService_GetEntryReviewSnapshot_FullMethodName, contentpb.ContentService_ApplyEntryReviewDecision_FullMethodName, moderationpb.ModerationService_CreateEntryReviewCase_FullMethodName:
			t.Fatal("service-only operation can issue user assertions", target)
		}
		if op, ok := want[target.Method]; ok {
			if target.Audience != "content" || target.Operation != op {
				t.Fatal("incorrect assertion binding", target)
			}
			delete(want, target.Method)
		}
	}
	if len(want) != 0 {
		t.Fatal("missing entry assertion targets", want)
	}
}
