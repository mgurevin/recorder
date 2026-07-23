package recorder

import (
	"crypto/sha256"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"sort"
	"strings"
	"testing"
)

const publicAPIContractSHA256 = "cbaf3cc0d0231f902cf31d857cc41151344848a8449d276a009db925bfb69822"

type apiContractImporter struct {
	fallback types.Importer
	local    map[string]*types.Package
}

func (i *apiContractImporter) Import(path string) (*types.Package, error) {
	if pkg := i.local[path]; pkg != nil {
		return pkg, nil
	}

	return i.fallback.Import(path)
}

func TestPublicAPIFreezeRules(t *testing.T) {
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

func TestPublicAPISignatureContract(t *testing.T) {
	surface := publicAPISurface(t)

	sum := fmt.Sprintf("%x", sha256.Sum256([]byte(surface)))
	if sum == publicAPIContractSHA256 {
		return
	}

	t.Fatalf(
		"public API signature changed\nsha256: %s\n"+
			"review the complete surface below, then update publicAPIContractSHA256:\n\n%s",
		sum,
		surface,
	)
}

func publicAPISurface(t *testing.T) string {
	t.Helper()

	fileSet := token.NewFileSet()
	contractImporter := &apiContractImporter{
		fallback: importer.Default(),
		local:    make(map[string]*types.Package),
	}

	var lines []string

	for _, target := range []struct {
		dir        string
		name       string
		importPath string
	}{
		{".", "recorder", "github.com/mgurevin/recorder"},
		{"hario", "hario", "github.com/mgurevin/recorder/hario"},
		{"hartest", "hartest", "github.com/mgurevin/recorder/hartest"},
	} {
		checked := checkAPIContractPackage(
			t,
			fileSet,
			contractImporter,
			target.dir,
			target.name,
			target.importPath,
		)
		contractImporter.local[target.importPath] = checked

		for _, line := range publicPackageSurface(checked) {
			lines = append(lines, target.importPath+" "+line)
		}
	}

	sort.Strings(lines)

	return strings.Join(lines, "\n") + "\n"
}

func checkAPIContractPackage(
	t *testing.T,
	fileSet *token.FileSet,
	contractImporter types.Importer,
	dir string,
	name string,
	importPath string,
) *types.Package {
	t.Helper()

	packages, err := parser.ParseDir(fileSet, dir, func(info fs.FileInfo) bool {
		return !strings.HasSuffix(info.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatal(err)
	}

	syntax := make([]*ast.File, 0, len(packages[name].Files))
	for _, file := range packages[name].Files {
		syntax = append(syntax, file)
	}

	config := types.Config{Importer: contractImporter}

	checked, err := config.Check(importPath, fileSet, syntax, nil)
	if err != nil {
		t.Fatal(err)
	}

	return checked
}

func publicPackageSurface(checked *types.Package) []string {
	qualifier := func(pkg *types.Package) string {
		if pkg == nil || pkg == checked {
			return ""
		}

		return pkg.Path()
	}

	var lines []string

	scope := checked.Scope()
	for _, name := range scope.Names() {
		if !ast.IsExported(name) {
			continue
		}

		object := scope.Lookup(name)
		switch value := object.(type) {
		case *types.Const:
			lines = append(lines, fmt.Sprintf(
				"const %s %s = %s",
				name,
				types.TypeString(value.Type(), qualifier),
				value.Val().ExactString(),
			))

		case *types.Var:
			lines = append(lines, fmt.Sprintf(
				"var %s %s",
				name,
				types.TypeString(value.Type(), qualifier),
			))

		case *types.Func:
			lines = append(lines, "func "+name+formatSignature(value.Signature(), qualifier))

		case *types.TypeName:
			lines = append(lines, formatPublicType(value, qualifier)...)
		}
	}

	return lines
}

func formatPublicType(name *types.TypeName, qualifier types.Qualifier) []string {
	if name.IsAlias() {
		return []string{
			"type " + name.Name() + " = " + types.TypeString(name.Type(), qualifier),
		}
	}

	named, ok := name.Type().(*types.Named)
	if !ok {
		return []string{
			"type " + name.Name() + " " + types.TypeString(name.Type().Underlying(), qualifier),
		}
	}

	typeParameters := formatTypeParameters(named.TypeParams(), qualifier)
	lines := []string{
		"type " + name.Name() + typeParameters + " " + formatUnderlying(named.Underlying(), qualifier),
	}

	switch named.Underlying().(type) {
	case *types.Interface:
		return lines
	}

	valueMethods := methodSignatures(named, qualifier)
	pointerMethods := methodSignatures(types.NewPointer(named), qualifier)

	for method, signature := range valueMethods {
		lines = append(lines, "method "+name.Name()+"."+method+signature)
	}

	for method, signature := range pointerMethods {
		if _, ok := valueMethods[method]; ok {
			continue
		}

		lines = append(lines, "method *"+name.Name()+"."+method+signature)
	}

	return lines
}

func formatUnderlying(value types.Type, qualifier types.Qualifier) string {
	switch value := value.(type) {
	case *types.Struct:
		fields := make([]string, 0, value.NumFields())
		for index := range value.NumFields() {
			field := value.Field(index)
			if !field.Exported() {
				continue
			}

			text := field.Name() + " " + types.TypeString(field.Type(), qualifier)
			if tag := value.Tag(index); tag != "" {
				text += " `" + tag + "`"
			}

			fields = append(fields, text)
		}

		return "struct{" + strings.Join(fields, "; ") + "}"

	case *types.Interface:
		value.Complete()

		methods := make([]string, 0, value.NumMethods())
		for index := range value.NumMethods() {
			method := value.Method(index)
			if !method.Exported() {
				continue
			}

			methods = append(
				methods,
				method.Name()+formatSignature(method.Signature(), qualifier),
			)
		}

		sort.Strings(methods)

		return "interface{" + strings.Join(methods, "; ") + "}"

	default:
		return types.TypeString(value, qualifier)
	}
}

func methodSignatures(receiver types.Type, qualifier types.Qualifier) map[string]string {
	methods := make(map[string]string)

	set := types.NewMethodSet(receiver)
	for index := range set.Len() {
		method := set.At(index).Obj().(*types.Func)
		if !method.Exported() {
			continue
		}

		methods[method.Name()] = formatSignature(method.Signature(), qualifier)
	}

	return methods
}

func formatSignature(signature *types.Signature, qualifier types.Qualifier) string {
	return formatTypeParameters(signature.TypeParams(), qualifier) +
		formatTuple(signature.Params(), signature.Variadic(), qualifier) +
		formatResults(signature.Results(), qualifier)
}

func formatTypeParameters(parameters *types.TypeParamList, qualifier types.Qualifier) string {
	if parameters == nil || parameters.Len() == 0 {
		return ""
	}

	values := make([]string, parameters.Len())
	for index := range parameters.Len() {
		parameter := parameters.At(index)
		values[index] = parameter.Obj().Name() + " " +
			types.TypeString(parameter.Constraint(), qualifier)
	}

	return "[" + strings.Join(values, ", ") + "]"
}

func formatTuple(tuple *types.Tuple, variadic bool, qualifier types.Qualifier) string {
	values := make([]string, tuple.Len())
	for index := range tuple.Len() {
		valueType := tuple.At(index).Type()
		if variadic && index == tuple.Len()-1 {
			slice := valueType.(*types.Slice)
			values[index] = "..." + types.TypeString(slice.Elem(), qualifier)

			continue
		}

		values[index] = types.TypeString(valueType, qualifier)
	}

	return "(" + strings.Join(values, ", ") + ")"
}

func formatResults(results *types.Tuple, qualifier types.Qualifier) string {
	switch results.Len() {
	case 0:
		return ""

	case 1:
		return " " + types.TypeString(results.At(0).Type(), qualifier)

	default:
		return " " + formatTuple(results, false, qualifier)
	}
}
