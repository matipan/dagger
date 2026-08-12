package core

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"

	doublestar "github.com/bmatcuk/doublestar/v4"
	"github.com/iancoleman/strcase"
	"github.com/vektah/gqlparser/v2/ast"

	"github.com/dagger/dagger/dagql"
	"github.com/dagger/dagger/dagql/call"
	"github.com/dagger/dagger/engine"
	"github.com/dagger/dagger/engine/telemetryattrs"
	"github.com/dagger/dagger/util/parallel"
	telemetry "github.com/dagger/otel-go"
	digest "github.com/opencontainers/go-digest"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// Verb is a standardized artifact lifecycle operation.
type Verb string

var Verbs = dagql.NewEnum[Verb]()

var (
	VerbCheck    = Verbs.Register("CHECK", "Validate an artifact.")
	VerbGenerate = Verbs.Register("GENERATE", "Generate workspace changes for an artifact.")
	VerbShip     = Verbs.Register("SHIP", "Publish an artifact.")
	VerbUp       = Verbs.Register("UP", "Start an artifact as a long-running service.")
)

// GeneratedChecksMode controls how generators participate in a CHECK plan.
type GeneratedChecksMode string

var GeneratedChecksModes = dagql.NewEnum[GeneratedChecksMode]()

var (
	GeneratedChecksAuto = GeneratedChecksModes.Register(
		"AUTO",
		"Honor the workspace check-generated setting, which defaults to enabled.",
	)
	GeneratedChecksInclude = GeneratedChecksModes.Register(
		"INCLUDE",
		"Run checks and fail when generators report pending changes.",
	)
	GeneratedChecksExclude = GeneratedChecksModes.Register(
		"EXCLUDE",
		"Run checks without generators.",
	)
	GeneratedChecksOnly = GeneratedChecksModes.Register(
		"ONLY",
		"Only fail when generators report pending changes.",
	)
)

func (mode GeneratedChecksMode) Type() *ast.Type {
	return &ast.Type{NamedType: "GeneratedChecksMode", NonNull: true}
}

func (mode GeneratedChecksMode) TypeDescription() string {
	return "How generators participate in a CHECK plan."
}

func (mode GeneratedChecksMode) Decoder() dagql.InputDecoder {
	return GeneratedChecksModes
}

func (mode GeneratedChecksMode) ToLiteral() call.Literal {
	return GeneratedChecksModes.Literal(mode)
}

func (verb Verb) Type() *ast.Type {
	return &ast.Type{NamedType: "Verb", NonNull: true}
}

func (verb Verb) TypeDescription() string {
	return "A standardized artifact lifecycle operation."
}

func (verb Verb) Decoder() dagql.InputDecoder {
	return Verbs
}

func (verb Verb) ToLiteral() call.Literal {
	return Verbs.Literal(verb)
}

// TargetPattern is the engine-owned syntax for selecting artifact occurrences
// and lifecycle action paths.
type TargetPattern string

func (pattern TargetPattern) TypeName() string {
	return "TargetPattern"
}

func (pattern TargetPattern) TypeDescription() string {
	return "A type-rooted glob matching artifact fields or lifecycle action paths."
}

func (pattern TargetPattern) Type() *ast.Type {
	return &ast.Type{NamedType: pattern.TypeName(), NonNull: true}
}

func (pattern TargetPattern) Decoder() dagql.InputDecoder {
	return pattern
}

func (pattern TargetPattern) ToLiteral() call.Literal {
	return call.NewLiteralString(string(pattern))
}

func (TargetPattern) DecodeInput(value any) (dagql.Input, error) {
	pattern, ok := value.(string)
	if !ok {
		return nil, fmt.Errorf("cannot convert %T to TargetPattern", value)
	}
	if err := validateTargetPattern(TargetPattern(pattern)); err != nil {
		return nil, err
	}
	return TargetPattern(pattern), nil
}

func (pattern TargetPattern) MarshalJSON() ([]byte, error) {
	return json.Marshal(string(pattern))
}

var _ dagql.ScalarType = TargetPattern("")

// Action is one exact invocation on an artifact target.
type Action struct {
	verb              Verb
	target            *Artifacts
	functionPath      []string
	apiPath           []string
	targetPaths       []string
	selectorPath      []dagql.Selector
	rootField         string
	sourceModuleName  string
	collectionBatched bool
	batchOrigin       *artifactCollectionOrigin
	afterIDs          []dagql.ID[*Action]
	portMappings      []PortForward
	generatedAsCheck  bool
}

func (*Action) Type() *ast.Type {
	return &ast.Type{NamedType: "Action", NonNull: true}
}

func (*Action) TypeDescription() string {
	return "One exact artifact lifecycle invocation."
}

func (action *Action) Clone() *Action {
	cp := *action
	cp.target = action.target.Clone()
	cp.functionPath = slices.Clone(action.functionPath)
	cp.apiPath = slices.Clone(action.apiPath)
	cp.targetPaths = slices.Clone(action.targetPaths)
	cp.selectorPath = cloneSelectors(action.selectorPath)
	if action.batchOrigin != nil {
		origin := action.batchOrigin.Clone()
		cp.batchOrigin = &origin
	}
	cp.afterIDs = slices.Clone(action.afterIDs)
	cp.portMappings = slices.Clone(action.portMappings)
	return &cp
}

func (action *Action) Verb() Verb {
	return action.verb
}

func (action *Action) Target() *Artifacts {
	return action.target
}

func (action *Action) FunctionPath() []string {
	return slices.Clone(action.functionPath)
}

func (action *Action) CollectionBatched() bool {
	return action.collectionBatched
}

func (action *Action) After() []dagql.ID[*Action] {
	return slices.Clone(action.afterIDs)
}

func (action *Action) WithAfter(after []dagql.ID[*Action]) *Action {
	cp := action.Clone()
	for _, candidate := range after {
		if slices.ContainsFunc(cp.afterIDs, func(existing dagql.ID[*Action]) bool {
			existingID, existingErr := existing.ID()
			candidateID, candidateErr := candidate.ID()
			return existingErr == nil &&
				candidateErr == nil &&
				stableIDDigest(existingID) == stableIDDigest(candidateID)
		}) {
			continue
		}
		cp.afterIDs = append(cp.afterIDs, candidate)
	}
	return cp
}

func (action *Action) displayName() string {
	return strings.Join(action.functionPath, ":")
}

func (action *Action) qualifiedName() string {
	target := action.artifactTargetTelemetry()
	return strings.Join(append(target.commonCoordinates, action.displayName()), ":")
}

const maxArtifactTelemetryCoordinates = 128

type artifactTargetTelemetry struct {
	count              int
	digest             string
	commonDimensions   []string
	commonCoordinates  []string
	varyingDimension   string
	varyingCoordinates []string
	truncated          bool
}

func (action *Action) artifactTargetTelemetry() artifactTargetTelemetry {
	target := action.target
	if target == nil {
		return artifactTargetTelemetry{digest: digest.FromString("").String()}
	}

	metadata := artifactTargetTelemetry{count: len(target.rows)}
	canonicalRows := make([]string, 0, len(target.rows))
	for _, row := range target.rows {
		canonicalCoordinates := make([]string, 0, len(row.coordinates))
		for index, coordinate := range row.coordinates {
			if !coordinate.Valid || index >= len(target.dimensions) {
				continue
			}
			canonicalCoordinates = append(
				canonicalCoordinates,
				target.dimensions[index].Name+"="+coordinate.Value.String(),
			)
		}
		canonicalRows = append(canonicalRows, strings.Join([]string{
			strings.Join(canonicalCoordinates, "\x00"),
			selectorPathString(row.selectorPath),
			artifactRowCollectionOccurrence(row),
		}, "\x01"))
	}
	sort.Strings(canonicalRows)
	metadata.digest = digest.FromString(strings.Join(canonicalRows, "\x02")).String()

	for dimensionIndex, dimension := range target.dimensions {
		values := make(map[string]struct{}, len(target.rows))
		allValid := len(target.rows) > 0
		for _, row := range target.rows {
			if dimensionIndex >= len(row.coordinates) || !row.coordinates[dimensionIndex].Valid {
				allValid = false
				continue
			}
			values[row.coordinates[dimensionIndex].Value.String()] = struct{}{}
		}
		if allValid && len(values) == 1 {
			metadata.commonDimensions = append(metadata.commonDimensions, dimension.Name)
			for coordinate := range values {
				metadata.commonCoordinates = append(metadata.commonCoordinates, coordinate)
			}
			continue
		}
		if metadata.varyingDimension == "" && len(values) > 0 {
			metadata.varyingDimension = dimension.Name
			for coordinate := range values {
				metadata.varyingCoordinates = append(metadata.varyingCoordinates, coordinate)
			}
			sort.Strings(metadata.varyingCoordinates)
		}
	}

	if len(metadata.varyingCoordinates) > maxArtifactTelemetryCoordinates {
		metadata.varyingCoordinates = metadata.varyingCoordinates[:maxArtifactTelemetryCoordinates]
		metadata.truncated = true
	}
	return metadata
}

