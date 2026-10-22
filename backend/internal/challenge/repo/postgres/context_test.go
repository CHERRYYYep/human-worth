package postgres

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/KDZZZZZZ/human-worth/backend/internal/challenge/adapter/protobuf"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestPersistenceContextContract(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code codes.Code
	}{
		{context.Canceled, codes.Canceled}, {context.DeadlineExceeded, codes.DeadlineExceeded},
	} {
		got := persistence(fmt.Errorf("query: %w", tc.err))
		if !errors.Is(got, tc.err) || status.Code(protobuf.Error(got)) != tc.code {
			t.Errorf("%v: lost context classification: %v", tc.err, got)
		}
	}
}
