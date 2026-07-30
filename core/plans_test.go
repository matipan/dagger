package core

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/dagger/dagger/dagql"
	"github.com/dagger/dagger/dagql/call"
	"github.com/stretchr/testify/require"
)

func TestArtifactActionsDiscoverArtifactRelativePaths(t *testing.T) {
	artifacts := planTestArtifacts(t)
	items := artifacts.Items()
	require.Len(t, items, 3)

	goArtifact := items[0]
	require.Equal(t, "go", mustArtifactCoordinate(t, goArtifact, ArtifactTypeDimension).Value.String())

	actions, err := goArtifact.Actions(nil)
	require.NoError(t, err)
	require.Equal(t, []string{
		"CHECK:lint",
		"CHECK:tests:unit",
		"GENERATE:generate",
	}, actionKeys(actions))

	checks, err := goArtifact.Actions([]Verb{VerbCheck})
	require.NoError(t, err)
	require.Equal(t, []string{"CHECK:lint", "CHECK:tests:unit"}, actionKeys(checks))

	stateArtifact := items[1]
	require.Equal(
		t,
		"go-toolchain-state",
		mustArtifactCoordinate(t, stateArtifact, ArtifactTypeDimension).Value.String(),
	)
	stateActions, err := stateArtifact.Actions([]Verb{VerbCheck})
	require.NoError(t, err)
	require.Equal(t, []string{"CHECK:validate"}, actionKeys(stateActions))
}

func TestArtifactActionsStopAtRequiredArgsAndArtifactBoundaries(t *testing.T) {
	artifacts := planTestArtifacts(t)
	goArtifact := artifacts.Items()[0]

	actions, err := goArtifact.Actions(nil)
	require.NoError(t, err)
	require.NotContains(t, actionKeys(actions), "CHECK:configured:validate")
	require.NotContains(t, actionKeys(actions), "CHECK:configured-check")
	require.NotContains(t, actionKeys(actions), "CHECK:sdk-release:publish")
}

func TestArtifactActionSelectsExactNormalizedPath(t *testing.T) {
	goArtifact := planTestArtifacts(t).Items()[0]

	action, err := goArtifact.Action(VerbCheck, []string{"Tests", "unit"})
	require.NoError(t, err)
	require.Equal(t, []string{"tests", "unit"}, action.FunctionPath())

	_, err = goArtifact.Action(VerbCheck, []string{"tests"})
	require.EqualError(t, err, `CHECK action "tests" not found on artifact "Go"`)
}

func TestActionWithAfterDeduplicatesHandleIDs(t *testing.T) {
	actionType := call.NewType((&Action{}).Type())
	first := dagql.NewID[*Action](call.NewEngineResultID(1, actionType))
	same := dagql.NewID[*Action](call.NewEngineResultID(1, actionType))
	second := dagql.NewID[*Action](call.NewEngineResultID(2, actionType))

	action := (&Action{target: planTestArtifacts(t)}).WithAfter(
		[]dagql.ID[*Action]{first, same, second},
	)
	require.Len(t, action.After(), 2)
}

func TestArtifactsPlanFiltersAndOrdersActions(t *testing.T) {
	artifacts := planTestArtifacts(t)

	plan, err := artifacts.Plan(
		VerbCheck,
		[]TargetPattern{"**"},
		[]TargetPattern{"tests:*"},
	)
	require.NoError(t, err)
	require.Equal(t, []string{
		"go:CHECK:lint",
		"go-toolchain-state:CHECK:validate",
		"sdk-release:CHECK:publish",
	}, planNodeKeys(t, plan))

	plan, err = artifacts.Plan(
		VerbCheck,
		[]TargetPattern{"tests:**"},
		nil,
	)
	require.NoError(t, err)
	require.Equal(t, []string{"go:CHECK:tests:unit"}, planNodeKeys(t, plan))

	plan, err = artifacts.Plan(
		VerbCheck,
		[]TargetPattern{"tests"},
		nil,
	)
	require.NoError(t, err)
	require.Equal(t, []string{"go:CHECK:tests:unit"}, planNodeKeys(t, plan))

	plan, err = artifacts.Plan(
		VerbCheck,
		[]TargetPattern{"go:lint"},
		nil,
	)
	require.NoError(t, err)
	require.Equal(t, []string{"go:CHECK:lint"}, planNodeKeys(t, plan))

	plan, err = artifacts.Plan(VerbCheck, []TargetPattern{"go"}, nil)
	require.NoError(t, err)
	require.Equal(t, []string{
		"go:CHECK:lint",
		"go:CHECK:tests:unit",
		"go-toolchain-state:CHECK:validate",
	}, planNodeKeys(t, plan))

	plan, err = artifacts.Plan(VerbCheck, []TargetPattern{"go:state"}, nil)
	require.NoError(t, err)
	require.Equal(t, []string{"go-toolchain-state:CHECK:validate"}, planNodeKeys(t, plan))
}