func (action *Action) telemetryAttributes() []attribute.KeyValue {
	target := action.artifactTargetTelemetry()
	actionIdentity, _ := json.Marshal(struct {
		Verb              Verb
		FunctionPath      []string
		Implementation    string
		SourceModule      string
		CollectionBatched bool
		GeneratedAsCheck  bool
		TargetDigest      string
	}{
		Verb:              action.verb,
		FunctionPath:      action.functionPath,
		Implementation:    action.implementationIdentity(),
		SourceModule:      action.sourceModuleName,
		CollectionBatched: action.collectionBatched,
		GeneratedAsCheck:  action.generatedAsCheck,
		TargetDigest:      target.digest,
	})

	attrs := []attribute.KeyValue{
		attribute.String(telemetryattrs.ArtifactActionIDAttr, digest.FromBytes(actionIdentity).String()),
		attribute.String(telemetryattrs.ArtifactActionVerbAttr, string(action.verb)),
		attribute.StringSlice(telemetryattrs.ArtifactActionFunctionPathAttr, action.functionPath),
		attribute.Bool(telemetryattrs.ArtifactActionCollectionBatchedAttr, action.collectionBatched),
		attribute.Int(telemetryattrs.ArtifactActionTargetCountAttr, target.count),
		attribute.String(telemetryattrs.ArtifactActionTargetDigestAttr, target.digest),
	}
	if action.sourceModuleName != "" {
		attrs = append(attrs, attribute.String(
			telemetryattrs.ArtifactActionSourceModuleAttr,
			action.sourceModuleName,
		))
	}
	if action.batchOrigin != nil {
		attrs = append(attrs,
			attribute.String(
				telemetryattrs.ArtifactActionBatchTypeAttr,
				action.batchOrigin.typeName,
			),
			attribute.Int(
				telemetryattrs.ArtifactActionBatchDepthAttr,
				action.batchDepth(),
			),
		)
	}
	if len(target.commonDimensions) > 0 {
		attrs = append(attrs,
			attribute.StringSlice(
				telemetryattrs.ArtifactActionCommonDimensionsAttr,
				target.commonDimensions,
			),
			attribute.StringSlice(
				telemetryattrs.ArtifactActionCommonCoordinatesAttr,
				target.commonCoordinates,
			),
		)
	}
	if target.varyingDimension != "" {
		attrs = append(attrs,
			attribute.String(
				telemetryattrs.ArtifactActionVaryingDimensionAttr,
				target.varyingDimension,
			),
			attribute.StringSlice(
				telemetryattrs.ArtifactActionVaryingCoordinatesAttr,
				target.varyingCoordinates,
			),
		)
	}
	if target.truncated {
		attrs = append(attrs, attribute.Bool(
			telemetryattrs.ArtifactActionTargetsTruncatedAttr,
			true,
		))
	}
	return attrs
}

func (action *Action) batchDepth() int {
	if action == nil || action.batchOrigin == nil || action.target == nil || len(action.target.rows) == 0 {
		return 0
	}
	depth := 0
	for _, row := range action.target.rows {
		for i := range row.collectionLineage {
			if row.collectionLineage[i].occurrence == action.batchOrigin.occurrence {
				depth = max(depth, len(row.collectionLineage)-i)
				break
			}
		}
	}
	return depth
}

func (plan *Plan) recordTelemetry(ctx context.Context) {
	targetCount := 0
	batchCount := 0
	for _, node := range plan.nodes {
		if node.target != nil {
			targetCount += len(node.target.rows)
		}
		if node.collectionBatched {
			batchCount++
		}
	}
	trace.SpanFromContext(ctx).SetAttributes(
		attribute.String(telemetryattrs.ArtifactPlanVerbAttr, string(plan.verb)),
		attribute.Int(telemetryattrs.ArtifactPlanActionCountAttr, len(plan.nodes)),
		attribute.Int(telemetryattrs.ArtifactPlanTargetCountAttr, targetCount),
		attribute.Int(telemetryattrs.ArtifactPlanBatchCountAttr, batchCount),
	)
}

// Plan is a finite DAG of artifact actions.
type Plan struct {
	verb         Verb
	nodes        []*Action
	loadFailures []string
}

func (*Plan) Type() *ast.Type {
	return &ast.Type{NamedType: "Plan", NonNull: true}
}

func (*Plan) TypeDescription() string {
	return "A compiled finite DAG of artifact actions."
}

func (plan *Plan) Clone() *Plan {
	cp := &Plan{
		verb:         plan.verb,
		nodes:        make([]*Action, len(plan.nodes)),
		loadFailures: slices.Clone(plan.loadFailures),
	}
	for i, node := range plan.nodes {
		cp.nodes[i] = node.Clone()
	}
	return cp
}

func (plan *Plan) Verb() Verb {
	return plan.verb
}

func (plan *Plan) Nodes() []*Action {
	nodes := make([]*Action, len(plan.nodes))
	for i, node := range plan.nodes {
		nodes[i] = node.Clone()
	}
	return nodes
}

func (plan *Plan) LoadFailures() []string {
	return slices.Clone(plan.loadFailures)
}

func (plan *Plan) WithLoadFailures(loadFailures []string) *Plan {
	cp := plan.Clone()
	cp.loadFailures = slices.Clone(loadFailures)
	return cp
}

// WithPortMappings applies explicit host port mappings by qualified action name.
func (plan *Plan) WithPortMappings(mappings map[string][]PortForward) *Plan {
	cp := plan.Clone()
	for _, node := range cp.nodes {
		node.portMappings = slices.Clone(mappings[node.qualifiedName()])
	}
	return cp
}

func (artifact *Artifact) Actions(verbs []Verb) ([]*Action, error) {
	selected := map[Verb]struct{}{}
	for _, verb := range verbs {
		selected[verb] = struct{}{}
	}
	allVerbs := len(selected) == 0

	root, ok := artifact.scope.objects[artifact.rootType]
	if !ok {
		return nil, fmt.Errorf("artifact root type %q not found", artifact.rootType)
	}

	actions, err := artifact.discoverActions(root, selected, allVerbs, nil)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(actions, func(i, j int) bool {
		if actions[i].verb != actions[j].verb {
			return actions[i].verb < actions[j].verb
		}
		return actions[i].displayName() < actions[j].displayName()
	})
	return actions, nil
}

func (artifact *Artifact) discoverActions(
	root *ObjectTypeDef,
	selected map[Verb]struct{},
	allVerbs bool,
	batchOrigin *artifactCollectionOrigin,
) ([]*Action, error) {
	var actions []*Action
	var walk func(*ObjectTypeDef, []string, []string, []string, map[string]struct{}) error
	walk = func(
		object *ObjectTypeDef,
		functionPath []string,
		apiPath []string,
		targetPaths []string,
		stack map[string]struct{},
	) error {
		if _, found := stack[object.Name]; found {
			return nil
		}
		stack = cloneStringSet(stack)
		stack[object.Name] = struct{}{}

		for _, fieldResult := range object.Fields {
			field := fieldResult.Self()
			if field == nil || field.TypeDef.Self() == nil {
				continue
			}
			fieldType := artifact.scope.fullObjectTypeDef(field.TypeDef.Self())
			if !fieldType.AsObject.Valid || fieldType.AsObject.Value.Self() == nil {
				continue
			}
			if fieldType.AsCollection.Valid {
				continue
			}
			if isStaticArtifactField(fieldType) {
				continue
			}
			childType := fieldType.AsObject.Value.Self().Name
			if _, boundary := artifact.scope.topLevelTypes[childType]; boundary {
				continue
			}
			child, found := artifact.scope.objects[childType]
			if !found {
				continue
			}
			nextTargetPaths := artifactFieldTargetPatterns(
				targetPaths,
				object,
				field.Name,
			)
			if err := walk(
				child,
				appendPath(functionPath, strcase.ToKebab(field.Name)),
				appendPath(apiPath, field.Name),
				nextTargetPaths,
				stack,
			); err != nil {
				return err
			}
		}

		for _, fnResult := range object.Functions {
			fn := fnResult.Self()
			if fn == nil {
				continue
			}
			if functionRequiresArgs(fn) {
				continue
			}
			cliSegment := strcase.ToKebab(fn.Name)
			nextFunctionPath := appendPath(functionPath, cliSegment)
			nextAPIPath := appendPath(apiPath, fn.Name)
			nextTargetPaths := artifactFieldTargetPatterns(
				targetPaths,
				object,
				fn.Name,
			)

			for _, verb := range functionVerbs(fn) {
				var action *Action
				if batchOrigin != nil {
					action = artifact.newBatchAction(*batchOrigin, verb, nextFunctionPath, nextAPIPath)
				} else {
					action = artifact.newAction(verb, nextFunctionPath, nextAPIPath)
					action.targetPaths = slices.Clone(nextTargetPaths)
				}
				if allVerbs {
					actions = append(actions, action)
					continue
				}
				if _, wanted := selected[verb]; wanted {
					actions = append(actions, action)
				}
			}

			if fn.IsCheck || fn.IsGenerator || fn.IsUp || fn.ReturnType.Self() == nil {
				continue
			}
			returnType := artifact.scope.fullObjectTypeDef(fn.ReturnType.Self())
			if !returnType.AsObject.Valid || returnType.AsObject.Value.Self() == nil {
				continue
			}
			if returnType.AsCollection.Valid {
				continue
			}
			childType := returnType.AsObject.Value.Self().Name
			if _, boundary := artifact.scope.topLevelTypes[childType]; boundary {
				continue
			}
			child, found := artifact.scope.objects[childType]
			if !found {
				continue
			}
			if err := walk(
				child,
				nextFunctionPath,
				nextAPIPath,
				nextTargetPaths,
				stack,
			); err != nil {
				return err
			}
		}
		return nil
	}

	if err := walk(root, nil, nil, nil, nil); err != nil {
		return nil, err
	}
	return actions, nil
}

