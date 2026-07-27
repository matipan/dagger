package core

import (
	"fmt"
	"testing"

	"github.com/dagger/dagger/dagql"
	"github.com/stretchr/testify/require"
)

type collectionTestBuilder struct {
	t     *testing.T
	dag   *dagql.Server
	index int
}

func newCollectionTestBuilder(t *testing.T) *collectionTestBuilder {
	return &collectionTestBuilder{t: t, dag: newTypeDefTestDag(t)}
}

func collectionTestResult[T dagql.Typed](
	b *collectionTestBuilder,
	label string,
	value T,
) dagql.ObjectResult[T] {
	b.index++
	return newTypeDefDetachedResult(
		b.t,
		b.dag,
		fmt.Sprintf("collection-%d-%s", b.index, label),
		value,
	)
}

func (b *collectionTestBuilder) typeDefResult(typeDef *TypeDef) dagql.ObjectResult[*TypeDef] {
	return collectionTestResult(b, "typedef", typeDef)
}

func (b *collectionTestBuilder) primitive(kind TypeDefKind) *TypeDef {
	return (&TypeDef{}).WithKind(kind)
}

func (b *collectionTestBuilder) object(name string) *TypeDef {
	object := NewObjectTypeDef(name, "", nil)
	return (&TypeDef{}).WithObject(collectionTestResult(b, "object", object))
}

func (b *collectionTestBuilder) list(element *TypeDef) *TypeDef {
	list := &ListTypeDef{ElementTypeDef: b.typeDefResult(element)}
	return (&TypeDef{}).WithListOf(collectionTestResult(b, "list", list))
}

func (b *collectionTestBuilder) field(
	name string,
	typeDef *TypeDef,
) dagql.ObjectResult[*FieldTypeDef] {
	return collectionTestResult(b, "field", &FieldTypeDef{
		Name:         name,
		OriginalName: name,
		TypeDef:      b.typeDefResult(typeDef),
	})
}

func (b *collectionTestBuilder) function(
	name string,
	returnType *TypeDef,
	argName string,
	argType *TypeDef,
) dagql.ObjectResult[*Function] {
	fn := &Function{
		Name:         gqlFieldName(name),
		OriginalName: name,
		ReturnType:   b.typeDefResult(returnType),
	}
	if argName != "" {
		fn.Args = append(fn.Args, collectionTestResult(b, "arg", &FunctionArg{
			Name:         gqlFieldName(argName),
			OriginalName: argName,
			TypeDef:      b.typeDefResult(argType),
		}))
	}
	return collectionTestResult(b, "function", fn)
}

func (b *collectionTestBuilder) collection(
	name string,
	keysName string,
	getName string,
	keyType *TypeDef,
	returnType *TypeDef,
) *TypeDef {
	object := NewObjectTypeDef(name, "", nil)
	if keysName != "" {
		object.Fields = append(object.Fields, b.field(keysName, b.list(keyType)))
	}
	if getName != "" {
		object.Functions = append(
			object.Functions,
			b.function(getName, returnType, "name", keyType),
		)
	}
	return (&TypeDef{}).
		WithObject(collectionTestResult(b, "collection-object", object)).
		WithCollection()
}

func TestValidateCollectionTypeDefDefaultMembers(t *testing.T) {
	b := newCollectionTestBuilder(t)
	collectionType := b.collection(
		"GoTests",
		"keys",
		"get",
		b.primitive(TypeDefKindString),
		b.object("GoTest"),
	)

	require.NoError(t, (&Module{}).validateCollectionTypeDef(collectionType))
	collection := collectionType.AsCollection.Value
	require.Equal(t, "keys", collection.KeysFieldName)
	require.Equal(t, "get", collection.GetFunctionName)
	require.Equal(t, "name", collection.GetArgName)
	require.Equal(t, TypeDefKindString, collection.KeyType.Kind)
	require.Equal(t, "GoTest", objectTypeDef(collection.ValueType).Name)
}

func TestValidateCollectionTypeDefExplicitOverrides(t *testing.T) {
	b := newCollectionTestBuilder(t)
	collectionType := b.collection(
		"GoModules",
		"paths",
		"module",
		b.primitive(TypeDefKindString),
		b.object("GoModule"),
	)
	var err error
	collectionType, err = collectionType.WithCollectionKeys("paths")
	require.NoError(t, err)
	collectionType, err = collectionType.WithCollectionGet("module")
	require.NoError(t, err)

	require.NoError(t, (&Module{}).validateCollectionTypeDef(collectionType))
	collection := collectionType.AsCollection.Value
	require.Equal(t, "paths", collection.KeysFieldName)
	require.Equal(t, "module", collection.GetFunctionName)
	require.Equal(t, "name", collection.GetArgName)
}

func TestCollectionOverridesRequireExplicitCollectionAndAreUnique(t *testing.T) {
	plain := newCollectionTestBuilder(t).object("GoTests")

	_, err := plain.WithCollectionKeys("names")
	require.EqualError(t, err, "collection keys override requires a collection type")
	_, err = plain.WithCollectionGet("lookup")
	require.EqualError(t, err, "collection get override requires a collection type")

	collection := plain.WithCollection()
	collection, err = collection.WithCollectionKeys("names")
	require.NoError(t, err)
	_, err = collection.WithCollectionKeys("otherNames")
	require.EqualError(t, err, "collection keys override is already set")

	collection, err = collection.WithCollectionGet("lookup")
	require.NoError(t, err)
	_, err = collection.WithCollectionGet("otherLookup")
	require.EqualError(t, err, "collection get override is already set")
}

func TestValidateCollectionTypeDefErrors(t *testing.T) {
	tests := []struct {
		name     string
		build    func(*collectionTestBuilder) *TypeDef
		contains string
	}{
		{
			name: "missing keys field",
			build: func(b *collectionTestBuilder) *TypeDef {
				return b.collection(
					"GoTests",
					"",
					"get",
					b.primitive(TypeDefKindString),
					b.object("GoTest"),
				)
			},
			contains: "must define exactly one effective keys field",
		},
		{
			name: "get key type mismatch",
			build: func(b *collectionTestBuilder) *TypeDef {
				def := b.collection(
					"GoTests",
					"keys",
					"",
					b.primitive(TypeDefKindString),
					b.object("GoTest"),
				)
				objectTypeDef(def).Functions = append(
					objectTypeDef(def).Functions,
					b.function(
						"get",
						b.object("GoTest"),
						"name",
						b.primitive(TypeDefKindInteger),
					),
				)
				return def
			},
			contains: "must match keys field type",
		},
		{
			name: "get returns non object",
			build: func(b *collectionTestBuilder) *TypeDef {
				return b.collection(
					"GoTests",
					"keys",
					"get",
					b.primitive(TypeDefKindString),
					b.primitive(TypeDefKindString),
				)
			},
			contains: "must return an object",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := (&Module{}).validateCollectionTypeDef(tt.build(newCollectionTestBuilder(t)))
			require.ErrorContains(t, err, tt.contains)
		})
	}
}
