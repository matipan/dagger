package core

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/dagger/dagger/dagql"
	"github.com/dagger/dagger/engine"
	"github.com/stretchr/testify/require"
	"github.com/vektah/gqlparser/v2/ast"
)

type artifactTraversalCounts struct {
	root            atomic.Int64
	modules         atomic.Int64
	moduleGet       atomic.Int64
	testDirectories atomic.Int64
	directoryGet    atomic.Int64
	tests           atomic.Int64
}

type artifactTraversalRootValue struct {
	counts *artifactTraversalCounts
}

type artifactTraversalModulesValue struct {
	counts *artifactTraversalCounts
}

type artifactTraversalModuleValue struct {
	counts *artifactTraversalCounts
	name   string
}

type artifactTraversalDirectoriesValue struct {
	counts *artifactTraversalCounts
	module string
}

type artifactTraversalDirectoryValue struct {
	counts *artifactTraversalCounts
	path   string
}

type artifactTraversalTestsValue struct{}
type artifactTraversalTestValue struct{}

func (*artifactTraversalRootValue) Type() *ast.Type {
	return &ast.Type{NamedType: "ArtifactTraversalRoot", NonNull: true}
}

func (*artifactTraversalModulesValue) Type() *ast.Type {
	return &ast.Type{NamedType: "ArtifactTraversalModules", NonNull: true}
}

func (*artifactTraversalModuleValue) Type() *ast.Type {
	return &ast.Type{NamedType: "ArtifactTraversalModule", NonNull: true}
}

func (*artifactTraversalDirectoriesValue) Type() *ast.Type {
	return &ast.Type{NamedType: "ArtifactTraversalDirectories", NonNull: true}
}

func (*artifactTraversalDirectoryValue) Type() *ast.Type {
	return &ast.Type{NamedType: "ArtifactTraversalDirectory", NonNull: true}
}

func (*artifactTraversalTestsValue) Type() *ast.Type {
	return &ast.Type{NamedType: "ArtifactTraversalTests", NonNull: true}
}

func (*artifactTraversalTestValue) Type() *ast.Type {
	return &ast.Type{NamedType: "ArtifactTraversalTest", NonNull: true}
}