func (artifact *Artifact) Action(verb Verb, functionPath []string) (*Action, error) {
	normalized := normalizeFunctionPath(functionPath)
	actions, err := artifact.Actions([]Verb{verb})
	if err != nil {
		return nil, err
	}
	for _, action := range actions {
		if slices.Equal(action.functionPath, normalized) {
			return action, nil
		}
	}
	return nil, fmt.Errorf(
		"%s action %q not found on artifact %q",
		verb,
		strings.Join(normalized, ":"),
		artifact.rootType,
	)
}

func (artifact *Artifact) newAction(verb Verb, functionPath, apiPath []string) *Action {
	return &Action{
		verb:             verb,
		target:           artifact.targetScope(),
		functionPath:     slices.Clone(functionPath),
		apiPath:          slices.Clone(apiPath),
		selectorPath:     cloneSelectors(artifact.selectorPath),
		rootField:        artifact.rootField,
		sourceModuleName: artifact.sourceModuleName,
	}
}

func (artifact *Artifact) newBatchAction(
	origin artifactCollectionOrigin,
	verb Verb,
	functionPath, apiPath []string,
) *Action {
	action := artifact.newAction(verb, functionPath, apiPath)
	action.collectionBatched = true
	clonedOrigin := origin.Clone()
	action.batchOrigin = &clonedOrigin
	action.selectorPath = collectionBatchSelectorPath(
		origin.path,
		origin.keyType,
		[]dagql.Input{origin.key},
	)
	return action
}

func (artifact *Artifact) planActions(verb Verb) ([]*Action, error) {
	itemActions, err := artifact.Actions([]Verb{verb})
	if err != nil {
		return nil, err
	}
	origin := artifact.immediateCollectionOrigin()
	if origin == nil || origin.batchType == "" {
		return itemActions, nil
	}
	batch, found := artifact.scope.objects[origin.batchType]
	if !found {
		return itemActions, nil
	}
	batchActions, err := artifact.discoverActions(
		batch,
		map[Verb]struct{}{verb: {}},
		false,
		origin,
	)
	if err != nil {
		return nil, err
	}
	for index, itemAction := range itemActions {
		batchIndex := slices.IndexFunc(batchActions, func(batchAction *Action) bool {
			return slices.Equal(batchAction.functionPath, itemAction.functionPath)
		})
		if batchIndex >= 0 {
			itemActions[index] = batchActions[batchIndex]
		}
	}
	return itemActions, nil
}

func (artifact *Artifact) batchAction(
	origin artifactCollectionOrigin,
	verb Verb,
	functionPath []string,
) (*Action, error) {
	if origin.batchType == "" {
		return nil, nil
	}
	batch, found := artifact.scope.objects[origin.batchType]
	if !found {
		return nil, nil
	}
	actions, err := artifact.discoverActions(
		batch,
		map[Verb]struct{}{verb: {}},
		false,
		&origin,
	)
	if err != nil {
		return nil, err
	}
	for _, action := range actions {
		if slices.Equal(action.functionPath, functionPath) {
			return action, nil
		}
	}
	return nil, nil
}

func collectionBatchSelectorPath(
	collectionPath []dagql.Selector,
	keyType *TypeDef,
	keys []dagql.Input,
) []dagql.Selector {
	path := appendSelector(collectionPath, dagql.Selector{
		Field: collectionSubsetName,
		Args: []dagql.NamedInput{{
			Name: collectionKeysFieldName,
			Value: dagql.DynamicArrayInput{
				Elem:   keyType.ToInput(),
				Values: slices.Clone(keys),
			},
		}},
	})
	return appendSelector(path, dagql.Selector{Field: collectionBatchFieldName})
}

func (artifacts *Artifacts) Plan(
	verb Verb,
	include []TargetPattern,
	exclude []TargetPattern,
) (*Plan, error) {
	return artifacts.PlanWithSourceExcludes(verb, include, exclude, nil)
}

// PlanWithSourceExcludes compiles a plan while applying target patterns to
// artifacts owned by a particular source module.
func (artifacts *Artifacts) PlanWithSourceExcludes(
	verb Verb,
	include []TargetPattern,
	exclude []TargetPattern,
	sourceExcludes map[string][]TargetPattern,
) (*Plan, error) {
	if err := validateTargetPatterns(include); err != nil {
		return nil, err
	}
	if err := validateTargetPatterns(exclude); err != nil {
		return nil, err
	}
	for _, patterns := range sourceExcludes {
		if err := validateTargetPatterns(patterns); err != nil {
			return nil, err
		}
	}

	switch verb {
	case VerbCheck, VerbGenerate, VerbUp:
	default:
		return nil, fmt.Errorf("%s plan construction is not implemented", verb)
	}

	var nodes []*Action
	for _, artifact := range artifacts.Items() {
		actions, err := artifact.planActions(verb)
		if err != nil {
			return nil, err
		}
		for _, action := range actions {
			if !matchesTargetPatterns(action, include, len(include) == 0) {
				continue
			}
			if matchesTargetPatterns(action, exclude, false) {
				continue
			}
			if matchesTargetPatterns(
				action,
				sourceExcludes[action.sourceModuleName],
				false,
			) {
				continue
			}
			if action.collectionBatched {
				existingIndex := slices.IndexFunc(nodes, func(existing *Action) bool {
					return existing.collectionBatched &&
						existing.batchOrigin != nil &&
						action.batchOrigin != nil &&
						existing.verb == action.verb &&
						existing.sourceModuleName == action.sourceModuleName &&
						existing.batchOrigin.occurrence == action.batchOrigin.occurrence &&
						slices.Equal(existing.functionPath, action.functionPath)
				})
				if existingIndex >= 0 {
					mergeCollectionBatchAction(nodes[existingIndex], action)
					continue
				}
			}
			if slices.ContainsFunc(nodes, func(existing *Action) bool {
				return actionsEqual(existing, action)
			}) {
				continue
			}
			nodes = append(nodes, action)
		}
	}
	if err := resolveSingletonCollectionBatches(nodes); err != nil {
		return nil, err
	}
	nodes, err := resolveRecursiveCollectionBatches(artifacts, nodes)
	if err != nil {
		return nil, err
	}
	nodes = mergeEquivalentBatchImplementations(nodes)

	sort.SliceStable(nodes, func(i, j int) bool {
		left := nodes[i].target.rows[0]
		right := nodes[j].target.rows[0]
		if cmp := compareCoordinateRows(left.coordinates, right.coordinates); cmp != 0 {
			return cmp < 0
		}
		return nodes[i].displayName() < nodes[j].displayName()
	})
	return &Plan{verb: verb, nodes: nodes}, nil
}

// CheckPlan compiles checks and, when requested, generators that validate the
// workspace by failing if they produce a non-empty changeset.
func (artifacts *Artifacts) CheckPlan(
	include []TargetPattern,
	exclude []TargetPattern,
	checkSourceExcludes map[string][]TargetPattern,
	generateSourceExcludes map[string][]TargetPattern,
	includeChecks bool,
	includeGenerators bool,
) (*Plan, error) {
	plan := &Plan{verb: VerbCheck}
	if includeChecks {
		checks, err := artifacts.PlanWithSourceExcludes(
			VerbCheck,
			include,
			exclude,
			checkSourceExcludes,
		)
		if err != nil {
			return nil, err
		}
		plan.nodes = append(plan.nodes, checks.nodes...)
	}
	if includeGenerators {
		generators, err := artifacts.PlanWithSourceExcludes(
			VerbGenerate,
			include,
			exclude,
			generateSourceExcludes,
		)
		if err != nil {
			return nil, err
		}
		for _, generator := range generators.nodes {
			generator = generator.Clone()
			generator.verb = VerbCheck
			generator.generatedAsCheck = true
			if slices.ContainsFunc(plan.nodes, func(existing *Action) bool {
				return actionsEqual(existing, generator)
			}) {
				continue
			}
			plan.nodes = append(plan.nodes, generator)
		}
	}
	sort.SliceStable(plan.nodes, func(i, j int) bool {
		left := plan.nodes[i].target.rows[0]
		right := plan.nodes[j].target.rows[0]
		if cmp := compareCoordinateRows(left.coordinates, right.coordinates); cmp != 0 {
			return cmp < 0
		}
		return plan.nodes[i].displayName() < plan.nodes[j].displayName()
	})
	return plan, nil
}