func TestArtifactsPlanAppliesSourceSpecificExcludes(t *testing.T) {
	artifacts := planTestArtifacts(t)

	plan, err := artifacts.PlanWithSourceExcludes(
		VerbCheck,
		nil,
		nil,
		map[string][]TargetPattern{
			"go-toolchain": {"lint"},
		},
	)
	require.NoError(t, err)
	require.Equal(t, []string{
		"go:CHECK:tests:unit",
		"go-toolchain-state:CHECK:validate",
		"sdk-release:CHECK:publish",
	}, planNodeKeys(t, plan))
}

func TestTargetPatternValidation(t *testing.T) {
	decoded, err := (TargetPattern("")).DecodeInput("Tests:**")
	require.NoError(t, err)
	require.Equal(t, TargetPattern("Tests:**"), decoded)

	_, err = (TargetPattern("")).DecodeInput("[")
	require.ErrorContains(t, err, `invalid target pattern "["`)

	action := &Action{
		functionPath: []string{"verify"},
		target: &Artifacts{
			dimensions: []*ArtifactDimension{{
				Name:    ArtifactTypeDimension,
				KeyType: &TypeDef{Kind: TypeDefKindString},
			}},
			rows: []*artifactRow{{
				coordinates: []dagql.Nullable[dagql.String]{
					dagql.NonNull(dagql.String("e2e-test-suite")),
				},
			}},
		},
	}
	require.True(t, matchesTargetPatterns(
		action,
		[]TargetPattern{"e2e-test-suite:verify"},
		false,
	))
	require.True(t, matchesTargetPatterns(
		action,
		[]TargetPattern{"e2e-test-suite"},
		false,
	))
}

func TestStaticArtifactGeneratorTarget(t *testing.T) {
	plan, err := planTestArtifacts(t).Plan(
		VerbGenerate,
		[]TargetPattern{"go:state"},
		nil,
	)
	require.NoError(t, err)
	require.Equal(
		t,
		[]string{"go-toolchain-state:GENERATE:sync"},
		planNodeKeys(t, plan),
	)
}

func TestPlanRunGraphExecutesSharedDependenciesOnce(t *testing.T) {
	prepare := &Action{verb: VerbCheck, functionPath: []string{"prepare"}}
	lint := &Action{verb: VerbCheck, functionPath: []string{"lint"}}
	test := &Action{verb: VerbCheck, functionPath: []string{"test"}}
	report := &Action{verb: VerbCheck, functionPath: []string{"report"}}
	after := map[*Action][]*Action{
		prepare: nil,
		lint:    {prepare},
		test:    {prepare},
		report:  {lint, test},
	}
	plan, dependencies, err := collectActionGraph(
		context.Background(),
		report,
		func(_ context.Context, action *Action) ([]*Action, error) {
			return after[action], nil
		},
	)
	require.NoError(t, err)
	require.Equal(t, []string{"report", "lint", "prepare", "test"}, actionNames(plan.nodes))

	counts := make([]int, len(plan.nodes))
	var mu sync.Mutex
	err = plan.runGraph(
		context.Background(),
		dependencies,
		false,
		func(_ context.Context, node int, _ *Action) error {
			mu.Lock()
			defer mu.Unlock()
			for _, dependency := range dependencies[node] {
				if counts[dependency] != 1 {
					return fmt.Errorf(
						"node %d started before dependency %d completed",
						node,
						dependency,
					)
				}
			}
			counts[node]++
			return nil
		},
	)
	require.NoError(t, err)
	require.Equal(t, []int{1, 1, 1, 1}, counts)
}

func TestCollectActionGraphRejectsSemanticSelfDependency(t *testing.T) {
	root := &Action{verb: VerbCheck, functionPath: []string{"lint"}}
	sameAction := &Action{verb: VerbCheck, functionPath: []string{"lint"}}

	_, _, err := collectActionGraph(
		context.Background(),
		root,
		func(_ context.Context, action *Action) ([]*Action, error) {
			if action == root {
				return []*Action{sameAction}, nil
			}
			return nil, nil
		},
	)
	require.EqualError(t, err, `CHECK action "lint": dependency cycle detected at node 0`)
}

