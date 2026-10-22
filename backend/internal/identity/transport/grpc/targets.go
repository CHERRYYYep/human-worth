package grpc

import (
	"github.com/KDZZZZZZ/human-worth/backend/internal/contract"
	"github.com/KDZZZZZZ/human-worth/backend/internal/identity/application"
)

// Targets derives Identity's assertion bindings from the shared operation
// contract. Methods without an actor policy are not valid assertion targets.
func Targets() []application.Target {
	var targets []application.Target
	for _, operation := range contract.Operations() {
		if !operation.HasPolicy {
			continue
		}
		targets = append(targets, application.Target{
			Operation: operation.ID,
			Audience:  operation.Policy.Audience,
			Method:    operation.FullMethod,
		})
	}
	return targets
}