// Batch candidates have already been merged by collection occurrence. Resolve
// singleton candidates back to their semantic item implementation.
func resolveSingletonCollectionBatches(nodes []*Action) error {
	for index, batchAction := range nodes {
		if !batchAction.collectionBatched || len(batchAction.target.rows) != 1 {
			continue
		}
		items := batchAction.target.Items()
		if len(items) != 1 {
			continue
		}
		itemActions, err := items[0].Actions([]Verb{batchAction.verb})
		if err != nil {
			return err
		}
		for _, itemAction := range itemActions {
			if slices.Equal(itemAction.functionPath, batchAction.functionPath) {
				nodes[index] = itemAction
				break
			}
		}
	}
	return nil
}

func mergeCollectionBatchAction(target, additional *Action) {
	for _, additionalRow := range additional.target.rows {
		if slices.ContainsFunc(target.target.rows, func(existingRow *artifactRow) bool {
			return artifactRowsEqual(existingRow, additionalRow)
		}) {
			continue
		}
		target.target.rows = append(target.target.rows, additionalRow.Clone())
	}

	if target.batchOrigin == nil {
		return
	}
	keys := make([]dagql.Input, 0, len(target.target.rows))
	for _, row := range target.target.rows {
		origin := artifactRowCollectionOrigin(row, target.batchOrigin.occurrence)
		if origin != nil {
			keys = append(keys, origin.key)
		}
	}
	target.selectorPath = collectionBatchSelectorPath(
		target.batchOrigin.path,
		target.batchOrigin.keyType,
		keys,
	)
}

type recursiveBatchGroupKey struct {
	verb         Verb
	sourceModule string
	rootType     string
	functionPath string
	occurrence   string
	batchType    string
}

type recursiveBatchItem struct {
	origin       artifactCollectionOrigin
	nodeIndexes  map[int]struct{}
	selectedRows map[string]struct{}
}

type recursiveBatchGroup struct {
	key          recursiveBatchGroupKey
	functionPath []string
	items        map[string]*recursiveBatchItem
}

// Immediate collection batching has already run. Walk the remaining collection
// lineage from the inside out and replace complete item subtrees with the
// outermost matching handler.
func resolveRecursiveCollectionBatches(
	artifacts *Artifacts,
	nodes []*Action,
) ([]*Action, error) {
	maxLineage := 0
	for _, node := range nodes {
		for _, row := range node.target.rows {
			maxLineage = max(maxLineage, len(row.collectionLineage))
		}
	}
	for depth := maxLineage - 2; depth >= 0; depth-- {
		var err error
		nodes, err = resolveRecursiveCollectionBatchDepth(artifacts, nodes, depth)
		if err != nil {
			return nil, err
		}
	}
	return nodes, nil
}

func resolveRecursiveCollectionBatchDepth(
	artifacts *Artifacts,
	nodes []*Action,
	depth int,
) ([]*Action, error) {
	groups := map[recursiveBatchGroupKey]*recursiveBatchGroup{}
	for nodeIndex, node := range nodes {
		origin, rootType, keyID, ok, err := recursiveBatchNodeOrigin(node, depth)
		if err != nil {
			return nil, err
		}
		if !ok || origin.batchType == "" {
			continue
		}
		groupKey := recursiveBatchGroupKey{
			verb:         node.verb,
			sourceModule: node.sourceModuleName,
			rootType:     rootType,
			functionPath: strings.Join(node.functionPath, "\x00"),
			occurrence:   origin.occurrence,
			batchType:    origin.batchType,
		}
		group := groups[groupKey]
		if group == nil {
			group = &recursiveBatchGroup{
				key:          groupKey,
				functionPath: slices.Clone(node.functionPath),
				items:        map[string]*recursiveBatchItem{},
			}
			groups[groupKey] = group
		}
		item := group.items[keyID]
		if item == nil {
			item = &recursiveBatchItem{
				origin:       origin.Clone(),
				nodeIndexes:  map[int]struct{}{},
				selectedRows: map[string]struct{}{},
			}
			group.items[keyID] = item
		}
		item.nodeIndexes[nodeIndex] = struct{}{}
		for _, row := range node.target.rows {
			item.selectedRows[artifactRowPlanIdentity(row)] = struct{}{}
		}
	}

	groupKeys := make([]recursiveBatchGroupKey, 0, len(groups))
	for key := range groups {
		groupKeys = append(groupKeys, key)
	}
	sort.Slice(groupKeys, func(i, j int) bool {
		left, right := groupKeys[i], groupKeys[j]
		return strings.Join([]string{
			string(left.verb), left.sourceModule, left.rootType,
			left.functionPath, left.occurrence, left.batchType,
		}, "\x00") < strings.Join([]string{
			string(right.verb), right.sourceModule, right.rootType,
			right.functionPath, right.occurrence, right.batchType,
		}, "\x00")
	})

	removed := make(map[int]struct{})
	var replacements []*Action
	for _, groupKey := range groupKeys {
		group := groups[groupKey]
		itemKeys := make([]string, 0, len(group.items))
		for key := range group.items {
			itemKeys = append(itemKeys, key)
		}
		sort.Strings(itemKeys)

		var eligible []*recursiveBatchItem
		covered := map[int]struct{}{}
		for _, key := range itemKeys {
			item := group.items[key]
			if !recursiveBatchItemComplete(artifacts, group.key, item, depth) {
				continue
			}
			eligible = append(eligible, item)
			for nodeIndex := range item.nodeIndexes {
				covered[nodeIndex] = struct{}{}
			}
		}
		if len(covered) == 0 {
			continue
		}

		coveredIndexes := make([]int, 0, len(covered))
		for nodeIndex := range covered {
			if _, alreadyRemoved := removed[nodeIndex]; alreadyRemoved {
				continue
			}
			coveredIndexes = append(coveredIndexes, nodeIndex)
		}
		if len(coveredIndexes) == 0 {
			continue
		}
		sort.Ints(coveredIndexes)

		firstNode := nodes[coveredIndexes[0]]
		firstRow := firstNode.target.rows[0]
		origin := eligible[0].origin
		artifact := artifacts.artifactForRow(firstRow)
		batchAction, err := artifact.batchAction(
			origin,
			group.key.verb,
			group.functionPath,
		)
		if err != nil {
			return nil, err
		}
		if batchAction == nil {
			continue
		}

		batchAction.target = firstNode.target.Clone()
		batchAction.target.rows = nil
		var afterIDs []dagql.ID[*Action]
		for _, nodeIndex := range coveredIndexes {
			for _, row := range nodes[nodeIndex].target.rows {
				if slices.ContainsFunc(batchAction.target.rows, func(existing *artifactRow) bool {
					return artifactRowsEqual(existing, row)
				}) {
					continue
				}
				batchAction.target.rows = append(batchAction.target.rows, row.Clone())
			}
			afterIDs = append(afterIDs, nodes[nodeIndex].afterIDs...)
			removed[nodeIndex] = struct{}{}
		}
		sort.SliceStable(batchAction.target.rows, func(i, j int) bool {
			return compareCoordinateRows(
				batchAction.target.rows[i].coordinates,
				batchAction.target.rows[j].coordinates,
			) < 0
		})
		keys := make([]dagql.Input, len(eligible))
		for i, item := range eligible {
			keys[i] = item.origin.key
		}
		batchAction.selectorPath = collectionBatchSelectorPath(
			origin.path,
			origin.keyType,
			keys,
		)
		batchAction = batchAction.WithAfter(afterIDs)
		replacements = append(replacements, batchAction)
	}

	if len(replacements) == 0 {
		return nodes, nil
	}
	resolved := make([]*Action, 0, len(nodes)-len(removed)+len(replacements))
	for nodeIndex, node := range nodes {
		if _, found := removed[nodeIndex]; !found {
			resolved = append(resolved, node)
		}
	}
	return append(resolved, replacements...), nil
}

