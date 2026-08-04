package core

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/dagger/dagger/dagql"
	"github.com/dagger/dagger/dagql/call"
	"github.com/dagger/dagger/engine/telemetryattrs"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

func TestArtifactActionTelemetryDescribesBatchScope(t *testing.T) {
	dimensions := []*ArtifactDimension{
		{Name: ArtifactTypeDimension, KeyType: (&TypeDef{}).WithKind(TypeDefKindString)},
		{Name: "go-module", KeyType: (&TypeDef{}).WithKind(TypeDefKindString)},
		{Name: "go-directory", KeyType: (&TypeDef{}).WithKind(TypeDefKindString)},
		{Name: "go-test", KeyType: (&TypeDef{}).WithKind(TypeDefKindString)},
	}
	row := func(testName string) *artifactRow {
		return &artifactRow{
			coordinates: []dagql.Nullable[dagql.String]{
				dagql.NonNull(dagql.String("go-test")),
				dagql.NonNull(dagql.String("api")),
				dagql.NonNull(dagql.String("api/auth")),
				dagql.NonNull(dagql.String(testName)),
			},
			selectorPath: []dagql.Selector{{Field: "go"}, {Field: "tests"}},
			collectionLineage: []artifactCollectionOrigin{{
				occurrence: "go.tests",
			}},
		}
	}
	action := &Action{
		verb:              VerbCheck,
		functionPath:      []string{"test"},
		sourceModuleName:  "go",
		collectionBatched: true,
		target: &Artifacts{
			dimensions: dimensions,
			rows:       []*artifactRow{row("TestAuth"), row("TestJWT")},
		},
	}

	metadata := action.artifactTargetTelemetry()
	require.Equal(t, 2, metadata.count)
	require.Equal(t,
		[]string{"type", "go-module", "go-directory"},
		metadata.commonDimensions,
	)
	require.Equal(t,
		[]string{"go-test", "api", "api/auth"},
		metadata.commonCoordinates,
	)
	require.Equal(t, "go-test", metadata.varyingDimension)
	require.Equal(t, []string{"TestAuth", "TestJWT"}, metadata.varyingCoordinates)
	require.Equal(t, "go-test:api:api/auth:test", action.qualifiedName())

	attrs := attributeValues(action.telemetryAttributes())
	require.NotEmpty(t, attrs[telemetryattrs.ArtifactActionIDAttr])
	require.Equal(t, "CHECK", attrs[telemetryattrs.ArtifactActionVerbAttr])
	require.Equal(t, []string{"test"}, attrs[telemetryattrs.ArtifactActionFunctionPathAttr])
	require.Equal(t, int64(2), attrs[telemetryattrs.ArtifactActionTargetCountAttr])
	require.Equal(t, true, attrs[telemetryattrs.ArtifactActionCollectionBatchedAttr])

	reordered := action.Clone()
	reordered.target.rows[0], reordered.target.rows[1] = reordered.target.rows[1], reordered.target.rows[0]
	reorderedAttrs := attributeValues(reordered.telemetryAttributes())
	require.Equal(
		t,
		attrs[telemetryattrs.ArtifactActionIDAttr],
		reorderedAttrs[telemetryattrs.ArtifactActionIDAttr],
		"action identity must not depend on discovery order",
	)
}

func TestArtifactPlanTelemetrySummarizesGraph(t *testing.T) {
	plan := &Plan{
		verb: VerbCheck,
		nodes: []*Action{
			{target: &Artifacts{rows: []*artifactRow{{}, {}}}, collectionBatched: true},
			{target: &Artifacts{rows: []*artifactRow{{}}}},
		},
	}
	span := &telemetryTestSpan{}
	plan.recordTelemetry(trace.ContextWithSpan(context.Background(), span))
	attrs := attributeValues(span.attrs)
	require.Equal(t, "CHECK", attrs[telemetryattrs.ArtifactPlanVerbAttr])
	require.Equal(t, int64(2), attrs[telemetryattrs.ArtifactPlanActionCountAttr])
	require.Equal(t, int64(3), attrs[telemetryattrs.ArtifactPlanTargetCountAttr])
	require.Equal(t, int64(1), attrs[telemetryattrs.ArtifactPlanBatchCountAttr])
}

