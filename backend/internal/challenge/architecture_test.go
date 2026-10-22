package challenge_test

import (
	"strings"
	"testing"

	"github.com/KDZZZZZZ/human-worth/backend/internal/architecturetest"
)

func TestLayerDependencies(t *testing.T) {
	architecturetest.CheckLayerDependencies(t, ".", "github.com/KDZZZZZZ/human-worth/backend/internal/challenge", challengeDependencyAllowed)
}

func challengeDependencyAllowed(path, dependency string, standard bool) bool {
	const challenge = "github.com/KDZZZZZZ/human-worth/backend/internal/challenge/"
	switch {
	case strings.HasPrefix(path, "domain/"):
		// Standard-library dependencies may still hide nondeterminism or I/O.
		return standard && dependency != "crypto/rand" && dependency != "math/rand" && dependency != "math/rand/v2" && dependency != "os" && dependency != "os/exec" && dependency != "syscall" && dependency != "database/sql" && dependency != "net" && !strings.HasPrefix(dependency, "net/")
	case strings.HasPrefix(path, "application/dto/"):
		return standard || dependency == challenge+"domain"
	case strings.HasPrefix(path, "application/"), strings.HasPrefix(path, "agent/"):
		if dependency == "os" || dependency == "os/exec" || dependency == "syscall" || dependency == "database/sql" || dependency == "net" || strings.HasPrefix(dependency, "net/") {
			return false
		}
		return standard || dependency == challenge+"domain" || dependency == challenge+"application/dto"
	case strings.HasPrefix(path, "transport/"):
		return !strings.Contains(dependency, "/repo/") && !strings.Contains(dependency, "pgx") && (!strings.Contains(dependency, "/adapter/") || dependency == challenge+"adapter/protobuf")
	case strings.HasPrefix(path, "adapter/"):
		return !strings.Contains(dependency, "/repo/") && !strings.Contains(dependency, "/transport/") && !strings.Contains(dependency, "pgx")
	case strings.HasPrefix(path, "repo/"):
		return !strings.Contains(dependency, "/transport/") && !strings.Contains(dependency, "/agent") && !strings.HasPrefix(dependency, "google.golang.org/grpc")
	}
	return true
}

func TestChallengeDependencyPolicy(t *testing.T) {
	const challenge = "github.com/KDZZZZZZ/human-worth/backend/internal/challenge/"
	for _, test := range []struct {
		path, dependency string
		allowed          bool
	}{
		{"domain/rules.go", "crypto/rand", false},
		{"domain/rules.go", "math/rand", false},
		{"domain/rules.go", "math/rand/v2", false},
		{"domain/rules.go", "time", true},
		{"domain/rules.go", "crypto/sha256", true},
		{"domain/rules.go", "net/http", false},
		{"domain/rules.go", challenge + "application", false},
		{"application/ids.go", "crypto/rand", true},
		{"application/service.go", "database/sql", false},
		{"application/service.go", challenge + "domain", true},
		{"transport/grpc/server.go", challenge + "adapter/protobuf", true},
		{"transport/grpc/server.go", challenge + "repo/postgres", false},
	} {
		t.Run(test.path+"/"+test.dependency, func(t *testing.T) {
			standard := !strings.Contains(strings.Split(test.dependency, "/")[0], ".")
			if got := challengeDependencyAllowed(test.path, test.dependency, standard); got != test.allowed {
				t.Fatalf("dependency allowed = %v, want %v", got, test.allowed)
			}
		})
	}
}
