package architecturetest

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestLayerDependencyScan(t *testing.T) {
	root := t.TempDir()
	for name, source := range map[string]string{
		"root.go":             "package fixture\n",
		"domain/allowed.go":   "package domain\nimport renamed \"time\"\n",
		"domain/random.go":    "package domain\nimport random \"crypto/rand\"\n",
		"domain/foreign.go":   "package domain\nimport _ \"example.com/foreign\"\n",
		"domain/skip_test.go": "this is deliberately not valid Go",
	} {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(source), 0600); err != nil {
			t.Fatal(err)
		}
	}
	var violations []string
	err := checkLayerDependencies(root, "example.com/project/backend/internal/challenge", func(path, dependency string, standard bool) bool {
		if path == "root.go" {
			return true
		}
		return standard && dependency != "crypto/rand"
	}, func(format string, args ...any) {
		violations = append(violations, fmt.Sprintf(format, args...))
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"domain/foreign.go imports forbidden dependency example.com/foreign",
		"domain/random.go imports forbidden dependency crypto/rand",
		"production code bypasses layers: root.go",
	}
	if !reflect.DeepEqual(violations, want) {
		t.Fatalf("violations = %v, want %v", violations, want)
	}
	if err := os.WriteFile(filepath.Join(root, "domain/broken.go"), []byte("not Go"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := checkLayerDependencies(root, "example.com/project/backend/internal/challenge", func(string, string, bool) bool { return true }, func(string, ...any) {}); err == nil {
		t.Fatal("malformed production source must fail the check")
	}
}

func TestLayerDependencyScanRejectsForeignServiceInternals(t *testing.T) {
	root := t.TempDir()
	sources := map[string]string{
		"repo/store.go":           "package repo\nimport _ \"example.com/project/backend/internal/identity/domain\"\n",
		"application/service.go":  "package application\nimport _ \"example.com/project/backend/internal/platform\"\n",
		"application/protocol.go": "package application\nimport _ \"example.com/project/backend/gen/humanworth/identity/v1\"\n",
		"transport/server.go":     "package transport\nimport _ \"example.com/project/backend/internal/challenge/domain\"\n",
	}
	for name, source := range sources {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(source), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range []struct {
		service, want string
	}{
		{"challenge", "repo/store.go imports forbidden service internals example.com/project/backend/internal/identity/domain (service challenge depends on service identity)"},
		{"identity", "transport/server.go imports forbidden service internals example.com/project/backend/internal/challenge/domain (service identity depends on service challenge)"},
	} {
		t.Run(test.service, func(t *testing.T) {
			var violations []string
			modulePath := "example.com/project/backend/internal/" + test.service
			err := checkLayerDependencies(root, modulePath, func(string, string, bool) bool { return true }, func(format string, args ...any) {
				violations = append(violations, fmt.Sprintf(format, args...))
			})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(violations, []string{test.want}) {
				t.Fatalf("violations = %v, want [%s]", violations, test.want)
			}
		})
	}
}

func TestServiceImportRootMustBeRegistered(t *testing.T) {
	if _, _, ok := serviceImportRoot("example.com/project/backend/internal/unknown"); ok {
		t.Fatal("unknown service import root must be rejected")
	}
}