func attributeValues(attrs []attribute.KeyValue) map[string]any {
	values := make(map[string]any, len(attrs))
	for _, attr := range attrs {
		values[string(attr.Key)] = attr.Value.AsInterface()
	}
	return values
}

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
		[]TargetPattern{"**:**"},
		[]TargetPattern{"go:tests:*"},
	)
	require.NoError(t, err)
	require.Equal(t, []string{
		"go:CHECK:lint",
		"go-toolchain-state:CHECK:validate",
		"sdk-release:CHECK:publish",
	}, planNodeKeys(t, plan))

	plan, err = artifacts.Plan(
		VerbCheck,
		[]TargetPattern{"go:tests:**"},
		nil,
	)
	require.NoError(t, err)
	require.Equal(t, []string{"go:CHECK:tests:unit"}, planNodeKeys(t, plan))

	plan, err = artifacts.Plan(
		VerbCheck,
		[]TargetPattern{"go-toolchain-tests:unit"},
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
			"go-toolchain": {"go:lint"},
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

	for _, pattern := range []string{"verify", "e2e-test-suite", "e2e:["} {
		_, err = (TargetPattern("")).DecodeInput(pattern)
		require.ErrorContains(t, err, `invalid target pattern "`+pattern+`"`)
	}
	for _, pattern := range []string{"e2e-test-suite:", ":verify", "e2e::verify"} {
		_, err = (TargetPattern("")).DecodeInput(pattern)
		require.ErrorContains(t, err, "type and field segments must not be empty")
	}

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
				targetPatterns: []string{
					"e2e-sdk-dev:go",
					"e2e:sdksarm:go",
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
		[]TargetPattern{"e2e-sdk-dev:go:verify"},
		false,
	))
	require.True(t, matchesTargetPatterns(
		action,
		[]TargetPattern{"e2e:sdksarm:go"},
		false,
	))
	require.False(t, matchesTargetPatterns(
		action,
		[]TargetPattern{"e2e:sdks:go:verify"},
		false,
	))
	require.False(t, matchesTargetPatterns(action, []TargetPattern{"verify"}, false))
}