// Different semantic artifact actions can resolve to the exact same outer
// batch invocation. Execute that implementation once while retaining every
// semantic target and dependency represented by it.
func mergeEquivalentBatchImplementations(nodes []*Action) []*Action {
	merged := make([]*Action, 0, len(nodes))
	for _, node := range nodes {
		index := slices.IndexFunc(merged, func(existing *Action) bool {
			return batchImplementationsEqual(existing, node)
		})
		if index < 0 {
			merged = append(merged, node)
			continue
		}

		existing := merged[index]
		for _, row := range node.target.rows {
			if slices.ContainsFunc(existing.target.rows, func(candidate *artifactRow) bool {
				return artifactRowsEqual(candidate, row)
			}) {
				continue
			}
			existing.target.rows = append(existing.target.rows, row.Clone())
		}
		sort.SliceStable(existing.target.rows, func(i, j int) bool {
			return compareCoordinateRows(
				existing.target.rows[i].coordinates,
				existing.target.rows[j].coordinates,
			) < 0
		})
		existing.afterIDs = existing.WithAfter(node.afterIDs).afterIDs
	}
	return merged
}

func batchImplementationsEqual(left, right *Action) bool {
	return left != nil &&
		right != nil &&
		left.collectionBatched &&
		right.collectionBatched &&
		left.batchOrigin != nil &&
		right.batchOrigin != nil &&
		left.verb == right.verb &&
		left.sourceModuleName == right.sourceModuleName &&
		left.rootField == right.rootField &&
		left.batchOrigin.occurrence == right.batchOrigin.occurrence &&
		left.batchOrigin.batchType == right.batchOrigin.batchType &&
		slices.Equal(left.functionPath, right.functionPath) &&
		left.implementationIdentity() == right.implementationIdentity()
}

func recursiveBatchNodeOrigin(
	node *Action,
	depth int,
) (artifactCollectionOrigin, string, string, bool, error) {
	if node == nil || node.target == nil || len(node.target.rows) == 0 {
		return artifactCollectionOrigin{}, "", "", false, nil
	}
	first := node.target.rows[0]
	if len(first.collectionLineage) <= depth+1 {
		return artifactCollectionOrigin{}, "", "", false, nil
	}
	origin := first.collectionLineage[depth]
	keyID, err := collectionKeyID(origin.key)
	if err != nil {
		return artifactCollectionOrigin{}, "", "", false, err
	}
	for _, row := range node.target.rows[1:] {
		if row.rootType != first.rootType || len(row.collectionLineage) <= depth+1 {
			return artifactCollectionOrigin{}, "", "", false, nil
		}
		candidate := row.collectionLineage[depth]
		candidateKeyID, err := collectionKeyID(candidate.key)
		if err != nil {
			return artifactCollectionOrigin{}, "", "", false, err
		}
		if candidate.occurrence != origin.occurrence || candidateKeyID != keyID {
			return artifactCollectionOrigin{}, "", "", false, nil
		}
	}
	return origin.Clone(), first.rootType, keyID, true, nil
}

func recursiveBatchItemComplete(
	artifacts *Artifacts,
	group recursiveBatchGroupKey,
	item *recursiveBatchItem,
	depth int,
) bool {
	rows := artifacts.allRows
	if len(rows) == 0 {
		rows = artifacts.rows
	}
	expected := map[string]struct{}{}
	wantedKey, err := collectionKeyID(item.origin.key)
	if err != nil {
		return false
	}
	for _, row := range rows {
		if row.rootType != group.rootType || len(row.collectionLineage) <= depth+1 {
			continue
		}
		origin := row.collectionLineage[depth]
		if origin.occurrence != group.occurrence {
			continue
		}
		keyID, err := collectionKeyID(origin.key)
		if err != nil || keyID != wantedKey {
			continue
		}
		expected[artifactRowPlanIdentity(row)] = struct{}{}
	}
	if len(expected) == 0 || len(expected) != len(item.selectedRows) {
		return false
	}
	for rowID := range expected {
		if _, found := item.selectedRows[rowID]; !found {
			return false
		}
	}
	return true
}

func artifactRowPlanIdentity(row *artifactRow) string {
	if row == nil {
		return ""
	}
	coordinates := make([]string, len(row.coordinates))
	for i, coordinate := range row.coordinates {
		if coordinate.Valid {
			coordinates[i] = "1" + coordinate.Value.String()
		}
	}
	return strings.Join(coordinates, "\x00") + "\x01" + selectorPathString(row.selectorPath)
}

func (action *Action) Run(ctx context.Context) error {
	plan, dependencies, err := action.executionGraph(ctx)
	if err != nil {
		return err
	}
	switch action.verb {
	case VerbCheck:
		return plan.runGraph(ctx, dependencies, false, func(ctx context.Context, _ int, node *Action) error {
			return node.runOwn(ctx)
		})
	case VerbGenerate:
		changes, err := plan.changesWithDependencies(
			ctx,
			dependencies,
			FailEarlyOnConflicts,
		)
		if err != nil {
			return err
		}
		return changes.Export(ctx, ".")
	case VerbUp:
		return plan.runUpWithDependencies(ctx, dependencies)
	default:
		return fmt.Errorf("cannot run %s action", action.verb)
	}
}

func (action *Action) runOwn(ctx context.Context) error {
	switch action.verb {
	case VerbCheck:
		if action.generatedAsCheck {
			changes, err := action.generateChanges(ctx)
			if err != nil {
				return err
			}
			empty, err := changes.IsEmpty(ctx)
			if err != nil {
				return err
			}
			if !empty {
				return fmt.Errorf("generated files are out of date for %q", action.displayName())
			}
			return nil
		}
		return action.runCheck(ctx)
	case VerbGenerate:
		changes, err := action.generateChanges(ctx)
		if err != nil {
			return err
		}
		return changes.Export(ctx, ".")
	case VerbUp:
		return (&Plan{verb: VerbUp, nodes: []*Action{action}}).runUp(ctx)
	default:
		return fmt.Errorf("cannot run %s action", action.verb)
	}
}

func (action *Action) executionGraph(ctx context.Context) (*Plan, [][]int, error) {
	if len(action.afterIDs) == 0 {
		return &Plan{verb: action.verb, nodes: []*Action{action}}, [][]int{nil}, nil
	}

	srv, err := CurrentDagqlServer(ctx)
	if err != nil {
		return nil, nil, err
	}
	loaded := map[string]*Action{}
	return collectActionGraph(
		ctx,
		action,
		func(ctx context.Context, current *Action) ([]*Action, error) {
			dependencies := make([]*Action, 0, len(current.afterIDs))
			for _, id := range current.afterIDs {
				callID, err := id.ID()
				if err != nil {
					return nil, fmt.Errorf(
						"resolve dependency of %s action %q: %w",
						current.verb,
						current.displayName(),
						err,
					)
				}
				key := stableIDDigest(callID).String()
				dependency, found := loaded[key]
				if !found {
					result, err := id.Load(ctx, srv)
					if err != nil {
						return nil, fmt.Errorf(
							"resolve dependency of %s action %q: %w",
							current.verb,
							current.displayName(),
							err,
						)
					}
					dependency = result.Self()
					loaded[key] = dependency
				}
				dependencies = append(dependencies, dependency)
			}
			return dependencies, nil
		},
	)
}

func collectActionGraph(
	ctx context.Context,
	root *Action,
	loadDependencies func(context.Context, *Action) ([]*Action, error),
) (*Plan, [][]int, error) {
	plan := &Plan{verb: root.verb}
	var dependencies [][]int
	expanded := map[*Action]struct{}{}

	var add func(*Action) (int, error)
	add = func(action *Action) (int, error) {
		if action.verb != plan.verb {
			return -1, fmt.Errorf(
				"%s action %q cannot depend on %s action %q",
				plan.verb,
				root.displayName(),
				action.verb,
				action.displayName(),
			)
		}

		nodeIndex := slices.IndexFunc(plan.nodes, func(candidate *Action) bool {
			return actionsEqual(candidate, action)
		})
		if nodeIndex < 0 {
			nodeIndex = len(plan.nodes)
			plan.nodes = append(plan.nodes, action)
			dependencies = append(dependencies, nil)
		}
		if _, found := expanded[action]; found {
			return nodeIndex, nil
		}
		expanded[action] = struct{}{}

		after, err := loadDependencies(ctx, action)
		if err != nil {
			return -1, err
		}
		for _, dependency := range after {
			dependencyIndex, err := add(dependency)
			if err != nil {
				return -1, err
			}
			if !slices.Contains(dependencies[nodeIndex], dependencyIndex) {
				dependencies[nodeIndex] = append(dependencies[nodeIndex], dependencyIndex)
			}
		}
		return nodeIndex, nil
	}
	if _, err := add(root); err != nil {
		return nil, nil, err
	}
	if err := validatePlanDependencies(dependencies); err != nil {
		return nil, nil, fmt.Errorf("%s action %q: %w", root.verb, root.displayName(), err)
	}
	return plan, dependencies, nil
}

