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
	require.Len(t, items, 2)

	goArtifact := items[0]
	require.Equal(t, "go", mustArtifactCoordinate(t, goArtifact, ArtifactTypeDimension).Value.String())

	actions, err := goArtifact.Actions(nil)
	require.NoError(t, err)
	require.Equal(t, []string{
		"CHECK:lint",
		"CHECK:state:validate",
		"CHECK:tests:unit",
		"GENERATE:generate",
	}, actionKeys(actions))

	checks, err := goArtifact.Actions([]Verb{VerbCheck})
	require.NoError(t, err)
	require.Equal(t, []string{"CHECK:lint", "CHECK:state:validate", "CHECK:tests:unit"}, actionKeys(checks))
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
		[]FunctionPattern{"**"},
		[]FunctionPattern{"tests:*"},
	)
	require.NoError(t, err)
	require.Equal(t, []string{
		"go:CHECK:lint",
		"go:CHECK:state:validate",
		"sdk-release:CHECK:publish",
	}, planNodeKeys(t, plan))

	plan, err = artifacts.Plan(
		VerbCheck,
		[]FunctionPattern{"tests:**"},
		nil,
	)
	require.NoError(t, err)
	require.Equal(t, []string{"go:CHECK:tests:unit"}, planNodeKeys(t, plan))

	plan, err = artifacts.Plan(
		VerbCheck,
		[]FunctionPattern{"tests"},
		nil,
	)
	require.NoError(t, err)
	require.Equal(t, []string{"go:CHECK:tests:unit"}, planNodeKeys(t, plan))

	plan, err = artifacts.Plan(
		VerbCheck,
		[]FunctionPattern{"go:lint"},
		nil,
	)
	require.NoError(t, err)
	require.Equal(t, []string{"go:CHECK:lint"}, planNodeKeys(t, plan))

	plan, err = artifacts.Plan(VerbCheck, []FunctionPattern{"go"}, nil)
	require.NoError(t, err)
	require.Equal(t, []string{
		"go:CHECK:lint",
		"go:CHECK:state:validate",
		"go:CHECK:tests:unit",
	}, planNodeKeys(t, plan))
}

func TestArtifactsPlanAppliesSourceSpecificExcludes(t *testing.T) {
	artifacts := planTestArtifacts(t)

	plan, err := artifacts.PlanWithSourceExcludes(
		VerbCheck,
		nil,
		nil,
		map[string][]FunctionPattern{
			"go-toolchain": {"lint"},
		},
	)
	require.NoError(t, err)
	require.Equal(t, []string{
		"go:CHECK:state:validate",
		"go:CHECK:tests:unit",
		"sdk-release:CHECK:publish",
	}, planNodeKeys(t, plan))
}

func TestFunctionPatternValidation(t *testing.T) {
	decoded, err := (FunctionPattern("")).DecodeInput("Tests:**")
	require.NoError(t, err)
	require.Equal(t, FunctionPattern("Tests:**"), decoded)

	_, err = (FunctionPattern("")).DecodeInput("[")
	require.ErrorContains(t, err, `invalid function pattern "["`)
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

func actionNames(actions []*Action) []string {
	names := make([]string, len(actions))
	for i, action := range actions {
		names[i] = action.displayName()
	}
	return names
}

func planTestArtifacts(t *testing.T) *Artifacts {
	t.Helper()
	typeDefs := artifactTestTypeDefs(t,
		&Function{
			Name:             "go",
			SourceModuleName: "go-toolchain",
			ReturnType:       artifactTestObject(t, "Go"),
		},
		&Function{
			Name:             "sdkRelease",
			SourceModuleName: "release-toolchain",
			ReturnType:       artifactTestObject(t, "SdkRelease"),
		},
	)
	typeDefs = append(
		typeDefs,
		planTestObjectWithFields(t, "Go",
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
		),
		planTestObject(t, "Tests",
			planTestAction(t, "unit", VerbCheck),
		),
		planTestObject(t, "Configured",
			planTestAction(t, "validate", VerbCheck),
		),
		planTestObject(t, "SdkRelease",
			planTestAction(t, "publish", VerbCheck),
		),
	)
	return NewArtifactsFromTypeDefs(typeDefs)
}

func planTestObjectWithFields(
	t *testing.T,
	name string,
	fields []*FieldTypeDef,
	functions ...*Function,
) dagql.ObjectResult[*TypeDef] {
	t.Helper()
	typeDef := planTestObject(t, name, functions...)
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
	object := newTypeDefDetachedResult(t, dag, "plan-object-"+name, &ObjectTypeDef{
		Name:      name,
		Functions: functionResults,
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
