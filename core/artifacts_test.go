package core

import (
	"strings"
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

	artifacts := mustArtifactsFromTypeDefs(t, typeDefs)
	require.Equal(t, []string{"cli-release", "go", "sdk-release"}, artifactCoordinates(t, artifacts))
	require.Equal(t, []string{ArtifactTypeDimension}, artifactDimensionNames(artifacts))
	require.Equal(t, TypeDefKindString, artifacts.Dimensions()[0].KeyType.Kind)
}

func TestArtifactsNamespaceAdditionalTopLevelObjectTypes(t *testing.T) {
	e2e := artifactTestNamedObject(t, "E2E", "E2E", "e2e")
	objectTypeDef(e2e.Self()).IsMainObject = true
	sdkDev := artifactTestNamedObject(t, "E2ESdkdev", "SDKDev", "e2e")
	setArtifactTestFields(t, e2e, artifactTestField(t, "sdkDev", sdkDev))
	typeDefs := artifactTestTypeDefs(t,
		&Function{
			Name:             "e2e",
			SourceModuleName: "e2e",
			ReturnType:       artifactTestObject(t, "E2E"),
		},
		&Function{
			Name:             "sdkDev",
			SourceModuleName: "e2e",
			ReturnType:       artifactTestObject(t, "E2ESdkdev"),
		},
	)
	typeDefs = append(typeDefs, e2e, sdkDev)

	artifacts := mustArtifactsFromTypeDefs(t, typeDefs)
	require.Equal(
		t,
		[]string{"e2e", "e2e-sdk-dev"},
		artifactCoordinates(t, artifacts),
	)
	require.Equal(t, []string{ArtifactTypeDimension}, artifactDimensionNames(artifacts))
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

	require.Empty(t, mustArtifactsFromTypeDefs(t, typeDefs).Items())
}