func (plan *Plan) Run(ctx context.Context, failFast bool) error {
	switch plan.verb {
	case VerbCheck:
		return plan.runActions(ctx, failFast)
	case VerbGenerate:
		changes, err := plan.Changes(ctx, FailEarlyOnConflicts)
		if err != nil {
			return err
		}
		return changes.Export(ctx, ".")
	case VerbUp:
		return plan.runUp(ctx)
	default:
		return fmt.Errorf("cannot run %s plan", plan.verb)
	}
}

// Services evaluates an UP plan and returns one service per action in node order.
func (plan *Plan) Services(ctx context.Context) ([]dagql.ObjectResult[*Service], error) {
	if plan.verb != VerbUp {
		return nil, fmt.Errorf("services are only available for UP plans")
	}
	dependencies, err := plan.resolveDependencies(ctx)
	if err != nil {
		return nil, err
	}
	return plan.servicesWithDependencies(ctx, dependencies)
}

// Service evaluates an UP plan that structurally resolves to exactly one service.
func (plan *Plan) Service(ctx context.Context) (dagql.ObjectResult[*Service], error) {
	if plan.verb != VerbUp {
		return dagql.ObjectResult[*Service]{}, fmt.Errorf("service is only available for UP plans")
	}
	if len(plan.nodes) != 1 {
		return dagql.ObjectResult[*Service]{}, fmt.Errorf(
			"UP plan must contain exactly one service action, found %d",
			len(plan.nodes),
		)
	}
	services, err := plan.Services(ctx)
	if err != nil {
		return dagql.ObjectResult[*Service]{}, err
	}
	return services[0], nil
}

func (plan *Plan) runActions(ctx context.Context, failFast bool) error {
	dependencies, err := plan.resolveDependencies(ctx)
	if err != nil {
		return err
	}
	return plan.runGraph(ctx, dependencies, failFast, func(ctx context.Context, _ int, node *Action) error {
		return node.runOwn(ctx)
	})
}

func (plan *Plan) Changes(
	ctx context.Context,
	conflictStrategy WithChangesetsMergeConflict,
) (*Changeset, error) {
	if plan.verb != VerbGenerate {
		return nil, fmt.Errorf("changes are only available for GENERATE plans")
	}

	dependencies, err := plan.resolveDependencies(ctx)
	if err != nil {
		return nil, err
	}
	return plan.changesWithDependencies(ctx, dependencies, conflictStrategy)
}

func (plan *Plan) changesWithDependencies(
	ctx context.Context,
	dependencies [][]int,
	conflictStrategy WithChangesetsMergeConflict,
) (*Changeset, error) {
	changes := make([]*Changeset, len(plan.nodes))
	if err := plan.runGraph(ctx, dependencies, false, func(ctx context.Context, index int, node *Action) error {
		var nodeErr error
		changes[index], nodeErr = node.generateChanges(ctx)
		return nodeErr
	}); err != nil {
		return nil, err
	}

	merged, err := NewEmptyChangeset(ctx)
	if err != nil {
		return nil, err
	}
	return merged.WithChangesets(ctx, changes, conflictStrategy)
}

func (plan *Plan) resolveDependencies(ctx context.Context) ([][]int, error) {
	dependencies := make([][]int, len(plan.nodes))
	hasDependencies := false
	for _, node := range plan.nodes {
		if len(node.afterIDs) > 0 {
			hasDependencies = true
			break
		}
	}
	if !hasDependencies {
		return dependencies, nil
	}

	srv, err := CurrentDagqlServer(ctx)
	if err != nil {
		return nil, err
	}
	for nodeIndex, node := range plan.nodes {
		for _, id := range node.afterIDs {
			result, err := id.Load(ctx, srv)
			if err != nil {
				return nil, fmt.Errorf(
					"resolve dependency of %s action %q: %w",
					node.verb,
					node.displayName(),
					err,
				)
			}
			dependency := result.Self()
			if dependency.verb != plan.verb {
				return nil, fmt.Errorf(
					"%s plan action %q cannot depend on %s action %q",
					plan.verb,
					node.displayName(),
					dependency.verb,
					dependency.displayName(),
				)
			}
			dependencyIndex := slices.IndexFunc(plan.nodes, func(candidate *Action) bool {
				return actionsEqual(candidate, dependency)
			})
			if dependencyIndex < 0 {
				return nil, fmt.Errorf(
					"%s action %q depends on action %q, which is not present in the plan",
					plan.verb,
					node.displayName(),
					dependency.displayName(),
				)
			}
			if !slices.Contains(dependencies[nodeIndex], dependencyIndex) {
				dependencies[nodeIndex] = append(dependencies[nodeIndex], dependencyIndex)
			}
		}
	}
	if err := validatePlanDependencies(dependencies); err != nil {
		return nil, fmt.Errorf("%s plan: %w", plan.verb, err)
	}
	return dependencies, nil
}

func (plan *Plan) runGraph(
	ctx context.Context,
	dependencies [][]int,
	failFast bool,
	run func(context.Context, int, *Action) error,
) error {
	plan.recordTelemetry(ctx)

	done := make([]chan struct{}, len(plan.nodes))
	nodeErrors := make([]error, len(plan.nodes))
	for i := range done {
		done[i] = make(chan struct{})
	}

	jobs := parallel.New().WithContextualTracer(true).WithFailFast(failFast)
	for nodeIndex, node := range plan.nodes {
		nodeIndex, node := nodeIndex, node
		jobs = jobs.WithJob(node.displayName(), func(ctx context.Context) error {
			defer close(done[nodeIndex])
			for _, dependencyIndex := range dependencies[nodeIndex] {
				select {
				case <-done[dependencyIndex]:
					if nodeErrors[dependencyIndex] != nil {
						nodeErrors[nodeIndex] = fmt.Errorf(
							"dependency %q failed: %w",
							plan.nodes[dependencyIndex].displayName(),
							nodeErrors[dependencyIndex],
						)
						return nodeErrors[nodeIndex]
					}
				case <-ctx.Done():
					nodeErrors[nodeIndex] = context.Cause(ctx)
					return nodeErrors[nodeIndex]
				}
			}
			nodeErrors[nodeIndex] = run(ctx, nodeIndex, node)
			return nodeErrors[nodeIndex]
		})
	}
	return jobs.Run(ctx)
}

func validatePlanDependencies(dependencies [][]int) error {
	const (
		unvisited = iota
		visiting
		visited
	)
	states := make([]int, len(dependencies))
	var visit func(int) error
	visit = func(node int) error {
		switch states[node] {
		case visiting:
			return fmt.Errorf("dependency cycle detected at node %d", node)
		case visited:
			return nil
		}
		states[node] = visiting
		for _, dependency := range dependencies[node] {
			if dependency < 0 || dependency >= len(dependencies) {
				return fmt.Errorf("node %d has invalid dependency %d", node, dependency)
			}
			if err := visit(dependency); err != nil {
				return err
			}
		}
		states[node] = visited
		return nil
	}
	for node := range dependencies {
		if err := visit(node); err != nil {
			return err
		}
	}
	return nil
}

func actionsEqual(left, right *Action) bool {
	return left != nil &&
		right != nil &&
		left.verb == right.verb &&
		slices.Equal(left.functionPath, right.functionPath) &&
		left.implementationIdentity() == right.implementationIdentity() &&
		artifactScopesEqual(left.target, right.target)
}

func (action *Action) implementationIdentity() string {
	if action == nil {
		return ""
	}
	return selectorPathString(action.selectorPath) + "\x00" + strings.Join(action.apiPath, "\x00")
}

func artifactScopesEqual(left, right *Artifacts) bool {
	if left == nil || right == nil {
		return left == right
	}
	if len(left.dimensions) != len(right.dimensions) || len(left.rows) != len(right.rows) {
		return false
	}
	for i := range left.dimensions {
		if left.dimensions[i].Name != right.dimensions[i].Name {
			return false
		}
	}

	matched := make([]bool, len(right.rows))
	for _, leftRow := range left.rows {
		match := -1
		for rightIndex, rightRow := range right.rows {
			if !matched[rightIndex] && artifactRowsEqual(leftRow, rightRow) {
				match = rightIndex
				break
			}
		}
		if match < 0 {
			return false
		}
		matched[match] = true
	}
	return true
}

func artifactRowsEqual(left, right *artifactRow) bool {
	return left != nil &&
		right != nil &&
		compareCoordinateRows(left.coordinates, right.coordinates) == 0 &&
		selectorPathString(left.selectorPath) == selectorPathString(right.selectorPath)
}

