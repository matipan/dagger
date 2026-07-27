package templates

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"

	"github.com/dagger/dagger/cmd/codegen/generator"
)

var collectionAuthoringSources = map[string]string{
	"main.go": `package main

type GoTest struct{}

// +collection
type GoTests struct {
	// +keys
	Paths []string ` + "`json:\"paths\"`" + `
}

func (tests *GoTests) Get(name string) *GoTest {
	return &GoTest{}
}

// +get
func (tests *GoTests) Lookup(name string) *GoTest {
	return &GoTest{}
}
`,
}

func loadCollectionAuthoringTestPackage(t *testing.T) *packages.Package {
	t.Helper()

	fset := token.NewFileSet()
	syntax := make([]*ast.File, 0, len(collectionAuthoringSources))
	for fileName, src := range collectionAuthoringSources {
		file, err := parser.ParseFile(fset, fileName, src, parser.ParseComments)
		require.NoErrorf(t, err, "parse %q", fileName)
		syntax = append(syntax, file)
	}

	typesPkg, err := (&types.Config{}).Check("example.com/collections", fset, syntax, nil)
	require.NoError(t, err)

	return &packages.Package{
		Types:  typesPkg,
		Syntax: syntax,
		Fset:   fset,
		Module: &packages.Module{Dir: "."},
	}
}

func TestParseCollectionAuthoringPragmas(t *testing.T) {
	t.Parallel()

	testPkg := loadCollectionAuthoringTestPackage(t)
	funcs := goTemplateFuncs{
		cfg: generator.Config{
			ModuleConfig: &generator.ModuleGeneratorConfig{
				ModuleName: "go-tests",
			},
		},
		modulePkg:  testPkg,
		moduleFset: testPkg.Fset,
	}

	var got *parsedObjectType
	err := funcs.visitTypes(false, &visitorFuncs{
		RootVisitor: func(string) error { return nil },
		StructVisitor: func(_ *parseState, _ *types.Named, obj *types.TypeName, objTypeSpec *parsedObjectType, _ *types.Struct) error {
			if obj.Name() == "GoTests" {
				got = objTypeSpec
			}
			return nil
		},
		IfaceVisitor: func(_ *parseState, _ *types.Named, _ *types.TypeName, _ *parsedIfaceType, _ *types.Interface) error {
			return nil
		},
		EnumVisitor: func(_ *parseState, _ *types.Named, _ *types.TypeName, _ *parsedEnumType, _ *types.Basic) error {
			return nil
		},
	})
	require.NoError(t, err)
	require.NotNil(t, got)
	require.True(t, got.isCollection)
	require.Len(t, got.fields, 1)
	require.Equal(t, "paths", got.fields[0].name)
	require.True(t, got.fields[0].isCollectionKeys)

	var lookup *funcTypeSpec
	var get *funcTypeSpec
	for _, method := range got.methods {
		switch method.name {
		case "Lookup":
			lookup = method
		case "Get":
			get = method
		}
	}
	require.NotNil(t, get)
	require.NotNil(t, lookup)
	require.False(t, get.isCollectionGet)
	require.True(t, lookup.isCollectionGet)
}

func TestValidateCollectionAuthoringPragmas(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		spec *parsedObjectType
		err  string
	}{
		{
			name: "keys requires collection",
			spec: &parsedObjectType{
				name:   "GoTests",
				fields: []*fieldSpec{{isCollectionKeys: true}},
			},
			err: "object GoTests uses collection member pragmas without +collection",
		},
		{
			name: "get requires collection",
			spec: &parsedObjectType{
				name:    "GoTests",
				methods: []*funcTypeSpec{{isCollectionGet: true}},
			},
			err: "object GoTests uses collection member pragmas without +collection",
		},
		{
			name: "keys override is unique",
			spec: &parsedObjectType{
				name:         "GoTests",
				isCollection: true,
				fields: []*fieldSpec{
					{isCollectionKeys: true},
					{isCollectionKeys: true},
				},
			},
			err: "collection object GoTests has multiple +keys fields",
		},
		{
			name: "get override is unique",
			spec: &parsedObjectType{
				name:         "GoTests",
				isCollection: true,
				methods: []*funcTypeSpec{
					{isCollectionGet: true},
					{isCollectionGet: true},
				},
			},
			err: "collection object GoTests has multiple +get methods",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tt.spec.TypeDefCode()
			require.EqualError(t, err, tt.err)
		})
	}
}