func TestValidatePlanDependenciesRejectsCycles(t *testing.T) {
	err := validatePlanDependencies([][]int{
		{1},
		{2},
		{0},
	})
	require.EqualError(t, err, "dependency cycle detected at node 0")
}

func TestActionsEqualUsesTargetRowSet(t *testing.T) {
	target := planTestArtifacts(t)
	reordered := target.Clone()
	reordered.rows[0], reordered.rows[1] = reordered.rows[1], reordered.rows[0]

	left := &Action{
		verb:         VerbCheck,
		target:       target,
		functionPath: []string{"lint"},
	}
	right := &Action{
		verb:         VerbCheck,
		target:       reordered,
		functionPath: []string{"lint"},
	}
	require.True(t, actionsEqual(left, right))

	right.functionPath = []string{"test"}
	require.False(t, actionsEqual(left, right))
}

func TestCollectionPlanBatchesShadowedActions(t *testing.T) {
	artifacts := collectionPlanTestArtifacts(t)

	plan, err := artifacts.Plan(VerbCheck, nil, nil)
	require.NoError(t, err)
	require.Len(t, plan.nodes, 4)

	batches := map[string]*Action{}
	var itemKeys []string
	for _, node := range plan.nodes {
		switch node.displayName() {
		case "audit", "run":
			require.True(t, node.CollectionBatched())
			batches[node.displayName()] = node
		case "lint":
			require.False(t, node.CollectionBatched())
			require.Len(t, node.target.rows, 1)
			itemKeys = append(
				itemKeys,
				node.target.rows[0].coordinates[1].Value.String(),
			)
		default:
			require.Failf(t, "unexpected action", "%s", node.displayName())
		}
	}

	require.Equal(t, []string{"integration", "unit"}, itemKeys)
	require.Contains(t, batches, "audit")
	require.Contains(t, batches, "run")
	for _, batch := range batches {
		require.Len(t, batch.target.rows, 2)
		require.Equal(
			t,
			`go.tests.subset(keys: ["integration","unit"]).batch`,
			selectorPathString(batch.selectorPath),
		)
	}
}

func TestCollectionPlanTargetsBatchedActionsByType(t *testing.T) {
	artifacts := collectionPlanTestArtifacts(t)

	plan, err := artifacts.Plan(
		VerbCheck,
		[]TargetPattern{"go-test:run"},
		nil,
	)
	require.NoError(t, err)
	require.Len(t, plan.nodes, 1)
	require.True(t, plan.nodes[0].CollectionBatched())
	require.Equal(t, "run", plan.nodes[0].displayName())
	require.Len(t, plan.nodes[0].target.rows, 2)
}

func TestCollectionPlanDoesNotBatchStaticArtifacts(t *testing.T) {
	artifacts := collectionPlanTestArtifacts(t)
	artifacts.rows = append(artifacts.rows, &artifactRow{
		coordinates: []dagql.Nullable[dagql.String]{
			dagql.NonNull(dagql.NewString("go-test")),
			dagql.NonNull(dagql.NewString("go:engine")),
		},
		rootField:        "go",
		rootType:         "GoTest",
		sourceModuleName: "go",
		selectorPath: []dagql.Selector{
			{Field: "go"},
			{Field: "engine"},
		},
		targetPatterns: []string{"go", "go:engine"},
	})

	plan, err := artifacts.Plan(VerbCheck, nil, nil)
	require.NoError(t, err)
	require.Len(t, plan.nodes, 6)

	var staticActions []*Action
	var batches []*Action
	for _, node := range plan.nodes {
		if node.CollectionBatched() {
			batches = append(batches, node)
			continue
		}
		if node.target.rows[0].coordinates[1].Value.String() == "go:engine" {
			staticActions = append(staticActions, node)
		}
	}
	require.Equal(t, []string{"CHECK:lint", "CHECK:run"}, actionKeys(staticActions))
	require.Len(t, batches, 2)
	for _, batch := range batches {
		require.Len(t, batch.target.rows, 2)
		for _, row := range batch.target.rows {
			require.NotEqual(t, "go:engine", row.coordinates[1].Value.String())
		}
	}
}

func TestCollectionArtifactActionsRemainItemLocal(t *testing.T) {
	artifacts := collectionPlanTestArtifacts(t)

	for _, artifact := range artifacts.Items() {
		actions, err := artifact.Actions([]Verb{VerbCheck})
		require.NoError(t, err)
		require.Equal(t, []string{"CHECK:lint", "CHECK:run"}, actionKeys(actions))
		for _, action := range actions {
			require.False(t, action.CollectionBatched())
			require.Len(t, action.target.rows, 1)
		}

		run, err := artifact.Action(VerbCheck, []string{"run"})
		require.NoError(t, err)
		require.False(t, run.CollectionBatched())
		require.Len(t, run.target.rows, 1)
	}
}

