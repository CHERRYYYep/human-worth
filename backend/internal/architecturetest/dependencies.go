// Package architecturetest contains shared helpers for module boundary tests.
package architecturetest

import (
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// DependencyPolicy decides whether a production file may import a dependency.
// sourcePath is relative to root and always uses slash separators.
type DependencyPolicy func(sourcePath, dependency string, standardLibrary bool) bool

// CheckLayerDependencies parses every production Go file under root and applies
// a module-specific policy using one shared traversal and import parser. It
// also prevents a deployable service from importing another service's internal
// implementation packages. moduleImportPath is the current service's Go import
// root, such as github.com/example/project/backend/internal/identity.
func CheckLayerDependencies(t testing.TB, root, moduleImportPath string, allowed DependencyPolicy) {
	t.Helper()
	if err := checkLayerDependencies(root, moduleImportPath, allowed, t.Errorf); err != nil {
		t.Fatal(err)
	}
}

// These packages have independent process, data, and release boundaries in
// the backend architecture. Shared technical packages such as internal/platform
// deliberately do not appear here.
var deployableServices = [...]string{
	"asset", "challenge", "challenge-worker", "content", "discovery",
	"gateway", "identity", "moderation", "voting",
}

func checkLayerDependencies(root, moduleImportPath string, allowed DependencyPolicy, report func(string, ...any)) error {
	moduleImportPath = strings.TrimSuffix(moduleImportPath, "/")
	internalRoot, currentService, ok := serviceImportRoot(moduleImportPath)
	if !ok {
		return fmt.Errorf("module import path %q is not a registered deployable service", moduleImportPath)
	}
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		sourcePath, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		sourcePath = filepath.ToSlash(sourcePath)
		if !strings.Contains(sourcePath, "/") {
			report("production code bypasses layers: %s", sourcePath)
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, spec := range file.Imports {
			dependency, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return err
			}
			if service, isServiceImport := serviceForImport(internalRoot, dependency); isServiceImport && service != currentService {
				report("%s imports forbidden service internals %s (service %s depends on service %s)", sourcePath, dependency, currentService, service)
				continue
			}
			standardLibrary := !strings.Contains(strings.Split(dependency, "/")[0], ".")
			if !allowed(sourcePath, dependency, standardLibrary) {
				report("%s imports forbidden dependency %s", sourcePath, dependency)
			}
		}
		return nil
	})
}

func serviceImportRoot(moduleImportPath string) (internalRoot, service string, ok bool) {
	for _, candidate := range deployableServices {
		if moduleImportPath == "" || !strings.HasSuffix(moduleImportPath, "/"+candidate) {
			continue
		}
		return strings.TrimSuffix(moduleImportPath, candidate), candidate, true
	}
	return "", "", false
}

func serviceForImport(internalRoot, dependency string) (string, bool) {
	for _, service := range deployableServices {
		servicePath := internalRoot + service
		if dependency == servicePath || strings.HasPrefix(dependency, servicePath+"/") {
			return service, true
		}
	}
	return "", false
}