func TestArtifactsFilterCoordinates(t *testing.T) {
	artifacts := mustArtifactsFromTypeDefs(t, artifactTestTypeDefs(t,
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

func TestArtifactsDiscoverCollectionDimensions(t *testing.T) {
	b := newCollectionTestBuilder(t)
	itemType := b.object("GoTest")
	collectionType := b.collection(
		"GoTests",
		"keys",
		"get",
		b.primitive(TypeDefKindString),
		itemType,
	)
	require.NoError(t, (&Module{}).validateCollectionTypeDef(collectionType))

	goType := b.object("Go")
	objectTypeDef(goType).Fields = append(
		objectTypeDef(goType).Fields,
		b.field("tests", collectionType),
	)
	typeDefs := artifactTestTypeDefs(t, &Function{
		Name:             "go",
		SourceModuleName: "module",
		ReturnType:       b.typeDefResult(goType),
	})
	typeDefs = append(
		typeDefs,
		b.typeDefResult(goType),
		b.typeDefResult(collectionType),
		b.typeDefResult(itemType),
	)

	artifacts := mustArtifactsFromTypeDefs(t, typeDefs)
	require.Equal(t, []string{"type", "go-test"}, artifactDimensionNames(artifacts))
	require.Equal(t, TypeDefKindString, artifacts.dimensions[1].KeyType.Kind)
	require.Equal(t, []string{"go-tests"}, artifacts.dimensions[1].collectionTypes)
	require.False(t, artifacts.rows[0].coordinates[1].Valid)
}

func TestArtifactsExposeNamespacedCollectionAliases(t *testing.T) {
	b := newCollectionTestBuilder(t)
	itemType := b.object("E2ETestsuite")
	itemObject := objectTypeDef(itemType)
	itemObject.OriginalName = "TestSuite"
	itemObject.SourceModuleName = "e2e"
	collectionType := b.collection(
		"E2ETestsuites",
		"keys",
		"get",
		b.primitive(TypeDefKindString),
		itemType,
	)
	collectionObject := objectTypeDef(collectionType)
	collectionObject.OriginalName = "TestSuites"
	collectionObject.SourceModuleName = "e2e"
	require.NoError(t, (&Module{}).validateCollectionTypeDef(collectionType))

	e2eType := artifactTestNamedObject(t, "E2E", "E2E", "e2e")
	objectTypeDef(e2eType.Self()).IsMainObject = true
	objectTypeDef(e2eType.Self()).Fields = append(
		objectTypeDef(e2eType.Self()).Fields,
		b.field("testSuites", collectionType),
	)
	typeDefs := artifactTestTypeDefs(t, &Function{
		Name:             "e2e",
		SourceModuleName: "e2e",
		ReturnType:       e2eType,
	})
	typeDefs = append(
		typeDefs,
		e2eType,
		b.typeDefResult(collectionType),
		b.typeDefResult(itemType),
	)

	artifacts := mustArtifactsFromTypeDefs(t, typeDefs)
	require.Equal(
		t,
		[]string{"type", "e2e-test-suite"},
		artifactDimensionNames(artifacts),
	)
	require.Equal(
		t,
		[]string{"e2e-test-suites"},
		artifacts.Dimensions()[1].CollectionTypes(),
	)
}

func TestArtifactsDiscoverStaticFieldOccurrences(t *testing.T) {
	testSuite := artifactTestNamedObject(t, "E2eTestSuite", "TestSuite", "e2e")
	sdkDev := artifactTestNamedObject(t, "E2eSdkdev", "SDKDev", "e2e")
	e2e := artifactTestNamedObject(t, "E2e", "E2E", "e2e")

	setArtifactTestFields(t, sdkDev,
		artifactTestField(t, "go", testSuite),
		artifactTestField(t, "python", testSuite),
	)
	setArtifactTestFields(t, e2e,
		artifactTestField(t, "engine", testSuite),
		artifactTestField(t, "cli", testSuite),
		artifactTestField(t, "sdks", sdkDev),
		artifactTestField(t, "sdksarm", sdkDev),
	)

	typeDefs := artifactTestTypeDefs(t, &Function{
		Name:             "e2e",
		SourceModuleName: "e2e",
		ReturnType:       e2e,
	})
	typeDefs = append(typeDefs, e2e, sdkDev, testSuite)

	artifacts := mustArtifactsFromTypeDefs(t, typeDefs)
	require.Equal(t,
		[]string{"type", "e2e-test-suite", "e2e-sdk-dev"},
		artifactDimensionNames(artifacts),
	)
	require.Equal(t, []string{
		"e2e||",
		"e2e-sdk-dev||e2e:sdks",
		"e2e-sdk-dev||e2e:sdksarm",
		"e2e-test-suite|e2e:cli|",
		"e2e-test-suite|e2e:engine|",
		"e2e-test-suite|sdk-dev:go|e2e:sdks",
		"e2e-test-suite|sdk-dev:go|e2e:sdksarm",
		"e2e-test-suite|sdk-dev:python|e2e:sdks",
		"e2e-test-suite|sdk-dev:python|e2e:sdksarm",
	}, artifactCoordinateRows(artifacts))

	filtered, err := artifacts.FilterCoordinates(
		"e2e-test-suite",
		[]string{"sdk-dev:go"},
	)
	require.NoError(t, err)
	require.Equal(t, []string{
		"e2e-test-suite|sdk-dev:go|e2e:sdks",
		"e2e-test-suite|sdk-dev:go|e2e:sdksarm",
	}, artifactCoordinateRows(filtered))

	filtered, err = filtered.FilterCoordinates(
		"e2e-sdk-dev",
		[]string{"e2e:sdksarm"},
	)
	require.NoError(t, err)
	require.Equal(t, []string{
		"e2e-test-suite|sdk-dev:go|e2e:sdksarm",
	}, artifactCoordinateRows(filtered))
}

func TestArtifactsRejectRepeatedStaticArtifactType(t *testing.T) {
	loop := artifactTestNamedObject(t, "Loop", "Loop", "loop")
	setArtifactTestFields(t, loop, artifactTestField(t, "next", loop))
	root := artifactTestNamedObject(t, "LoopRoot", "LoopRoot", "loop")
	objectTypeDef(root.Self()).IsMainObject = true
	setArtifactTestFields(t, root, artifactTestField(t, "loop", loop))
	typeDefs := artifactTestTypeDefs(t, &Function{
		Name:             "loop",
		SourceModuleName: "loop",
		ReturnType:       artifactTestObject(t, "LoopRoot"),
	})
	typeDefs = append(typeDefs, root, loop)

	_, err := NewArtifactsFromTypeDefs(typeDefs)
	require.EqualError(
		t,
		err,
		`static artifact field Loop.next repeats artifact dimension "loop"`,
	)
}

func TestArtifactsIgnoreOptionalObjectFields(t *testing.T) {
	testSuite := artifactTestNamedObject(t, "E2ETestSuite", "TestSuite", "e2e")
	optionalTestSuite := artifactTestTypeDef(
		t,
		"optional-test-suite",
		testSuite.Self().Clone(),
	)
	optionalTestSuite.Self().Optional = true
	e2e := artifactTestNamedObject(t, "E2E", "E2E", "e2e")
	objectTypeDef(e2e.Self()).IsMainObject = true
	setArtifactTestFields(t, e2e, artifactTestField(t, "engine", optionalTestSuite))

	typeDefs := artifactTestTypeDefs(t, &Function{
		Name:             "e2e",
		SourceModuleName: "e2e",
		ReturnType:       e2e,
	})
	typeDefs = append(typeDefs, e2e, testSuite)

	artifacts := mustArtifactsFromTypeDefs(t, typeDefs)
	require.Equal(t, []string{"type"}, artifactDimensionNames(artifacts))
	require.Equal(t, []string{"e2e"}, artifactCoordinates(t, artifacts))
}

func TestArtifactCollectionCoordinateEscapesFieldTargets(t *testing.T) {
	require.Equal(t, "fluffy", artifactCollectionCoordinate("fluffy"))
	require.Equal(t, "sdk-dev%3Ago", artifactCollectionCoordinate("sdk-dev:go"))
	require.Equal(t, "already%25encoded", artifactCollectionCoordinate("already%encoded"))
}

func TestArtifactsFilterDimensionAndScope(t *testing.T) {
	artifacts := mustArtifactsFromTypeDefs(t, artifactTestTypeDefs(t,
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

func TestArtifactHasCoordinateDistinguishesEmptyFromNull(t *testing.T) {
	scope := &Artifacts{
		dimensions: []*ArtifactDimension{
			{Name: ArtifactTypeDimension, KeyType: &TypeDef{Kind: TypeDefKindString}},
			{Name: "go-test", KeyType: &TypeDef{Kind: TypeDefKindString}},
			{Name: "go-module", KeyType: &TypeDef{Kind: TypeDefKindString}},
		},
	}
	artifact := &Artifact{
		scope: scope,
		coordinates: []dagql.Nullable[dagql.String]{
			dagql.NonNull(dagql.String("go-test")),
			dagql.NonNull(dagql.String("")),
			dagql.Null[dagql.String](),
		},
	}

	hasCoordinate, err := artifact.HasCoordinate("go-test")
	require.NoError(t, err)
	require.True(t, hasCoordinate)
	coordinate, err := artifact.Coordinate("go-test")
	require.NoError(t, err)
	require.True(t, coordinate.Valid)
	require.Empty(t, coordinate.Value.String())

	hasCoordinate, err = artifact.HasCoordinate("go-module")
	require.NoError(t, err)
	require.False(t, hasCoordinate)
}

func TestWorkspaceArtifactsMaterializeOnce(t *testing.T) {
	artifacts := NewWorkspaceArtifacts(&Workspace{}, nil)
	artifacts, err := artifacts.FilterCoordinates(ArtifactTypeDimension, []string{"go"})
	require.NoError(t, err)

	snapshot := mustArtifactsFromTypeDefs(t, artifactTestTypeDefs(t,
		artifactTestRoot(t, "go", "Go"),
		artifactTestRoot(t, "js", "Js"),
	))
	materialized, err := artifacts.Materialize(snapshot)
	require.NoError(t, err)
	require.Equal(t, []string{"go"}, artifactCoordinates(t, materialized))

	replacement := mustArtifactsFromTypeDefs(t, artifactTestTypeDefs(t,
		artifactTestRoot(t, "other", "Other"),
	))
	target, err := materialized.Materialize(replacement)
	require.NoError(t, err)
	require.Equal(t, []string{"go"}, artifactCoordinates(t, target))

	target, err = target.FilterCoordinates(ArtifactTypeDimension, []string{"js"})
	require.NoError(t, err)
	require.Empty(t, target.Items())
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
		ReturnType:       artifactTestMainObject(t, typeName),
	}
}

func artifactTestMainObject(t *testing.T, name string) dagql.ObjectResult[*TypeDef] {
	t.Helper()
	typeDef := artifactTestObject(t, name)
	objectTypeDef(typeDef.Self()).IsMainObject = true
	return typeDef
}

func artifactTestObject(t *testing.T, name string) dagql.ObjectResult[*TypeDef] {
	t.Helper()
	return artifactTestNamedObject(t, name, name, "module")
}

func artifactTestNamedObject(
	t *testing.T,
	name string,
	originalName string,
	sourceModuleName string,
) dagql.ObjectResult[*TypeDef] {
	t.Helper()
	dag := newTypeDefTestDag(t)
	obj := newTypeDefDetachedResult(t, dag, "artifactObject-"+name, &ObjectTypeDef{
		Name:             name,
		OriginalName:     originalName,
		SourceModuleName: sourceModuleName,
	})
	return newTypeDefDetachedResult(t, dag, "artifactObjectTypeDef-"+name, &TypeDef{
		Kind:     TypeDefKindObject,
		AsObject: dagql.NonNull(obj),
	})
}

func artifactTestField(
	t *testing.T,
	name string,
	typeDef dagql.ObjectResult[*TypeDef],
) *FieldTypeDef {
	t.Helper()
	return &FieldTypeDef{
		Name:         name,
		OriginalName: name,
		TypeDef:      typeDef,
	}
}

func setArtifactTestFields(
	t *testing.T,
	typeDef dagql.ObjectResult[*TypeDef],
	fields ...*FieldTypeDef,
) {
	t.Helper()
	object := typeDef.Self().AsObject.Value.Self()
	object.Fields = make(dagql.ObjectResultArray[*FieldTypeDef], len(fields))
	for i, field := range fields {
		object.Fields[i] = newTypeDefDetachedResult(
			t,
			newTypeDefTestDag(t),
			"artifactField-"+object.Name+"-"+field.Name,
			field,
		)
	}
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

func artifactCoordinateRows(artifacts *Artifacts) []string {
	rows := make([]string, len(artifacts.rows))
	for i, row := range artifacts.rows {
		coordinates := make([]string, len(row.coordinates))
		for j, coordinate := range row.coordinates {
			if coordinate.Valid {
				coordinates[j] = coordinate.Value.String()
			}
		}
		rows[i] = strings.Join(coordinates, "|")
	}
	return rows
}

func mustArtifactCoordinate(t *testing.T, artifact *Artifact, name string) dagql.Nullable[dagql.String] {
	t.Helper()
	coordinate, err := artifact.Coordinate(name)
	require.NoError(t, err)
	return coordinate
}

func mustArtifactsFromTypeDefs(
	t *testing.T,
	typeDefs dagql.ObjectResultArray[*TypeDef],
) *Artifacts {
	t.Helper()
	artifacts, err := NewArtifactsFromTypeDefs(typeDefs)
	require.NoError(t, err)
	return artifacts
}
