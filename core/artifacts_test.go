package core

import (
	"testing"

	"github.com/dagger/dagger/dagql"
	"github.com/stretchr/testify/require"
)

func TestArtifactsDetectTopLevelObjects(t *testing.T) {
	typeDefs := artifactTestTypeDefs(t,
		artifactTestRoot(t, "go", "Go"),
		artifactTestRoot(t, "sdkRelease", "SdkRelease"),
		artifactTestRoot(t, "cliRelease", "CliRelease"),
	)
	typeDefs = append(typeDefs, artifactTestObject(t, "StructuralGlue"))

	artifacts := NewArtifactsFromTypeDefs(typeDefs)
	require.Equal(t, []string{"cli-release", "go", "sdk-release"}, artifactCoordinates(t, artifacts))
	require.Equal(t, []string{ArtifactTypeDimension}, artifactDimensionNames(artifacts))
	require.Equal(t, TypeDefKindString, artifacts.Dimensions()[0].KeyType.Kind)
}

func TestArtifactsIgnoreCoreAndNonObjectQueryFields(t *testing.T) {
	typeDefs := artifactTestTypeDefs(t,
		&Function{
			Name:       "coreObject",
			ReturnType: artifactTestObject(t, "CoreObject"),
		},
		&Function{
			Name:             "moduleScalar",
			SourceModuleName: "module",
			ReturnType:       artifactTestTypeDef(t, "artifactModuleScalar", &TypeDef{Kind: TypeDefKindString}),
		},
	)

	require.Empty(t, NewArtifactsFromTypeDefs(typeDefs).Items())
}

func TestArtifactsFilterCoordinates(t *testing.T) {
	artifacts := NewArtifactsFromTypeDefs(artifactTestTypeDefs(t,
		artifactTestRoot(t, "go", "Go"),
		artifactTestRoot(t, "js", "Js"),
		artifactTestRoot(t, "goTest", "GoTest"),
	))

	filtered, err := artifacts.FilterCoordinates(ArtifactTypeDimension, []string{"go", "js"})
	require.NoError(t, err)
	require.Equal(t, []string{"go", "js"}, artifactCoordinates(t, filtered))
	require.Equal(t, []string{"go", "go-test", "js"}, artifactCoordinates(t, artifacts))

	filtered, err = filtered.FilterCoordinates(ArtifactTypeDimension, []string{"js"})
	require.NoError(t, err)
	require.Equal(t, []string{"js"}, artifactCoordinates(t, filtered))
	require.Equal(t, []string{ArtifactTypeDimension}, artifactDimensionNames(filtered))

	_, err = artifacts.FilterCoordinates(ArtifactTypeDimension, nil)
	require.EqualError(t, err, "values must not be empty")

	_, err = artifacts.FilterCoordinates("missing", []string{"value"})
	require.EqualError(t, err, `artifact dimension "missing" not found`)
}

func TestArtifactsFilterDimensionAndScope(t *testing.T) {
	artifacts := NewArtifactsFromTypeDefs(artifactTestTypeDefs(t,
		artifactTestRoot(t, "go", "Go"),
	))

	filtered, err := artifacts.FilterDimension(ArtifactTypeDimension)
	require.NoError(t, err)
	require.Equal(t, []string{"go"}, artifactCoordinates(t, filtered))

	item := filtered.Items()[0]
	require.Same(t, filtered, item.Scope())
	require.Equal(t, item.Coordinates()[0], mustArtifactCoordinate(t, item, ArtifactTypeDimension))

	_, err = artifacts.FilterDimension("missing")
	require.EqualError(t, err, `artifact dimension "missing" not found`)

	_, err = item.Coordinate("missing")
	require.EqualError(t, err, `artifact dimension "missing" not found`)
}

func artifactTestTypeDefs(t *testing.T, queryFunctions ...*Function) dagql.ObjectResultArray[*TypeDef] {
	t.Helper()
	dag := newTypeDefTestDag(t)
	functions := make(dagql.ObjectResultArray[*Function], len(queryFunctions))
	for i, fn := range queryFunctions {
		functions[i] = newTypeDefDetachedResult(t, dag, "artifactQueryFunction-"+fn.Name, fn)
	}
	query := newTypeDefDetachedResult(t, dag, "artifactQuery", &ObjectTypeDef{
		Name:      "Query",
		Functions: functions,
	})
	return dagql.ObjectResultArray[*TypeDef]{
		newTypeDefDetachedResult(t, dag, "artifactQueryTypeDef", &TypeDef{
			Kind:     TypeDefKindObject,
			AsObject: dagql.NonNull(query),
		}),
	}
}

func artifactTestRoot(t *testing.T, name, typeName string) *Function {
	t.Helper()
	return &Function{
		Name:             name,
		SourceModuleName: "module",
		ReturnType:       artifactTestObject(t, typeName),
	}
}

func artifactTestObject(t *testing.T, name string) dagql.ObjectResult[*TypeDef] {
	t.Helper()
	dag := newTypeDefTestDag(t)
	obj := newTypeDefDetachedResult(t, dag, "artifactObject-"+name, &ObjectTypeDef{Name: name})
	return newTypeDefDetachedResult(t, dag, "artifactObjectTypeDef-"+name, &TypeDef{
		Kind:     TypeDefKindObject,
		AsObject: dagql.NonNull(obj),
	})
}

func artifactTestTypeDef(t *testing.T, op string, typeDef *TypeDef) dagql.ObjectResult[*TypeDef] {
	t.Helper()
	return newTypeDefDetachedResult(t, newTypeDefTestDag(t), op, typeDef)
}

func artifactDimensionNames(artifacts *Artifacts) []string {
	dimensions := artifacts.Dimensions()
	names := make([]string, len(dimensions))
	for i, dimension := range dimensions {
		names[i] = dimension.Name
	}
	return names
}

func artifactCoordinates(t *testing.T, artifacts *Artifacts) []string {
	t.Helper()
	items := artifacts.Items()
	coordinates := make([]string, len(items))
	for i, item := range items {
		coordinate := mustArtifactCoordinate(t, item, ArtifactTypeDimension)
		require.True(t, coordinate.Valid)
		coordinates[i] = coordinate.Value.String()
	}
	return coordinates
}

func mustArtifactCoordinate(t *testing.T, artifact *Artifact, name string) dagql.Nullable[dagql.String] {
	t.Helper()
	coordinate, err := artifact.Coordinate(name)
	require.NoError(t, err)
	return coordinate
}
