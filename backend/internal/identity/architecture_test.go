package identity

import (
	"strings"
	"testing"

	"github.com/KDZZZZZZ/human-worth/backend/internal/architecturetest"
)

func TestLayerDependencies(t *testing.T) {
	const module = "github.com/KDZZZZZZ/human-worth/backend/"
	const identity = module + "internal/identity/"
	architecturetest.CheckLayerDependencies(t, ".", "github.com/KDZZZZZZ/human-worth/backend/internal/identity", func(path, dependency string, standard bool) bool {
		if standard {
			return true
		}
		switch {
		case strings.HasPrefix(path, "domain/"), strings.HasPrefix(path, "application/dto/"):
			return dependency == module+"internal/contract"
		case strings.HasPrefix(path, "application/"):
			return dependency == identity+"domain" || dependency == identity+"application/dto"
		case strings.HasPrefix(path, "repo/"), strings.HasPrefix(path, "adapter/"):
			if strings.HasPrefix(dependency, module) {
				return dependency == identity+"domain" || dependency == identity+"application" || dependency == identity+"application/dto"
			}
		case strings.HasPrefix(path, "transport/"):
			return !strings.Contains(dependency, "/repo/") && !strings.Contains(dependency, "/adapter/") && !strings.Contains(dependency, "pgx") && !strings.Contains(dependency, "jwt")
		}
		return true
	})
}
