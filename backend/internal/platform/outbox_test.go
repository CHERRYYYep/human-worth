package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestRunOutboxLogsFailuresAndPersistedBlocks(t *testing.T) {
	for _, tc := range []struct {
		name, outcome, level, message string
		err                           error
		cause                         error
	}{
		{name: "retryable", outcome: "retryable_failure", level: "WARN", message: "outbox_delivery_failed", err: errors.New("temporary failure"), cause: &pgconn.PgError{Code: "23505", ConstraintName: "review_unique", Detail: "private row detail"}},
		{name: "blocked", outcome: "blocked", level: "ERROR", message: "outbox_delivery_blocked", cause: errors.New("stable business reason")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&output, nil))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var calls atomic.Int32
			runOutbox(ctx, logger, "content_to_moderation", time.Millisecond, time.Second, func(context.Context) (OutboxResult, error) {
				if calls.Add(1) == 1 {
					return OutboxResult{Outcome: tc.outcome, Stage: "call_downstream", ItemID: "rev_test", Reason: "business_conflict", Cause: tc.cause}, tc.err
				}
				cancel()
				return OutboxResult{}, context.Canceled
			})
			if calls.Load() < 2 {
				t.Fatal("runner did not stop on service cancellation")
			}
			var entry map[string]any
			if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &entry); err != nil {
				t.Fatalf("log record: %s (%v)", output.String(), err)
			}
			if entry["level"] != tc.level || entry["msg"] != tc.message || entry["delivery"] != "content_to_moderation" || entry["stage"] != "call_downstream" || entry["item_id"] != "rev_test" {
				t.Fatalf("unexpected structured log: %v", entry)
			}
			if tc.name == "retryable" && entry["error_reason"] != "unspecified" {
				t.Fatalf("unfiltered technical error reason: %v", entry["error_reason"])
			}
			if entry["detail"] == "private row detail" || strings.Contains(output.String(), "private row detail") || strings.Contains(output.String(), "temporary failure") {
				t.Fatal("log leaked unfiltered error detail")
			}
		})
	}
}

func TestRunOutboxDoesNotLogIdleOrShutdown(t *testing.T) {
	var output bytes.Buffer
	ctx, cancel := context.WithCancel(t.Context())
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	var calls atomic.Int32
	runOutbox(ctx, logger, "moderation_to_content", time.Millisecond, time.Second, func(context.Context) (OutboxResult, error) {
		if calls.Add(1) == 2 {
			cancel()
			return OutboxResult{}, context.Canceled
		}
		return OutboxResult{Outcome: "idle"}, nil
	})
	if output.Len() != 0 {
		t.Fatalf("idle/shutdown emitted error logs: %s", output.String())
	}
}
