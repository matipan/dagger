package core

import (
	"testing"

	"github.com/dagger/dagger/dagql"
	"github.com/stretchr/testify/require"
)

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