func TestStaticArtifactGeneratorTarget(t *testing.T) {
	artifacts := planTestArtifacts(t)
	plan, err := artifacts.Plan(
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

	for _, target := range []TargetPattern{
		"go:state:sync",
		"go-toolchain-state:sync",
	} {
		plan, err = artifacts.Plan(VerbGenerate, []TargetPattern{target}, nil)
		require.NoError(t, err)
		require.Equal(
			t,
			[]string{"go-toolchain-state:GENERATE:sync"},
			planNodeKeys(t, plan),
		)
	}
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

	right.functionPath = []string{"lint"}
	right.selectorPath = []dagql.Selector{{Field: "alternate"}}
	require.False(t, actionsEqual(left, right))
}

func TestCollectionPlanBatchesShadowedActions(t *testing.T) {
	artifacts := collectionPlanTestArtifacts(t)

	plan, err := artifacts.Plan(VerbCheck, nil, nil)
	require.NoError(t, err)
	require.Len(t, plan.nodes, 3)

	var batch *Action
	var itemKeys []string
	for _, node := range plan.nodes {
		switch node.displayName() {
		case "run":
			require.True(t, node.CollectionBatched())
			batch = node
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
	require.NotNil(t, batch)
	require.Len(t, batch.target.rows, 2)
	require.Equal(
		t,
		`go.tests.subset(keys: ["integration","unit"]).batch`,
		selectorPathString(batch.selectorPath),
	)
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

func TestCollectionPlanUsesItemActionForSingleton(t *testing.T) {
	artifacts := collectionPlanTestArtifacts(t)
	artifacts, err := artifacts.FilterCoordinates("go-test", []string{"unit"})
	require.NoError(t, err)

	plan, err := artifacts.Plan(
		VerbCheck,
		[]TargetPattern{"go-test:run"},
		nil,
	)
	require.NoError(t, err)
	require.Len(t, plan.nodes, 1)
	require.False(t, plan.nodes[0].CollectionBatched())
	require.Equal(t, "run", plan.nodes[0].displayName())
	require.Len(t, plan.nodes[0].target.rows, 1)
	require.Equal(
		t,
		`go.tests.get(key: "unit")`,
		selectorPathString(plan.nodes[0].selectorPath),
	)
}

func TestCollectionPlanIgnoresBatchOnlyAction(t *testing.T) {
	artifacts := collectionPlanTestArtifacts(t)
	artifacts, err := artifacts.FilterCoordinates("go-test", []string{"unit"})
	require.NoError(t, err)

	plan, err := artifacts.Plan(
		VerbCheck,
		[]TargetPattern{"go-test:audit"},
		nil,
	)
	require.NoError(t, err)
	require.Empty(t, plan.nodes)
}

func TestCollectionPlanRecursivelyBatchesCompleteSubtrees(t *testing.T) {
	artifacts := recursiveCollectionPlanTestArtifacts(t)

	plan, err := artifacts.Plan(
		VerbCheck,
		[]TargetPattern{"go-test:test"},
		nil,
	)
	require.NoError(t, err)
	require.Len(t, plan.nodes, 1)

	batch := plan.nodes[0]
	require.True(t, batch.CollectionBatched())
	require.Len(t, batch.target.rows, 6)
	require.NotNil(t, batch.batchOrigin)
	require.Equal(t, "Directories_Batch", batch.batchOrigin.batchType)
	attrs := attributeValues(batch.telemetryAttributes())
	require.Equal(t, "directories", attrs[telemetryattrs.ArtifactActionBatchTypeAttr])
	require.Equal(t, int64(2), attrs[telemetryattrs.ArtifactActionBatchDepthAttr])
	require.Equal(
		t,
		`go.modules.get(key: "api").testDirectories.subset(keys: ["api/auth","api/db","api/server"]).batch`,
		selectorPathString(batch.selectorPath),
	)
}

func TestCollectionPlanPrefersOuterBatchOnRecursiveTie(t *testing.T) {
	artifacts := recursiveCollectionPlanTestArtifacts(t)
	artifacts, err := artifacts.FilterCoordinates("go-directory", []string{"api/auth"})
	require.NoError(t, err)

	plan, err := artifacts.Plan(
		VerbCheck,
		[]TargetPattern{"go-test:test"},
		nil,
	)
	require.NoError(t, err)
	require.Len(t, plan.nodes, 1)
	require.True(t, plan.nodes[0].CollectionBatched())
	require.Equal(t, "Directories_Batch", plan.nodes[0].batchOrigin.batchType)
	require.Equal(
		t,
		`go.modules.get(key: "api").testDirectories.subset(keys: ["api/auth"]).batch`,
		selectorPathString(plan.nodes[0].selectorPath),
	)
}

func TestCollectionPlanRejectsRecursiveBatchForPartialSubtree(t *testing.T) {
	artifacts := recursiveCollectionPlanTestArtifacts(t)
	artifacts, err := artifacts.FilterCoordinates(
		"go-test",
		[]string{"TestAuth", "TestDatabase"},
	)
	require.NoError(t, err)

	plan, err := artifacts.Plan(
		VerbCheck,
		[]TargetPattern{"go-test:test"},
		nil,
	)
	require.NoError(t, err)
	require.Len(t, plan.nodes, 2)
	for _, node := range plan.nodes {
		require.False(t, node.CollectionBatched())
	}
}

func TestCollectionPlanRecursivelyBatchesCompleteItemSubset(t *testing.T) {
	artifacts := recursiveCollectionPlanTestArtifacts(t)
	artifacts, err := artifacts.FilterCoordinates(
		"go-directory",
		[]string{"api/auth", "api/db"},
	)
	require.NoError(t, err)

	plan, err := artifacts.Plan(
		VerbCheck,
		[]TargetPattern{"go-test:test"},
		nil,
	)
	require.NoError(t, err)
	require.Len(t, plan.nodes, 1)
	require.True(t, plan.nodes[0].CollectionBatched())
	require.Equal(t, "Directories_Batch", plan.nodes[0].batchOrigin.batchType)
	require.Len(t, plan.nodes[0].target.rows, 4)
	require.Equal(
		t,
		`go.modules.get(key: "api").testDirectories.subset(keys: ["api/auth","api/db"]).batch`,
		selectorPathString(plan.nodes[0].selectorPath),
	)
}

func TestCollectionPlanMergesSemanticActionsWithSameOuterBatch(t *testing.T) {
	artifacts := recursiveCollectionPlanTestArtifacts(t)
	directoryType := planTestObject(t, "GoDirectory", planTestAction(t, "test", VerbCheck))
	artifacts.objects["GoDirectory"] = objectTypeDef(directoryType.Self())

	for _, directory := range []string{"api/auth", "api/db", "api/server"} {
		var source *artifactRow
		for _, row := range artifacts.rows {
			if row.coordinates[2].Value.String() == directory {
				source = row
				break
			}
		}
		require.NotNil(t, source)
		directoryOrigin := source.collectionLineage[0].Clone()
		artifacts.rows = append(artifacts.rows, &artifactRow{
			coordinates: []dagql.Nullable[dagql.String]{
				dagql.NonNull(dagql.String("go-directory")),
				dagql.NonNull(dagql.String("api")),
				dagql.NonNull(dagql.String(directory)),
				{},
			},
			coordinateDimensions: []string{
				ArtifactTypeDimension,
				"go-module",
				"go-directory",
				"go-test",
			},
			rootField:        "go",
			rootType:         "GoDirectory",
			sourceModuleName: "go",
			selectorPath: appendSelector(
				directoryOrigin.path,
				dagql.Selector{
					Field: collectionGetFunctionName,
					Args: []dagql.NamedInput{{
						Name:  collectionKeyArgName,
						Value: dagql.String(directory),
					}},
				},
			),
			collectionLineage: []artifactCollectionOrigin{directoryOrigin},
		})
	}
	artifacts.allRows = cloneArtifactRows(artifacts.rows)

	filtered, err := artifacts.FilterCoordinates(
		"go-directory",
		[]string{"api/auth", "api/db"},
	)
	require.NoError(t, err)
	plan, err := filtered.Plan(VerbCheck, nil, nil)
	require.NoError(t, err)
	require.Len(t, plan.nodes, 1)

	batch := plan.nodes[0]
	require.True(t, batch.CollectionBatched())
	require.Equal(t, "Directories_Batch", batch.batchOrigin.batchType)
	require.Len(t, batch.target.rows, 6)
	require.Equal(t, 2, batch.batchDepth())
	require.Equal(
		t,
		`go.modules.get(key: "api").testDirectories.subset(keys: ["api/auth","api/db"]).batch`,
		selectorPathString(batch.selectorPath),
	)

	targetTypes := map[string]int{}
	for _, row := range batch.target.rows {
		targetTypes[row.coordinates[0].Value.String()]++
	}
	require.Equal(t, map[string]int{
		"go-directory": 2,
		"go-test":      4,
	}, targetTypes)
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
		targetPatterns: []string{"go:engine"},
	})

	plan, err := artifacts.Plan(VerbCheck, nil, nil)
	require.NoError(t, err)
	require.Len(t, plan.nodes, 5)

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
	require.Len(t, batches, 1)
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
			rootField:        "go",
			rootType:         "GoTest",
			sourceModuleName: "go",
			selectorPath:     itemPath,
			collectionLineage: []artifactCollectionOrigin{{
				path:       collectionPath,
				key:        dagql.String(key),
				keyType:    keyType,
				dimension:  "go-test",
				typeName:   "go-tests",
				batchType:  "GoTests_Batch",
				occurrence: "go.tests",
			}},
		})
	}
	return artifacts
}

