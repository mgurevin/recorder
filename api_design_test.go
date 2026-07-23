package recorder

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"strings"
	"testing"
)

func TestPublicAPIDesignRules(t *testing.T) {
	packages, err := parser.ParseDir(token.NewFileSet(), ".", func(info fs.FileInfo) bool {
		return !strings.HasSuffix(info.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatal(err)
	}

	for _, file := range packages["recorder"].Files {
		for _, declaration := range file.Decls {
			switch value := declaration.(type) {
			case *ast.FuncDecl:
				contextHelpers := map[string]bool{
					"WithTraceID":          true,
					"WithSamplingKey":      true,
					"WithRequestRedaction": true,
					"WithRequestComment":   true,
				}
				if value.Recv == nil && ast.IsExported(value.Name.Name) && strings.HasPrefix(value.Name.Name, "With") && !contextHelpers[value.Name.Name] {
					t.Errorf("functional option %s must be represented by a Config field", value.Name.Name)
				}

			case *ast.GenDecl:
				for _, spec := range value.Specs {
					typeSpec, ok := spec.(*ast.TypeSpec)
					if !ok || !ast.IsExported(typeSpec.Name.Name) {
						continue
					}

					name := typeSpec.Name.Name
					if strings.HasSuffix(name, "Option") || strings.HasSuffix(name, "PolicyFunc") || name == "BatchRecorder" || name == "EntryAssetReleaser" {
						t.Errorf("maintenance-only type %s is exported", name)
					}
				}
			}
		}
	}
}