func TestArtifactScopesDistinguishCollectionOccurrences(t *testing.T) {
	left := collectionPlanTestArtifacts(t)
	right := left.Clone()
	right.rows = right.rows[:1]
	left.rows = left.rows[:1]
	right.rows[0].selectorPath[1].Field = "otherTests"

	require.False(t, artifactScopesEqual(left, right))
}

func actionNames(actions []*Action) []string {
	names := make([]string, len(actions))
	for i, action := range actions {
		names[i] = action.displayName()
	}
	return names
}

func collectionPlanTestArtifacts(t *testing.T) *Artifacts {
	t.Helper()
	keyType := (&TypeDef{}).WithKind(TypeDefKindString)
	itemType := planTestObject(t, "GoTest",
		planTestAction(t, "lint", VerbCheck),
		planTestAction(t, "run", VerbCheck),
	)
	batchType := planTestObject(t, "GoTests_Batch",
		planTestAction(t, "audit", VerbCheck),
		planTestAction(t, "run", VerbCheck),
	)
	artifacts := &Artifacts{
		dimensions: []*ArtifactDimension{
			{
				Name:    ArtifactTypeDimension,
				KeyType: (&TypeDef{}).WithKind(TypeDefKindString),
			},
			{
				Name:    "go-test",
				KeyType: keyType,
			},
		},
		objects: map[string]*ObjectTypeDef{
			"GoTest":        objectTypeDef(itemType.Self()),
			"GoTests_Batch": objectTypeDef(batchType.Self()),
		},
		typeDefs: map[string]*TypeDef{},
	}
	for _, key := range []string{"integration", "unit"} {
		collectionPath := []dagql.Selector{
			{Field: "go"},
			{Field: "tests"},
		}
		itemPath := appendSelector(collectionPath, dagql.Selector{
			Field: collectionGetFunctionName,
			Args: []dagql.NamedInput{{
				Name:  collectionKeyArgName,
				Value: dagql.String(key),
			}},
		})
		artifacts.rows = append(artifacts.rows, &artifactRow{
			coordinates: []dagql.Nullable[dagql.String]{
				dagql.NonNull(dagql.String("go-test")),
				dagql.NonNull(dagql.String(key)),
			},
			rootField:            "go",
			rootType:             "GoTest",
			sourceModuleName:     "go",
			selectorPath:         itemPath,
			collectionPath:       collectionPath,
			collectionKey:        dagql.String(key),
			collectionKeyType:    keyType,
			collectionDimension:  "go-test",
			collectionType:       "go-tests",
			collectionBatchType:  "GoTests_Batch",
			collectionOccurrence: "go.tests",
		})
	}
	return artifacts
}

func planTestArtifacts(t *testing.T) *Artifacts {
	t.Helper()
	goRoot := artifactTestMainObject(t, "Go")
	sdkReleaseRoot := artifactTestMainObject(t, "SdkRelease")
	typeDefs := artifactTestTypeDefs(t,
		&Function{
			Name:             "go",
			SourceModuleName: "go-toolchain",
			ReturnType:       goRoot,
		},
		&Function{
			Name:             "sdkRelease",
			SourceModuleName: "release-toolchain",
			ReturnType:       sdkReleaseRoot,
		},
	)
	typeDefs = append(
		typeDefs,
		planTestMainObjectWithFields(t, "Go",
			[]*FieldTypeDef{
				{
					Name:    "state",
					TypeDef: artifactTestObject(t, "State"),
				},
			},
			planTestAction(t, "lint", VerbCheck),
			planTestAction(t, "generate", VerbGenerate),
			planTestObjectFunction(t, "tests", "Tests"),
			&Function{
				Name:       "configuredCheck",
				IsCheck:    true,
				ReturnType: artifactTestTypeDef(t, "configured-check-return", &TypeDef{Kind: TypeDefKindVoid}),
				Args: dagql.ObjectResultArray[*FunctionArg]{
					newTypeDefDetachedResult(t, newTypeDefTestDag(t), "configured-check-arg", &FunctionArg{
						Name:    "required",
						TypeDef: artifactTestTypeDef(t, "configured-check-arg-type", &TypeDef{Kind: TypeDefKindString}),
					}),
				},
			},
			&Function{
				Name:       "configured",
				ReturnType: artifactTestObject(t, "Configured"),
				Args: dagql.ObjectResultArray[*FunctionArg]{
					newTypeDefDetachedResult(t, newTypeDefTestDag(t), "configured-arg", &FunctionArg{
						Name:    "required",
						TypeDef: artifactTestTypeDef(t, "configured-arg-type", &TypeDef{Kind: TypeDefKindString}),
					}),
				},
			},
			planTestObjectFunction(t, "sdkRelease", "SdkRelease"),
		),
		planTestObject(t, "State",
			planTestAction(t, "validate", VerbCheck),
			planTestAction(t, "sync", VerbGenerate),
		),
		planTestObject(t, "Tests",
			planTestAction(t, "unit", VerbCheck),
		),
		planTestObject(t, "Configured",
			planTestAction(t, "validate", VerbCheck),
		),
		planTestMainObject(t, "SdkRelease",
			planTestAction(t, "publish", VerbCheck),
		),
	)
	return mustArtifactsFromTypeDefs(t, typeDefs)
}

