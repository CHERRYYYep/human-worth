package platform

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"google.golang.org/grpc/status"
)

const (
	outboxInterval = time.Second
	outboxBudget   = 7 * time.Second
)

// OutboxResult contains only operational metadata, never the queued payload.
type OutboxResult struct {
	Outcome string // idle, delivered, blocked, or lease_lost
	Stage   string
	ItemID  string
	Reason  string
	Cause   error
	HadWork bool
}

type OutboxDelivery func(context.Context) (OutboxResult, error)

// RunOutbox shares scheduling, cancellation, timeout, and delivery logging.
// Claiming, acknowledgement, retry policy, and business blocking stay in the
// module-specific delivery function.
func RunOutbox(ctx context.Context, logger *slog.Logger, name string, deliver OutboxDelivery) {
	runOutbox(ctx, logger, name, outboxInterval, outboxBudget, deliver)
}

func runOutbox(ctx context.Context, logger *slog.Logger, name string, interval, budget time.Duration, deliver OutboxDelivery) {
	if logger == nil {
		logger = slog.Default()
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			call, cancel := context.WithTimeout(ctx, budget)
			result, err := deliver(call)
			cancel()
			if err != nil {
				if ctx.Err() != nil { // Service shutdown is an expected cancellation.
					return
				}
				attrs := []any{"delivery", name, "stage", result.Stage, "outcome", "retryable_failure", "grpc_code", status.Code(err).String(), "error_reason", safeLogReason(status.Convert(err).Message())}
				if result.ItemID != "" {
					attrs = append(attrs, "item_id", result.ItemID)
				}
				attrs = appendCauseAttrs(attrs, result.Cause)
				logger.WarnContext(ctx, "outbox_delivery_failed", attrs...)
				continue
			}
			if result.Outcome == "blocked" {
				attrs := []any{"delivery", name, "stage", result.Stage, "outcome", result.Outcome, "reason", safeLogReason(result.Reason)}
				if result.ItemID != "" {
					attrs = append(attrs, "item_id", result.ItemID)
				}
				attrs = appendCauseAttrs(attrs, result.Cause)
				logger.ErrorContext(ctx, "outbox_delivery_blocked", attrs...)
			}
		}
	}
}

func safeLogReason(message string) string {
	if message == "" || len(message) > 80 {
		return "unspecified"
	}
	for _, r := range message {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.') {
			return "unspecified"
		}
	}
	return message
}

// appendCauseAttrs records useful dependency classification without exposing
// SQL details, connection targets, or arbitrary downstream response bodies.
func appendCauseAttrs(attrs []any, cause error) []any {
	if cause == nil {
		return attrs
	}
	var database *pgconn.PgError
	if errors.As(cause, &database) {
		attrs = append(attrs, "cause_type", "postgres", "sql_state", database.Code)
		if database.ConstraintName != "" {
			attrs = append(attrs, "constraint", database.ConstraintName)
		}
		return attrs
	}
	var network *net.OpError
	if errors.As(cause, &network) {
		attrs = append(attrs, "cause_type", "network", "network_operation", network.Op)
		return attrs
	}
	if errors.Is(cause, context.DeadlineExceeded) {
		return append(attrs, "cause_type", "deadline")
	}
	if errors.Is(cause, context.Canceled) {
		return append(attrs, "cause_type", "canceled")
	}
	attrs = append(attrs, "cause_type", status.Code(cause).String())
	return attrs
}