func recursiveCollectionPlanTestArtifacts(t *testing.T) *Artifacts {
	t.Helper()
	keyType := (&TypeDef{}).WithKind(TypeDefKindString)
	itemType := planTestObject(t, "GoTest", planTestAction(t, "test", VerbCheck))
	testsBatchType := planTestObject(t, "Tests_Batch", planTestAction(t, "test", VerbCheck))
	directoriesBatchType := planTestObject(t, "Directories_Batch", planTestAction(t, "test", VerbCheck))
	artifacts := &Artifacts{
		dimensions: []*ArtifactDimension{
			{Name: ArtifactTypeDimension, KeyType: (&TypeDef{}).WithKind(TypeDefKindString)},
			{Name: "go-module", KeyType: keyType},
			{Name: "go-directory", KeyType: keyType},
			{Name: "go-test", KeyType: keyType},
		},
		objects: map[string]*ObjectTypeDef{
			"GoTest":            objectTypeDef(itemType.Self()),
			"Tests_Batch":       objectTypeDef(testsBatchType.Self()),
			"Directories_Batch": objectTypeDef(directoriesBatchType.Self()),
		},
		typeDefs: map[string]*TypeDef{},
	}
	tests := map[string][]string{
		"api/auth":   {"TestAuth", "TestJWT"},
		"api/db":     {"TestDatabase", "TestMigration"},
		"api/server": {"TestGraphQL", "TestServer"},
	}
	for _, directory := range []string{"api/auth", "api/db", "api/server"} {
		directoriesPath := []dagql.Selector{
			{Field: "go"},
			{Field: "modules"},
			{
				Field: collectionGetFunctionName,
				Args: []dagql.NamedInput{{
					Name:  collectionKeyArgName,
					Value: dagql.String("api"),
				}},
			},
			{Field: "testDirectories"},
		}
		directoryOrigin := artifactCollectionOrigin{
			path:       directoriesPath,
			key:        dagql.String(directory),
			keyType:    keyType,
			dimension:  "go-directory",
			typeName:   "directories",
			batchType:  "Directories_Batch",
			occurrence: selectorPathString(directoriesPath),
		}
		testsPath := appendSelector(
			directoriesPath,
			dagql.Selector{
				Field: collectionGetFunctionName,
				Args: []dagql.NamedInput{{
					Name:  collectionKeyArgName,
					Value: dagql.String(directory),
				}},
			},
		)
		testsPath = appendSelector(testsPath, dagql.Selector{Field: "tests"})
		for _, testName := range tests[directory] {
			testOrigin := artifactCollectionOrigin{
				path:       testsPath,
				key:        dagql.String(testName),
				keyType:    keyType,
				dimension:  "go-test",
				typeName:   "tests",
				batchType:  "Tests_Batch",
				occurrence: selectorPathString(testsPath),
			}
			artifacts.rows = append(artifacts.rows, &artifactRow{
				coordinates: []dagql.Nullable[dagql.String]{
					dagql.NonNull(dagql.String("go-test")),
					dagql.NonNull(dagql.String("api")),
					dagql.NonNull(dagql.String(directory)),
					dagql.NonNull(dagql.String(testName)),
				},
				coordinateDimensions: []string{
					ArtifactTypeDimension,
					"go-module",
					"go-directory",
					"go-test",
				},
				rootField:        "go",
				rootType:         "GoTest",
				sourceModuleName: "go",
				selectorPath: appendSelector(
					testsPath,
					dagql.Selector{
						Field: collectionGetFunctionName,
						Args: []dagql.NamedInput{{
							Name:  collectionKeyArgName,
							Value: dagql.String(testName),
						}},
					},
				),
				collectionLineage: []artifactCollectionOrigin{
					directoryOrigin,
					testOrigin,
				},
			})
		}
	}
	artifacts.allRows = cloneArtifactRows(artifacts.rows)
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