func planTestMainObjectWithFields(
	t *testing.T,
	name string,
	fields []*FieldTypeDef,
	functions ...*Function,
) dagql.ObjectResult[*TypeDef] {
	t.Helper()
	typeDef := planTestObject(t, name, functions...)
	objectTypeDef(typeDef.Self()).IsMainObject = true
	fieldResults := make(dagql.ObjectResultArray[*FieldTypeDef], len(fields))
	for i, field := range fields {
		fieldResults[i] = newTypeDefDetachedResult(
			t,
			newTypeDefTestDag(t),
			fmt.Sprintf("plan-%s-field-%s", name, field.Name),
			field,
		)
	}
	typeDef.Self().AsObject.Value.Self().Fields = fieldResults
	return typeDef
}

func planTestMainObject(
	t *testing.T,
	name string,
	functions ...*Function,
) dagql.ObjectResult[*TypeDef] {
	t.Helper()
	typeDef := planTestObject(t, name, functions...)
	objectTypeDef(typeDef.Self()).IsMainObject = true
	return typeDef
}

func planTestObject(
	t *testing.T,
	name string,
	functions ...*Function,
) dagql.ObjectResult[*TypeDef] {
	t.Helper()
	dag := newTypeDefTestDag(t)
	functionResults := make(dagql.ObjectResultArray[*Function], len(functions))
	for i, fn := range functions {
		functionResults[i] = newTypeDefDetachedResult(
			t,
			dag,
			fmt.Sprintf("plan-%s-function-%s", name, fn.Name),
			fn,
		)
	}
	sourceModuleName := "go-toolchain"
	if name == "SdkRelease" {
		sourceModuleName = "release-toolchain"
	}
	object := newTypeDefDetachedResult(t, dag, "plan-object-"+name, &ObjectTypeDef{
		Name:             name,
		OriginalName:     name,
		SourceModuleName: sourceModuleName,
		Functions:        functionResults,
	})
	return newTypeDefDetachedResult(t, dag, "plan-type-"+name, &TypeDef{
		Kind:     TypeDefKindObject,
		AsObject: dagql.NonNull(object),
	})
}

func planTestObjectFunction(t *testing.T, name, returnType string) *Function {
	t.Helper()
	return &Function{
		Name:       name,
		ReturnType: artifactTestObject(t, returnType),
	}
}

func planTestAction(t *testing.T, name string, verb Verb) *Function {
	t.Helper()
	fn := &Function{
		Name:       name,
		ReturnType: artifactTestTypeDef(t, "plan-action-"+name+"-return", &TypeDef{Kind: TypeDefKindVoid}),
	}
	switch verb {
	case VerbCheck:
		fn.IsCheck = true
	case VerbGenerate:
		fn.IsGenerator = true
		fn.ReturnType = artifactTestObject(t, "Changeset")
	}
	return fn
}

func actionKeys(actions []*Action) []string {
	keys := make([]string, len(actions))
	for i, action := range actions {
		keys[i] = string(action.Verb()) + ":" + action.displayName()
	}
	return keys
}

func planNodeKeys(t *testing.T, plan *Plan) []string {
	t.Helper()
	nodes := plan.Nodes()
	keys := make([]string, len(nodes))
	for i, node := range nodes {
		target := node.Target().Items()
		require.Len(t, target, 1)
		coordinate := mustArtifactCoordinate(t, target[0], ArtifactTypeDimension)
		keys[i] = coordinate.Value.String() + ":" + string(node.Verb()) + ":" + node.displayName()
	}
	return keys
}