func (action *Action) runCheck(ctx context.Context) (rerr error) {
	name := action.qualifiedName()
	attrs := append(action.telemetryAttributes(),
		attribute.Bool(telemetry.UIRollUpLogsAttr, true),
		attribute.Bool(telemetry.UIRollUpSpansAttr, true),
		attribute.String(telemetry.CheckNameAttr, name),
	)
	ctx, span := Tracer(ctx).Start(
		ctx,
		name,
		telemetry.Reveal(),
		trace.WithAttributes(attrs...),
	)
	finishCacheTracking := func() bool { return false }
	defer func() {
		if finishCacheTracking() && rerr == nil {
			span.SetAttributes(attribute.Bool(telemetry.CachedAttr, true))
		}
		span.SetAttributes(attribute.Bool(telemetry.CheckPassedAttr, rerr == nil))
		telemetry.EndWithCause(span, &rerr)
	}()

	srv, parent, leaf, err := action.selectionParent(ctx)
	if err != nil {
		return err
	}
	ctx, finishCacheTracking = trackArtifactActionCache(ctx)
	var result dagql.AnyResult
	if err := srv.Select(
		dagql.WithNonInternalTelemetry(ctx),
		parent,
		&result,
		dagql.Selector{Field: leaf},
	); err != nil {
		return err
	}
	if object, ok := dagql.UnwrapAs[dagql.AnyObjectResult](result); ok {
		if syncField, found := object.ObjectType().FieldSpec("sync", srv.View); found &&
			!syncField.Args.HasRequired(srv.View) {
			if err := srv.Select(
				dagql.WithNonInternalTelemetry(ctx),
				object,
				&result,
				dagql.Selector{Field: "sync"},
			); err != nil {
				return err
			}
		}
	}
	return nil
}

func (action *Action) generateChanges(ctx context.Context) (_ *Changeset, rerr error) {
	name := action.qualifiedName()
	attrs := append(action.telemetryAttributes(),
		attribute.Bool(telemetry.UIRollUpLogsAttr, true),
		attribute.Bool(telemetry.UIRollUpSpansAttr, true),
		attribute.String(telemetry.GeneratorNameAttr, name),
	)
	ctx, span := Tracer(ctx).Start(
		ctx,
		name,
		telemetry.Reveal(),
		trace.WithAttributes(attrs...),
	)
	finishCacheTracking := func() bool { return false }
	defer func() {
		if finishCacheTracking() && rerr == nil {
			span.SetAttributes(attribute.Bool(telemetry.CachedAttr, true))
		}
		telemetry.EndWithCause(span, &rerr)
	}()

	srv, parent, leaf, err := action.selectionParent(ctx)
	if err != nil {
		return nil, err
	}
	ctx, finishCacheTracking = trackArtifactActionCache(ctx)
	var changes dagql.ObjectResult[*Changeset]
	if err := srv.Select(
		dagql.WithNonInternalTelemetry(ctx),
		parent,
		&changes,
		dagql.Selector{Field: leaf},
	); err != nil {
		return nil, err
	}
	return changes.Self(), nil
}

func (plan *Plan) servicesWithDependencies(
	ctx context.Context,
	dependencies [][]int,
) ([]dagql.ObjectResult[*Service], error) {
	services := make([]dagql.ObjectResult[*Service], len(plan.nodes))
	if err := plan.runGraph(ctx, dependencies, false, func(ctx context.Context, index int, node *Action) error {
		var err error
		services[index], err = node.evaluateService(ctx)
		return err
	}); err != nil {
		return nil, err
	}
	return services, nil
}

func (plan *Plan) runUp(ctx context.Context) error {
	dependencies, err := plan.resolveDependencies(ctx)
	if err != nil {
		return err
	}
	return plan.runUpWithDependencies(ctx, dependencies)
}

func (plan *Plan) runUpWithDependencies(ctx context.Context, dependencies [][]int) error {
	services, err := plan.servicesWithDependencies(ctx, dependencies)
	if err != nil {
		return err
	}
	if err := plan.checkServicePortCollisions(services); err != nil {
		return err
	}

	var (
		mu      sync.Mutex
		results []*runUpStartResult
	)
	jobs := parallel.New().WithContextualTracer(true)
	for i, node := range plan.nodes {
		i, node := i, node
		jobs = jobs.WithJob(node.displayName(), func(ctx context.Context) error {
			result, err := node.startService(ctx, services[i])
			if err != nil {
				return err
			}
			mu.Lock()
			results = append(results, result)
			mu.Unlock()
			return nil
		})
	}
	if err := jobs.Run(ctx); err != nil {
		for _, result := range results {
			result.readySpan.End()
		}
		return err
	}

	<-ctx.Done()
	for _, result := range results {
		result.readySpan.End()
	}
	return nil
}

func (plan *Plan) checkServicePortCollisions(
	services []dagql.ObjectResult[*Service],
) error {
	type portKey struct {
		port     int
		protocol NetworkProtocol
	}
	type servicePort struct {
		name string
		port portKey
	}

	var allPorts []servicePort
	for i, node := range plan.nodes {
		if len(node.portMappings) > 0 {
			for _, mapping := range node.portMappings {
				hostPort := mapping.Backend
				if mapping.Frontend != nil {
					hostPort = *mapping.Frontend
				}
				allPorts = append(allPorts, servicePort{
					name: node.qualifiedName(),
					port: portKey{port: hostPort, protocol: mapping.Protocol},
				})
			}
			continue
		}

		service := services[i].Self()
		if service == nil || service.Container.Self() == nil {
			continue
		}
		for _, port := range service.Container.Self().Ports {
			allPorts = append(allPorts, servicePort{
				name: node.qualifiedName(),
				port: portKey{port: port.Port, protocol: port.Protocol},
			})
		}
	}

	seen := make(map[portKey]string)
	var conflicts []string
	for _, servicePort := range allPorts {
		if first, found := seen[servicePort.port]; found {
			conflicts = append(conflicts, fmt.Sprintf(
				"port %d/%s is exposed by both %q and %q",
				servicePort.port.port,
				strings.ToLower(string(servicePort.port.protocol)),
				first,
				servicePort.name,
			))
		} else {
			seen[servicePort.port] = servicePort.name
		}
	}
	if len(conflicts) > 0 {
		return fmt.Errorf("port collision detected:\n  %s", strings.Join(conflicts, "\n  "))
	}
	return nil
}

func (action *Action) evaluateService(
	ctx context.Context,
) (dagql.ObjectResult[*Service], error) {
	var service dagql.ObjectResult[*Service]
	srv, parent, leaf, err := action.selectionParent(ctx)
	if err != nil {
		return service, err
	}
	if err := srv.Select(
		dagql.WithNonInternalTelemetry(ctx),
		parent,
		&service,
		dagql.Selector{Field: leaf},
	); err != nil {
		return service, err
	}
	return service, nil
}

type runUpStartResult struct {
	readySpan trace.Span
}

const serviceNameAttr = "dagger.io/service.name"

