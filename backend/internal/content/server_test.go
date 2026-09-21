package content

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	pb "github.com/KDZZZZZZ/human-worth/backend/gen/humanworth/content/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

func TestDraftValidationAndFingerprint(t *testing.T) {
	const empty = `{"title":"t","summary":"","description":"","entries":[]}`
	const entry = `{"title":"t","summary":"","description":"","entries":[{"side":"agent","title":"work","source":"model","artifacts":[{"kind":"link","url":"https://example.com/work"}],"permissions":{"cloudUse":false},"agentConfiguration":{"model":"example","parameters":{"b":2,"a":1}}}]}`
	for _, raw := range []string{empty, entry} {
		input := &pb.TaskDraftInput{}
		if err := protojson.Unmarshal([]byte(raw), input); err != nil {
			t.Fatal(err)
		}
		first, err := validate(input)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 10; i++ {
			second, err := validate(input)
			if err != nil || !bytes.Equal(first, second) {
				t.Fatal("unstable fingerprint", err)
			}
		}
	}
	for _, tc := range []struct {
		raw  string
		code codes.Code
	}{
		{`{}`, codes.InvalidArgument},
		{`{"title":"t","summary":"","entries":[]}`, codes.InvalidArgument},
		{`{"title":"\u0000","summary":"","description":"","entries":[]}`, codes.InvalidArgument},
		{`{"title":"t","summary":"","description":"","entries":[{"side":"human","title":"w","source":"s","artifacts":[{"kind":"link","url":"javascript:alert(1)"}],"permissions":{"cloudUse":false}}]}`, codes.InvalidArgument},
		{`{"title":"t","summary":"","description":"","entries":[{"side":"human","title":"w","source":"s","artifacts":[{"kind":"file","assetId":"somebody-elses-file"}],"permissions":{"cloudUse":false}}]}`, codes.FailedPrecondition},
		{`{"title":"t","summary":"","description":"","entries":[{"side":"human","title":"w","source":"s","artifacts":[{"kind":"link","url":"https://example.com"}],"permissions":{}}]}`, codes.InvalidArgument},
		{`{"title":"t","summary":"","description":"","entries":[{"side":"agent","title":"w","source":"s","artifacts":[{"kind":"link","url":"https://example.com"}],"permissions":{"cloudUse":false}}]}`, codes.InvalidArgument},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			input := &pb.TaskDraftInput{}
			if err := protojson.Unmarshal([]byte(tc.raw), input); err != nil {
				t.Fatal(err)
			}
			_, err := validate(input)
			if status.Code(err) != tc.code {
				t.Fatalf("got %v want %v", err, tc.code)
			}
		})
	}
}

