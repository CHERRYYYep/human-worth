package application

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestApplicationDoesNotDependOnTransportOrPostgres(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, spec := range file.Imports {
			dependency, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(dependency, "/internal/asset/repository/") || strings.Contains(dependency, "/transport/") || dependency == "github.com/jackc/pgx/v5" || strings.HasPrefix(dependency, "google.golang.org/grpc") {
				t.Errorf("%s imports forbidden application dependency %s", name, dependency)
			}
		}
	}
}