func (action *Action) startService(
	ctx context.Context,
	service dagql.ObjectResult[*Service],
) (_ *runUpStartResult, rerr error) {
	name := action.qualifiedName()
	attrs := append(action.telemetryAttributes(),
		attribute.Bool(telemetry.UIRollUpLogsAttr, true),
		attribute.String(serviceNameAttr, name),
	)
	ctx, span := Tracer(ctx).Start(
		ctx,
		name,
		telemetry.Reveal(),
		trace.WithAttributes(attrs...),
	)
	defer func() {
		telemetry.EndWithCause(span, &rerr)
	}()

	var portDescriptions []string
	if len(action.portMappings) > 0 {
		portDescriptions = make([]string, 0, len(action.portMappings))
		for _, mapping := range action.portMappings {
			portDescriptions = append(
				portDescriptions,
				fmt.Sprintf(":%d->%d", mapping.FrontendOrBackendPort(), mapping.Backend),
			)
		}
	} else if service.Self() != nil && service.Self().Container.Self() != nil {
		portDescriptions = make([]string, 0, len(service.Self().Container.Self().Ports))
		for _, port := range service.Self().Container.Self().Ports {
			portDescriptions = append(portDescriptions, fmt.Sprintf(":%d", port.Port))
		}
	}
	if len(portDescriptions) > 0 {
		span.SetName(fmt.Sprintf("%s %s", name, strings.Join(portDescriptions, ", ")))
	}

	srv, err := CurrentDagqlServer(ctx)
	if err != nil {
		return nil, err
	}
	serviceID, err := service.ID()
	if err != nil {
		return nil, fmt.Errorf("get service ID: %w", err)
	}
	tunnelArgs := []dagql.NamedInput{
		{Name: "service", Value: dagql.NewID[*Service](serviceID)},
	}
	if len(action.portMappings) > 0 {
		portInputs := make([]dagql.InputObject[PortForward], len(action.portMappings))
		for i, mapping := range action.portMappings {
			inputMap := map[string]any{
				"backend": mapping.Backend,
			}
			if mapping.Frontend != nil {
				inputMap["frontend"] = *mapping.Frontend
			}
			if mapping.Protocol != "" {
				inputMap["protocol"] = string(mapping.Protocol)
			}
			decoded, err := (dagql.InputObject[PortForward]{}).Decoder().DecodeInput(inputMap)
			if err != nil {
				return nil, fmt.Errorf("decode host tunnel port forward input: %w", err)
			}
			portInput, ok := decoded.(dagql.InputObject[PortForward])
			if !ok {
				return nil, fmt.Errorf(
					"decode host tunnel port forward input: unexpected input %T",
					decoded,
				)
			}
			portInputs[i] = portInput
		}
		tunnelArgs = append(tunnelArgs, dagql.NamedInput{
			Name:  "ports",
			Value: dagql.ArrayInput[dagql.InputObject[PortForward]](portInputs),
		})
	} else {
		tunnelArgs = append(
			tunnelArgs,
			dagql.NamedInput{Name: "native", Value: dagql.Boolean(true)},
		)
	}

	var hostService dagql.Result[*Service]
	if err := srv.Select(
		ctx,
		srv.Root(),
		&hostService,
		dagql.Selector{Field: "host"},
		dagql.Selector{Field: "tunnel", Args: tunnelArgs},
	); err != nil {
		return nil, fmt.Errorf("create host tunnel: %w", err)
	}

	query, err := CurrentQuery(ctx)
	if err != nil {
		return nil, err
	}
	serviceManager, err := query.Services(ctx)
	if err != nil {
		return nil, fmt.Errorf("get services: %w", err)
	}
	hostServiceDigest, err := hostService.ContentPreferredDigest(ctx)
	if err != nil {
		return nil, fmt.Errorf("get host service digest: %w", err)
	}
	runningService, err := serviceManager.Start(
		ctx,
		hostServiceDigest,
		hostService.Self(),
		true,
	)
	if err != nil {
		return nil, fmt.Errorf("start service: %w", err)
	}

	var urls []string
	for _, port := range runningService.Ports {
		scheme := "http"
		if port.Port == 443 {
			scheme = "https"
		}
		urls = append(urls, fmt.Sprintf("%s://localhost:%d", scheme, port.Port))
	}
	readyName := "ready"
	if len(urls) > 0 {
		readyName += " " + strings.Join(urls, " ")
	}
	_, readySpan := Tracer(ctx).Start(
		ctx,
		readyName,
		telemetry.Reveal(),
		trace.WithAttributes(attribute.StringSlice("service.urls", urls)),
	)
	return &runUpStartResult{readySpan: readySpan}, nil
}

func (action *Action) selectionParent(
	ctx context.Context,
) (*dagql.Server, dagql.AnyObjectResult, string, error) {
	if len(action.apiPath) == 0 {
		return nil, nil, "", fmt.Errorf("action has an empty function path")
	}
	query, err := CurrentQuery(ctx)
	if err != nil {
		return nil, nil, "", err
	}
	if workspace := action.target.Workspace(); workspace != nil {
		clientMetadata, err := query.SpecificClientMetadata(ctx, workspace.ClientID)
		if err != nil {
			return nil, nil, "", fmt.Errorf("get workspace client metadata: %w", err)
		}
		ctx = engine.ContextWithClientMetadata(ctx, clientMetadata)
	}
	served, err := query.Server.CurrentServedDeps(ctx)
	if err != nil {
		return nil, nil, "", fmt.Errorf("get current served modules: %w", err)
	}
	srv, err := served.Schema(ctx)
	if err != nil {
		return nil, nil, "", fmt.Errorf("get current served schema: %w", err)
	}
	// Entrypoint schemas flatten a module's methods onto Query and omit its
	// constructor. Actions retain canonical API paths, so execute them against
	// the underlying schema rather than the entrypoint proxy.
	srv = srv.Canonical()
	if workspaceID := action.target.WorkspaceID(); workspaceID != nil {
		workspace, err := dagql.NewID[*Workspace](workspaceID).Load(ctx, srv)
		if err != nil {
			return nil, nil, "", fmt.Errorf("load action workspace: %w", err)
		}
		ctx = ContextWithWorkspaceOverride(ctx, workspace)
	}

	var parent dagql.AnyObjectResult = srv.Root()
	selectors := cloneSelectors(action.selectorPath)
	if len(selectors) == 0 {
		selectors = []dagql.Selector{{Field: action.rootField}}
	}
	for _, field := range action.apiPath[:len(action.apiPath)-1] {
		selectors = append(selectors, dagql.Selector{Field: field})
	}
	for _, selector := range selectors {
		var next dagql.AnyObjectResult
		if err := srv.Select(
			dagql.WithNonInternalTelemetry(ctx),
			parent,
			&next,
			selector,
		); err != nil {
			return nil, nil, "", err
		}
		parent = next
	}
	return srv, parent, action.apiPath[len(action.apiPath)-1], nil
}

func functionVerbs(fn *Function) []Verb {
	var verbs []Verb
	if fn.IsCheck {
		verbs = append(verbs, VerbCheck)
	}
	if fn.IsGenerator {
		verbs = append(verbs, VerbGenerate)
	}
	if fn.IsUp {
		verbs = append(verbs, VerbUp)
	}
	return verbs
}

func normalizeFunctionPath(path []string) []string {
	normalized := make([]string, len(path))
	for i, segment := range path {
		normalized[i] = strcase.ToKebab(segment)
	}
	return normalized
}

func normalizeTargetPattern(pattern string) string {
	segments := strings.Split(pattern, ":")
	for i, segment := range segments {
		segments[i] = artifactCLIName(segment)
	}
	return strings.Join(segments, "/")
}

func validateTargetPatterns(patterns []TargetPattern) error {
	for _, pattern := range patterns {
		if err := validateTargetPattern(pattern); err != nil {
			return err
		}
	}
	return nil
}

func validateTargetPattern(pattern TargetPattern) error {
	segments := strings.Split(string(pattern), ":")
	if len(segments) < 2 {
		return fmt.Errorf(
			"invalid target pattern %q: expected <type>:<field>[:<field>...]",
			pattern,
		)
	}
	for _, segment := range segments {
		if artifactCLIName(segment) == "" {
			return fmt.Errorf(
				"invalid target pattern %q: type and field segments must not be empty",
				pattern,
			)
		}
	}
	normalized := normalizeTargetPattern(string(pattern))
	if _, err := doublestar.PathMatch(normalized, "validate"); err != nil {
		return fmt.Errorf("invalid target pattern %q: %w", pattern, err)
	}
	return nil
}

func matchesTargetPatterns(
	action *Action,
	patterns []TargetPattern,
	emptyMatches bool,
) bool {
	if len(patterns) == 0 {
		return emptyMatches
	}
	actionPath := strings.Join(action.functionPath, "/")
	var candidates []string
	if action.target != nil && len(action.target.rows) == 1 {
		row := action.target.rows[0]
		if index, err := action.target.dimensionIndex(ArtifactTypeDimension); err == nil {
			coordinate := row.coordinates[index]
			if coordinate.Valid {
				actionType := coordinate.Value.String()
				candidates = append(
					candidates,
					actionType+"/"+actionPath,
				)
			}
		}
		for _, target := range row.targetPatterns {
			normalized := normalizeTargetPattern(target)
			candidates = append(
				candidates,
				normalized,
				normalized+"/"+actionPath,
			)
		}
	}
	for _, target := range action.targetPaths {
		candidates = append(candidates, normalizeTargetPattern(target))
	}
	for _, pattern := range patterns {
		normalized := normalizeTargetPattern(string(pattern))
		for _, candidate := range candidates {
			matched, err := doublestar.PathMatch(normalized, candidate)
			if err == nil && matched {
				return true
			}
			// A literal object or artifact target selects every lifecycle
			// entrypoint beneath it.
			if !strings.ContainsAny(normalized, `*?[\`) &&
				strings.HasPrefix(candidate, normalized+"/") {
				return true
			}
		}
	}
	return false
}

func appendPath(path []string, segment string) []string {
	next := make([]string, len(path)+1)
	copy(next, path)
	next[len(path)] = segment
	return next
}

func cloneStringSet(source map[string]struct{}) map[string]struct{} {
	cloned := make(map[string]struct{}, len(source)+1)
	for key := range source {
		cloned[key] = struct{}{}
	}
	return cloned
}

func compareCoordinateRows(
	left []dagql.Nullable[dagql.String],
	right []dagql.Nullable[dagql.String],
) int {
	for i := 0; i < min(len(left), len(right)); i++ {
		switch {
		case !left[i].Valid && !right[i].Valid:
			continue
		case !left[i].Valid:
			return -1
		case !right[i].Valid:
			return 1
		case left[i].Value.String() < right[i].Value.String():
			return -1
		case left[i].Value.String() > right[i].Value.String():
			return 1
		}
	}
	return len(left) - len(right)
}