func TestSubmissionFilterAndCursor(t *testing.T) {
	filter, limit, err := effectiveSubmissionFilter(&pb.ListMySubmissionsRequest{})
	if err != nil || filter.Kind != "task" || filter.State != "draft" || filter.Category != nil || limit != 20 {
		t.Fatalf("defaults: filter=%+v limit=%d err=%v", filter, limit, err)
	}
	category := "绘画"
	filter, limit, err = effectiveSubmissionFilter(&pb.ListMySubmissionsRequest{Kind: "task", State: "draft", Category: &category, Limit: 100})
	if err != nil || filter.Category == nil || *filter.Category != category || limit != 100 {
		t.Fatalf("explicit filter: filter=%+v limit=%d err=%v", filter, limit, err)
	}
	legacyCategory := strings.Repeat("a", 6400)
	empty := ""
	if _, err := validate(&pb.TaskDraftInput{Title: "t", Summary: &empty, Description: &empty, Category: &legacyCategory}); err != nil {
		t.Fatalf("legacy category rejected: %v", err)
	}
	tooLargeCategory := strings.Repeat("a", maxDraftBytes+1)
	for _, request := range []*pb.ListMySubmissionsRequest{
		{Kind: "entry"},
		{State: "published"},
		{Category: new(string)},
		{Category: &tooLargeCategory},
		{Limit: -1},
		{Limit: 101},
	} {
		if _, _, err := effectiveSubmissionFilter(request); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("accepted invalid filter: %+v err=%v", request, err)
		}
	}

	created := time.Date(2026, 9, 20, 12, 34, 56, 123456000, time.UTC)
	id := "tsk_0123456789abcdef0123456789abcdef"
	cursor := encodeSubmissionCursor("acct_alice", filter, created, id)
	gotTime, gotID, ok, err := decodeSubmissionCursor(cursor, "acct_alice", filter)
	if err != nil || !ok || gotID != id || !gotTime.Equal(created) || gotTime.Nanosecond() != created.Nanosecond() {
		t.Fatalf("cursor round trip: time=%v id=%q ok=%v err=%v", gotTime, gotID, ok, err)
	}
	if _, _, ok, err := decodeSubmissionCursor("", "acct_alice", filter); err != nil || ok {
		t.Fatalf("empty cursor was not first page: ok=%v err=%v", ok, err)
	}
	otherCategory := "代码"
	otherFilter := submissionFilter{Kind: "task", State: "draft", Category: &otherCategory}
	cases := map[string]struct {
		raw    string
		author string
		filter submissionFilter
	}{
		"damaged":       {cursor + "!", "acct_alice", filter},
		"too long":      {strings.Repeat("a", maxSubmissionCursorLength+1), "acct_alice", filter},
		"other account": {cursor, "acct_bob", filter},
		"other filter":  {cursor, "acct_alice", otherFilter},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, _, err := decodeSubmissionCursor(tc.raw, tc.author, tc.filter); status.Code(err) != codes.InvalidArgument {
				t.Fatalf("accepted invalid cursor: %v", err)
			}
		})
	}

	data, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		t.Fatal(err)
	}
	var versioned map[string]any
	if err = json.Unmarshal(data, &versioned); err != nil {
		t.Fatal(err)
	}
	versioned["v"] = float64(2)
	data, err = json.Marshal(versioned)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err = decodeSubmissionCursor(base64.RawURLEncoding.EncodeToString(data), "acct_alice", filter); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("accepted unsupported cursor version: %v", err)
	}
}

func TestSubmissionPageUsesProtobufBudget(t *testing.T) {
	numbers := strings.Repeat("0,", 2999) + "0"
	raw := fmt.Sprintf(`{"title":"large","summary":"","description":"","entries":[{"side":"agent","title":"work","source":"model","artifacts":[{"kind":"link","url":"https://example.com/work"}],"permissions":{"cloudUse":false},"agentConfiguration":{"model":"example","parameters":{"nested":{"values":[%s]}}}}]}`, numbers)
	content := &pb.TaskDraftInput{}
	if err := protojson.Unmarshal([]byte(raw), content); err != nil {
		t.Fatal(err)
	}
	created := time.Date(2026, 9, 20, 12, 34, 56, 123456000, time.UTC)
	filter := submissionFilter{Kind: "task", State: "draft"}
	builder := newSubmissionPage("acct_alice", filter, 100)
	attempted := 0
	for i := 0; i < 101; i++ {
		id := fmt.Sprintf("tsk_%032x", 101-i)
		attempted++
		added, err := builder.add(&pb.TaskSubmission{Id: id, AuthorId: "acct_alice", Revision: 1, State: "draft", Content: content}, created)
		if err != nil {
			t.Fatal(err)
		}
		if !added {
			break
		}
	}
	page := builder.finish(true)
	if len(page.Items) == 0 || len(page.Items) >= 100 || page.NextCursor == "" {
		t.Fatalf("page was not bounded by encoded size: items=%d cursor=%q", len(page.Items), page.NextCursor)
	}
	if attempted != len(page.Items)+1 {
		t.Fatalf("page decoded beyond first non-fitting item: attempted=%d items=%d", attempted, len(page.Items))
	}
	if size := proto.Size(page); size > maxSubmissionPageBytes {
		t.Fatalf("page exceeds protobuf budget: %d > %d", size, maxSubmissionPageBytes)
	}
	_, cursorID, ok, err := decodeSubmissionCursor(page.NextCursor, "acct_alice", filter)
	if err != nil || !ok || cursorID != page.Items[len(page.Items)-1].Id {
		t.Fatalf("cursor does not resume after last returned item: id=%q ok=%v err=%v", cursorID, ok, err)
	}
	values := page.Items[0].Content.Entries[0].AgentConfiguration.Parameters.Fields["nested"].GetStructValue().Fields["values"].GetListValue().Values
	if len(values) != 3000 {
		t.Fatalf("parameters were truncated: %d", len(values))
	}
}
