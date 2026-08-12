package core

import (
	"testing"

	"github.com/dagger/dagger/dagql"
	"github.com/dagger/dagger/engine"
	"github.com/stretchr/testify/require"
)

func TestModuleTypeDefsProjectCollectionsUnderCurrentCall(t *testing.T) {
	ctx := t.Context()
	cache, err := dagql.NewCache(ctx, "", nil, nil)
	require.NoError(t, err)
	ctx = dagql.ContextWithCache(ctx, cache)
	ctx = engine.ContextWithClientMetadata(ctx, &engine.ClientMetadata{
		ClientID:  "collection-projection-client",
		SessionID: "collection-projection-session",
	})

	b := newCollectionTestBuilder(t)
	elementType := newTypeDefAttachedResult(
		t,
		ctx,
		cache,
		b.dag,
		"projection-list-element",
		(&TypeDef{}).WithKind(TypeDefKindString),
	)
	listType := newTypeDefAttachedResult(
		t,
		ctx,
		cache,
		b.dag,
		"projection-list",
		&ListTypeDef{ElementTypeDef: elementType},
	)
	optionalListType := newTypeDefAttachedResult(
		t,
		ctx,
		cache,
		b.dag,
		"projection-optional-list",
		(&TypeDef{}).WithListOf(listType).WithOptional(true),
	)
	mod := &Module{
		ObjectDefs: dagql.ObjectResultArray[*TypeDef]{optionalListType},
	}

	// Type definition projection can run while an enclosing resolver call is
	// still in flight. Projection results must derive from the attached module
	// type definitions, not from this unattached current call.
	inFlight := moduleObjectTestSyntheticCall(
		"projection-in-flight-current-call",
		&TypeDef{},
	)
	ctx = dagql.ContextWithCall(ctx, inFlight)
	typeDefs, err := mod.TypeDefs(ctx, b.dag)
	require.NoError(t, err)
	require.Len(t, typeDefs, 1)
	require.True(t, typeDefs[0].Self().Optional)

	var normalized dagql.ObjectResult[*TypeDef]
	err = b.dag.Select(ctx, typeDefs[0], &normalized, dagql.Selector{
		Field: "withOptional",
		Args: []dagql.NamedInput{
			{Name: "optional", Value: dagql.Boolean(false)},
		},
	})
	require.NoError(t, err)
	require.False(t, normalized.Self().Optional)
	require.True(t, normalized.Self().AsList.Valid)
	listID, err := normalized.Self().AsList.Value.ID()
	require.NoError(t, err)
	require.NotZero(t, listID.EngineResultID())
}

func TestModuleTypeDefsProjectionIdentityIncludesAllModuleTypeDefs(t *testing.T) {
	b := newCollectionTestBuilder(t)
	first := b.object("First")
	second := b.object("Second")
	mod := &Module{
		NameField: "projection-identity",
		ObjectDefs: dagql.ObjectResultArray[*TypeDef]{
			b.typeDefResult(first),
			b.typeDefResult(second),
		},
	}

	initial, err := mod.TypeDefs(t.Context(), b.dag)
	require.NoError(t, err)
	require.Len(t, initial, 2)
	initialCall, err := initial[0].ResultCall()
	require.NoError(t, err)
	initialDigest := resultCallStringArg(t, initialCall, "projectionDigest")

	changedSecond := b.object("ChangedSecond")
	mod.ObjectDefs[1] = b.typeDefResult(changedSecond)
	changed, err := mod.TypeDefs(t.Context(), b.dag)
	require.NoError(t, err)
	require.Len(t, changed, 2)
	changedCall, err := changed[0].ResultCall()
	require.NoError(t, err)
	changedDigest := resultCallStringArg(t, changedCall, "projectionDigest")

	require.NotEqual(t, initialDigest, changedDigest)
}

func resultCallStringArg(t *testing.T, frame *dagql.ResultCall, name string) string {
	t.Helper()
	for _, arg := range frame.Args {
		if arg.Name == name && arg.Value != nil {
			return arg.Value.StringValue
		}
	}
	require.FailNow(t, "result call argument not found", name)
	return ""
}

func TestModuleTypeDefsProjectCollections(t *testing.T) {
	b := newCollectionTestBuilder(t)
	itemType := b.object("GoTest")
	collectionType := b.collection(
		"GoTests",
		"names",
		"test",
		b.primitive(TypeDefKindString),
		itemType,
	)
	var err error
	collectionType, err = collectionType.WithCollectionKeys("names")
	require.NoError(t, err)
	collectionType, err = collectionType.WithCollectionGet("test")
	require.NoError(t, err)
	objectTypeDef(collectionType).Functions = append(
		objectTypeDef(collectionType).Functions,
		b.function(
			"runTests",
			b.primitive(TypeDefKindString),
			"",
			nil,
		),
	)
	require.NoError(t, (&Module{}).validateCollectionTypeDef(collectionType))

	rootType := b.object("Go")
	objectTypeDef(rootType).Fields = append(
		objectTypeDef(rootType).Fields,
		b.field("tests", collectionType),
	)

	mod := &Module{
		NameField:    "go",
		OriginalName: "Go",
		Deps:         NewSchemaBuilder(nil, nil),
		ObjectDefs: dagql.ObjectResultArray[*TypeDef]{
			b.typeDefResult(rootType),
			b.typeDefResult(itemType),
			b.typeDefResult(collectionType),
		},
	}
	typeDefs, err := mod.TypeDefs(t.Context(), b.dag)
	require.NoError(t, err)

	typeByName := map[string]*TypeDef{}
	for _, typeDefResult := range typeDefs {
		typeDef := typeDefResult.Self()
		require.NotEmpty(t, typeDef.Name)
		if object := objectTypeDef(typeDef); object != nil {
			typeByName[object.Name] = typeDef
		}
	}

	projectedCollection := typeByName["GoTests"]
	require.NotNil(t, projectedCollection)
	require.True(t, projectedCollection.AsCollection.Valid)

	projectedObj := objectTypeDef(projectedCollection)
	require.Len(t, projectedObj.Fields, 3)
	require.Equal(t, "keys", projectedObj.Fields[0].Self().Name)
	require.Equal(t, "list", projectedObj.Fields[1].Self().Name)
	require.Equal(t, "batch", projectedObj.Fields[2].Self().Name)
	require.Len(t, projectedObj.Functions, 2)
	require.Equal(t, "get", projectedObj.Functions[0].Self().Name)
	require.Equal(t, "key", projectedObj.Functions[0].Self().Args[0].Self().Name)
	require.Equal(t, "subset", projectedObj.Functions[1].Self().Name)

	batchType := objectTypeDef(projectedCollection.AsCollection.Value.BatchType)
	require.NotNil(t, batchType)
	require.Equal(t, "GoTests_Batch", batchType.Name)
	projectedBatch := objectTypeDef(typeByName["GoTests_Batch"])
	require.NotNil(t, projectedBatch)
	require.Len(t, projectedBatch.Functions, 1)
	require.Equal(t, "runTests", projectedBatch.Functions[0].Self().Name)

	rootProjected := objectTypeDef(typeByName["Go"])
	require.NotNil(t, rootProjected)
	require.True(t, rootProjected.IsMainObject)
	require.False(t, projectedObj.IsMainObject)
	testsField, ok := rootProjected.FieldByName("tests")
	require.True(t, ok)
	require.True(t, testsField.TypeDef.Self().AsCollection.Valid)
	require.Equal(t, "GoTests", objectTypeDef(testsField.TypeDef.Self()).Name)
}