func TestArtifactsTraverseResolvedCollectionItems(t *testing.T) {
	counts := &artifactTraversalCounts{}
	typeDefs := artifactTraversalTypeDefs(t)
	cache, err := dagql.NewCache(t.Context(), "", nil, nil)
	require.NoError(t, err)
	ctx := dagql.ContextWithCache(t.Context(), cache)
	ctx = engine.ContextWithClientMetadata(ctx, &engine.ClientMetadata{
		ClientID:  "artifact-traversal-test",
		SessionID: "artifact-traversal-test",
	})
	srv := newCoreDagqlServerForTest(t, &Query{})
	dagql.Fields[*Query]{
		dagql.Func("artifactTraversal", func(
			context.Context,
			*Query,
			struct{},
		) (*artifactTraversalRootValue, error) {
			counts.root.Add(1)
			return &artifactTraversalRootValue{counts: counts}, nil
		}),
		dagql.Func("runArtifactTraversal", func(
			ctx context.Context,
			_ *Query,
			_ struct{},
		) (dagql.Int, error) {
			artifacts, err := NewArtifacts(ctx, srv, typeDefs)
			if err != nil {
				return 0, err
			}
			return dagql.Int(len(artifacts.Items())), nil
		}),
	}.Install(srv)
	dagql.Fields[*artifactTraversalRootValue]{
		dagql.Func("modules", func(
			_ context.Context,
			self *artifactTraversalRootValue,
			_ struct{},
		) (*artifactTraversalModulesValue, error) {
			self.counts.modules.Add(1)
			return &artifactTraversalModulesValue{counts: self.counts}, nil
		}),
	}.Install(srv)
	dagql.Fields[*artifactTraversalModulesValue]{
		dagql.Func("keys", func(
			context.Context,
			*artifactTraversalModulesValue,
			struct{},
		) (dagql.Array[dagql.String], error) {
			return dagql.NewStringArray("api", "cloud"), nil
		}),
		dagql.Func("get", func(
			_ context.Context,
			self *artifactTraversalModulesValue,
			args struct{ Key dagql.String },
		) (*artifactTraversalModuleValue, error) {
			self.counts.moduleGet.Add(1)
			return &artifactTraversalModuleValue{
				counts: self.counts,
				name:   args.Key.String(),
			}, nil
		}),
	}.Install(srv)
	dagql.Fields[*artifactTraversalModuleValue]{
		dagql.Func("testDirectories", func(
			_ context.Context,
			self *artifactTraversalModuleValue,
			_ struct{},
		) (*artifactTraversalDirectoriesValue, error) {
			self.counts.testDirectories.Add(1)
			return &artifactTraversalDirectoriesValue{
				counts: self.counts,
				module: self.name,
			}, nil
		}),
	}.Install(srv)
	dagql.Fields[*artifactTraversalDirectoriesValue]{
		dagql.Func("keys", func(
			_ context.Context,
			self *artifactTraversalDirectoriesValue,
			_ struct{},
		) (dagql.Array[dagql.String], error) {
			return dagql.NewStringArray(
				self.module+"/first",
				self.module+"/second",
			), nil
		}),
		dagql.Func("get", func(
			_ context.Context,
			self *artifactTraversalDirectoriesValue,
			args struct{ Key dagql.String },
		) (*artifactTraversalDirectoryValue, error) {
			self.counts.directoryGet.Add(1)
			return &artifactTraversalDirectoryValue{
				counts: self.counts,
				path:   args.Key.String(),
			}, nil
		}),
	}.Install(srv)
	dagql.Fields[*artifactTraversalDirectoryValue]{
		dagql.Func("tests", func(
			_ context.Context,
			self *artifactTraversalDirectoryValue,
			_ struct{},
		) (*artifactTraversalTestsValue, error) {
			self.counts.tests.Add(1)
			return &artifactTraversalTestsValue{}, nil
		}),
	}.Install(srv)
	dagql.Fields[*artifactTraversalTestsValue]{
		dagql.Func("keys", func(
			context.Context,
			*artifactTraversalTestsValue,
			struct{},
		) (dagql.Array[dagql.String], error) {
			return dagql.NewStringArray("TestOne", "TestTwo"), nil
		}),
		dagql.Func("get", func(
			context.Context,
			*artifactTraversalTestsValue,
			struct{ Key dagql.String },
		) (*artifactTraversalTestValue, error) {
			return &artifactTraversalTestValue{}, nil
		}),
	}.Install(srv)
	dagql.Fields[*artifactTraversalTestValue]{}.Install(srv)

	var artifactCount dagql.Int
	err = srv.Select(
		ctx,
		srv.Root(),
		&artifactCount,
		dagql.Selector{Field: "runArtifactTraversal"},
	)
	require.NoError(t, err)
	require.Equal(t, dagql.Int(15), artifactCount)
	require.Equal(t, int64(1), counts.root.Load())
	require.Equal(t, int64(1), counts.modules.Load())
	require.Equal(t, int64(2), counts.moduleGet.Load())
	require.Equal(t, int64(2), counts.testDirectories.Load())
	require.Equal(t, int64(4), counts.directoryGet.Load())
	require.Equal(t, int64(4), counts.tests.Load())
}

func artifactTraversalTypeDefs(t *testing.T) dagql.ObjectResultArray[*TypeDef] {
	b := newCollectionTestBuilder(t)
	testType := b.object("ArtifactTraversalTest")
	testsType := b.collection(
		"ArtifactTraversalTests",
		"keys",
		"get",
		b.primitive(TypeDefKindString),
		testType,
	)
	directoryType := b.object("ArtifactTraversalDirectory")
	objectTypeDef(directoryType).Functions = append(
		objectTypeDef(directoryType).Functions,
		b.function("tests", testsType, "", nil),
	)
	directoriesType := b.collection(
		"ArtifactTraversalDirectories",
		"keys",
		"get",
		b.primitive(TypeDefKindString),
		directoryType,
	)
	moduleType := b.object("ArtifactTraversalModule")
	objectTypeDef(moduleType).Functions = append(
		objectTypeDef(moduleType).Functions,
		b.function("testDirectories", directoriesType, "", nil),
	)
	modulesType := b.collection(
		"ArtifactTraversalModules",
		"keys",
		"get",
		b.primitive(TypeDefKindString),
		moduleType,
	)
	rootType := b.object("ArtifactTraversalRoot")
	objectTypeDef(rootType).Functions = append(
		objectTypeDef(rootType).Functions,
		b.function("modules", modulesType, "", nil),
	)

	for _, collectionType := range []*TypeDef{testsType, directoriesType, modulesType} {
		require.NoError(t, (&Module{}).validateCollectionTypeDef(collectionType))
	}
	typeDefs := artifactTestTypeDefs(t, &Function{
		Name:             "artifactTraversal",
		SourceModuleName: "test",
		ReturnType:       b.typeDefResult(rootType),
	})
	for _, typeDef := range []*TypeDef{
		rootType,
		modulesType,
		moduleType,
		directoriesType,
		directoryType,
		testsType,
		testType,
	} {
		typeDefs = append(typeDefs, b.typeDefResult(typeDef))
	}
	return typeDefs
}
