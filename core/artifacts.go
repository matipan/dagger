package core

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"unicode"

	"github.com/dagger/dagger/dagql"
	"github.com/dagger/dagger/dagql/call"
	"github.com/iancoleman/strcase"
	"github.com/vektah/gqlparser/v2/ast"
	"golang.org/x/sync/errgroup"
)

const ArtifactTypeDimension = "type"

// Artifacts is an immutable, filterable view over workspace artifacts.
type Artifacts struct {
	dimensions    []*ArtifactDimension
	rows          []*artifactRow
	objects       map[string]*ObjectTypeDef
	typeDefs      map[string]*TypeDef
	topLevelTypes map[string]struct{}
	workspace     *Workspace
	workspaceID   *call.ID
	filters       []artifactFilter
	materialized  bool
	loadFailures  []string
	bestEffort    bool
}

type artifactFilter struct {
	dimension string
	values    []string
	nonNull   bool
}

type artifactRow struct {
	coordinates          []dagql.Nullable[dagql.String]
	coordinateDimensions []string
	rootField            string
	rootType             string
	sourceModuleName     string
	selectorPath         []dagql.Selector
	targetPatterns       []string
	collectionPath       []dagql.Selector
	collectionKey        dagql.Input
	collectionKeyType    *TypeDef
	collectionDimension  string
	collectionType       string
	collectionBatchType  string
	collectionOccurrence string
}

// ArtifactDimension describes one coordinate axis in an Artifacts scope.
type ArtifactDimension struct {
	Name            string
	KeyType         *TypeDef
	collectionTypes []string
}

// Artifact is one coordinate row in an Artifacts scope.
type Artifact struct {
	coordinates          []dagql.Nullable[dagql.String]
	coordinateDimensions []string
	scope                *Artifacts
	rootField            string
	rootType             string
	sourceModuleName     string
	selectorPath         []dagql.Selector
	targetPatterns       []string
	collectionPath       []dagql.Selector
	collectionKey        dagql.Input
	collectionKeyType    *TypeDef
	collectionDimension  string
	collectionType       string
	collectionBatchType  string
	collectionOccurrence string
}

func (*Artifacts) Type() *ast.Type {
	return &ast.Type{
		NamedType: "Artifacts",
		NonNull:   true,
	}
}

func (*Artifacts) TypeDescription() string {
	return "A scoped, filterable view over workspace artifacts."
}

func (*ArtifactDimension) Type() *ast.Type {
	return &ast.Type{
		NamedType: "ArtifactDimension",
		NonNull:   true,
	}
}

func (*ArtifactDimension) TypeDescription() string {
	return "A filterable axis of the artifact graph."
}

func (*Artifact) Type() *ast.Type {
	return &ast.Type{
		NamedType: "Artifact",
		NonNull:   true,
	}
}

func (*Artifact) TypeDescription() string {
	return "One artifact in a workspace."
}

// NewWorkspaceArtifacts returns a lazy artifact scope. Workspace modules are
// loaded only when the scope is enumerated or compiled into a plan, after its
// filters are known.
func NewWorkspaceArtifacts(workspace *Workspace, workspaceID *call.ID) *Artifacts {
	return &Artifacts{
		dimensions: []*ArtifactDimension{
			{
				Name: ArtifactTypeDimension,
				KeyType: &TypeDef{
					Kind: TypeDefKindString,
				},
			},
		},
		objects:       map[string]*ObjectTypeDef{},
		typeDefs:      map[string]*TypeDef{},
		topLevelTypes: map[string]struct{}{},
		workspace:     workspace,
		workspaceID:   workspaceID,
	}
}

// NewArtifactsFromTypeDefs discovers the schema-stable artifact shape without
// evaluating dynamic collection keys.
func NewArtifactsFromTypeDefs(typeDefs dagql.ObjectResultArray[*TypeDef]) (*Artifacts, error) {
	objects := map[string]*ObjectTypeDef{}
	objectTypeDefs := map[string]*TypeDef{}
	for _, typeDefResult := range typeDefs {
		typeDef := typeDefResult.Self()
		if typeDef == nil || !typeDef.AsObject.Valid || typeDef.AsObject.Value.Self() == nil {
			continue
		}
		objectTypeDef := typeDef.AsObject.Value.Self()
		objects[objectTypeDef.Name] = objectTypeDef.Clone()
		objectTypeDefs[objectTypeDef.Name] = typeDef.Clone()
	}

	var roots []*artifactRow
	for _, typeDefResult := range typeDefs {
		typeDef := typeDefResult.Self()
		if typeDef == nil || !typeDef.AsObject.Valid || typeDef.AsObject.Value.Self() == nil {
			continue
		}
		queryObject := typeDef.AsObject.Value.Self()
		if queryObject.Name != "Query" {
			continue
		}
		for _, fnResult := range queryObject.Functions {
			fn := fnResult.Self()
			if fn == nil || fn.SourceModuleName == "" || fn.ReturnType.Self() == nil {
				continue
			}
			returnType := fn.ReturnType.Self()
			if !returnType.AsObject.Valid || returnType.AsObject.Value.Self() == nil {
				continue
			}
			returnObject := returnType.AsObject.Value.Self()
			typeName := returnObject.Name
			if canonicalType, found := objectTypeDefs[typeName]; found {
				if canonicalObject := objectTypeDef(canonicalType); canonicalObject != nil {
					returnObject = canonicalObject
				}
			}
			roots = append(roots, &artifactRow{
				coordinateDimensions: []string{ArtifactTypeDimension},
				rootField:            fn.Name,
				rootType:             typeName,
				sourceModuleName:     fn.SourceModuleName,
				selectorPath:         []dagql.Selector{{Field: fn.Name}},
				targetPatterns:       []string{artifactTypeCLIName(returnObject)},
			})
		}
	}

	sort.SliceStable(roots, func(i, j int) bool {
		if roots[i].rootType != roots[j].rootType {
			return roots[i].rootType < roots[j].rootType
		}
		if roots[i].sourceModuleName != roots[j].sourceModuleName {
			return roots[i].sourceModuleName < roots[j].sourceModuleName
		}
		return roots[i].rootField < roots[j].rootField
	})

	artifacts := &Artifacts{
		dimensions: []*ArtifactDimension{
			{
				Name: ArtifactTypeDimension,
				KeyType: &TypeDef{
					Kind: TypeDefKindString,
				},
			},
		},
		rows:          make([]*artifactRow, 0, len(roots)),
		objects:       objects,
		typeDefs:      objectTypeDefs,
		topLevelTypes: make(map[string]struct{}, len(roots)),
	}
	for _, row := range roots {
		artifacts.topLevelTypes[row.rootType] = struct{}{}
	}
	artifacts.rows = roots
	if err := artifacts.discoverArtifactShape(); err != nil {
		return nil, err
	}
	for _, row := range roots {
		row.coordinates = make([]dagql.Nullable[dagql.String], len(artifacts.dimensions))
		row.coordinates[0] = dagql.NonNull(
			dagql.NewString(row.targetPatterns[0]),
		)
	}
	for _, row := range roots {
		rootType, ok := artifacts.objects[row.rootType]
		if !ok {
			continue
		}
		if err := artifacts.discoverStaticArtifactRows(
			row,
			rootType,
			row.selectorPath,
			row.coordinates,
			nil,
			nil,
		); err != nil {
			return nil, err
		}
	}
	for _, row := range artifacts.rows {
		row.coordinates = slices.Grow(row.coordinates, len(artifacts.dimensions)-len(row.coordinates))
		row.coordinates = append(
			row.coordinates,
			make([]dagql.Nullable[dagql.String], len(artifacts.dimensions)-len(row.coordinates))...,
		)
	}
	sort.SliceStable(artifacts.rows, func(i, j int) bool {
		if cmp := compareCoordinateRows(
			artifacts.rows[i].coordinates,
			artifacts.rows[j].coordinates,
		); cmp != 0 {
			return cmp < 0
		}
		return selectorPathString(artifacts.rows[i].selectorPath) <
			selectorPathString(artifacts.rows[j].selectorPath)
	})
	return artifacts, nil
}

// NewArtifacts discovers artifact roots and evaluates all reachable collection
// occurrences against the canonical workspace schema.
func NewArtifacts(
	ctx context.Context,
	srv *dagql.Server,
	typeDefs dagql.ObjectResultArray[*TypeDef],
) (*Artifacts, error) {
	artifacts, err := NewArtifactsFromTypeDefs(typeDefs)
	if err != nil {
		return nil, err
	}
	staticRows := slices.Clone(artifacts.rows)
	for _, row := range staticRows {
		rootType, ok := artifacts.objects[row.rootType]
		if !ok {
			continue
		}
		if err := artifacts.discoverCollectionRows(
			ctx,
			srv,
			row,
			rootType,
			nil,
			row.selectorPath,
			row.coordinates,
			nil,
		); err != nil {
			return nil, err
		}
	}
	sort.SliceStable(artifacts.rows, func(i, j int) bool {
		if cmp := compareCoordinateRows(
			artifacts.rows[i].coordinates,
			artifacts.rows[j].coordinates,
		); cmp != 0 {
			return cmp < 0
		}
		return selectorPathString(artifacts.rows[i].selectorPath) <
			selectorPathString(artifacts.rows[j].selectorPath)
	})
	return artifacts, nil
}

func (artifacts *Artifacts) Clone() *Artifacts {
	cp := &Artifacts{
		dimensions:    make([]*ArtifactDimension, len(artifacts.dimensions)),
		rows:          make([]*artifactRow, len(artifacts.rows)),
		objects:       artifacts.objects,
		typeDefs:      artifacts.typeDefs,
		topLevelTypes: artifacts.topLevelTypes,
		workspace:     artifacts.workspace,
		workspaceID:   artifacts.workspaceID,
		filters:       make([]artifactFilter, len(artifacts.filters)),
		materialized:  artifacts.materialized,
		loadFailures:  slices.Clone(artifacts.loadFailures),
		bestEffort:    artifacts.bestEffort,
	}
	for i, dimension := range artifacts.dimensions {
		cp.dimensions[i] = dimension.Clone()
	}
	for i, row := range artifacts.rows {
		cp.rows[i] = row.Clone()
	}
	for i, filter := range artifacts.filters {
		cp.filters[i] = filter.Clone()
	}
	return cp
}

func (filter artifactFilter) Clone() artifactFilter {
	filter.values = slices.Clone(filter.values)
	return filter
}

func (row *artifactRow) Clone() *artifactRow {
	cp := *row
	cp.coordinates = append([]dagql.Nullable[dagql.String](nil), row.coordinates...)
	cp.coordinateDimensions = slices.Clone(row.coordinateDimensions)
	cp.selectorPath = cloneSelectors(row.selectorPath)
	cp.targetPatterns = slices.Clone(row.targetPatterns)
	cp.collectionPath = cloneSelectors(row.collectionPath)
	cp.collectionKeyType = cloneTypeDef(row.collectionKeyType)
	return &cp
}

func (dimension *ArtifactDimension) Clone() *ArtifactDimension {
	cp := *dimension
	cp.KeyType = dimension.KeyType.Clone()
	cp.collectionTypes = slices.Clone(dimension.collectionTypes)
	return &cp
}

func (dimension *ArtifactDimension) CollectionTypes() []string {
	return slices.Clone(dimension.collectionTypes)
}

func (artifact *Artifact) Clone() *Artifact {
	cp := *artifact
	cp.coordinates = append([]dagql.Nullable[dagql.String](nil), artifact.coordinates...)
	cp.coordinateDimensions = slices.Clone(artifact.coordinateDimensions)
	cp.selectorPath = cloneSelectors(artifact.selectorPath)
	cp.targetPatterns = slices.Clone(artifact.targetPatterns)
	cp.collectionPath = cloneSelectors(artifact.collectionPath)
	cp.collectionKeyType = cloneTypeDef(artifact.collectionKeyType)
	return &cp
}

func (artifacts *Artifacts) Dimensions() []*ArtifactDimension {
	dimensions := make([]*ArtifactDimension, len(artifacts.dimensions))
	for i, dimension := range artifacts.dimensions {
		dimensions[i] = dimension.Clone()
	}
	return dimensions
}

func (artifacts *Artifacts) Items() []*Artifact {
	items := make([]*Artifact, len(artifacts.rows))
	for i, row := range artifacts.rows {
		items[i] = &Artifact{
			coordinates:          append([]dagql.Nullable[dagql.String](nil), row.coordinates...),
			coordinateDimensions: slices.Clone(row.coordinateDimensions),
			scope:                artifacts,
			rootField:            row.rootField,
			rootType:             row.rootType,
			sourceModuleName:     row.sourceModuleName,
			selectorPath:         cloneSelectors(row.selectorPath),
			targetPatterns:       slices.Clone(row.targetPatterns),
			collectionPath:       cloneSelectors(row.collectionPath),
			collectionKey:        row.collectionKey,
			collectionKeyType:    cloneTypeDef(row.collectionKeyType),
			collectionDimension:  row.collectionDimension,
			collectionType:       row.collectionType,
			collectionBatchType:  row.collectionBatchType,
			collectionOccurrence: row.collectionOccurrence,
		}
	}
	return items
}

func (artifacts *Artifacts) FilterDimension(dimension string) (*Artifacts, error) {
	if artifacts.workspace != nil && !artifacts.materialized {
		filtered := artifacts.Clone()
		filtered.filters = append(filtered.filters, artifactFilter{
			dimension: dimension,
			nonNull:   true,
		})
		return filtered, nil
	}

	index, err := artifacts.dimensionIndex(dimension)
	if err != nil {
		return nil, err
	}

	filtered := artifacts.Clone()
	filtered.rows = filtered.rows[:0]
	for _, row := range artifacts.rows {
		if row.coordinates[index].Valid {
			filtered.rows = append(filtered.rows, row.Clone())
		}
	}
	return filtered, nil
}

func (artifacts *Artifacts) FilterCoordinates(dimension string, values []string) (*Artifacts, error) {
	if len(values) == 0 {
		return nil, fmt.Errorf("values must not be empty")
	}
	if artifacts.workspace != nil && !artifacts.materialized {
		filtered := artifacts.Clone()
		filtered.filters = append(filtered.filters, artifactFilter{
			dimension: dimension,
			values:    slices.Clone(values),
		})
		return filtered, nil
	}

	index, err := artifacts.dimensionIndex(dimension)
	if err != nil {
		return nil, err
	}

	accepted := make(map[string]struct{}, len(values))
	for _, value := range values {
		if err := validateArtifactCoordinate(artifacts.dimensions[index], value); err != nil {
			return nil, err
		}
		accepted[value] = struct{}{}
	}

	filtered := artifacts.Clone()
	filtered.rows = filtered.rows[:0]
	for _, row := range artifacts.rows {
		if !row.coordinates[index].Valid {
			continue
		}
		if _, ok := accepted[row.coordinates[index].Value.String()]; ok {
			filtered.rows = append(filtered.rows, row.Clone())
		}
	}
	return filtered, nil
}

func (artifacts *Artifacts) Workspace() *Workspace {
	return artifacts.workspace
}

func (artifacts *Artifacts) WorkspaceID() *call.ID {
	return artifacts.workspaceID
}

func (artifacts *Artifacts) IsMaterialized() bool {
	return artifacts.materialized
}

func (artifacts *Artifacts) LoadFailures() []string {
	return slices.Clone(artifacts.loadFailures)
}

func (artifacts *Artifacts) WasMaterializedBestEffort() bool {
	return artifacts.bestEffort
}

func (artifacts *Artifacts) WithMaterializationResult(
	loadFailures []string,
	bestEffort bool,
) *Artifacts {
	result := artifacts.Clone()
	result.loadFailures = slices.Clone(loadFailures)
	result.bestEffort = bestEffort
	return result
}

// WorkspaceModuleSelectors returns the narrowest selectors known before the
// workspace schema is loaded. An explicit type filter takes precedence over
// target patterns; otherwise the target patterns preserve type-qualified
// selectors such as "go:lint".
func (artifacts *Artifacts) WorkspaceModuleSelectors(include []TargetPattern) []string {
	var selectors []string
	for _, filter := range artifacts.filters {
		if filter.dimension == ArtifactTypeDimension && len(filter.values) > 0 {
			selectors = append(selectors, filter.values...)
		}
	}
	if len(selectors) > 0 {
		return selectors
	}
	for _, pattern := range include {
		selectors = append(selectors, string(pattern))
	}
	return selectors
}

// Materialize applies the filters recorded on a workspace-backed scope to an
// artifact snapshot built from the loaded workspace schema.
func (artifacts *Artifacts) Materialize(snapshot *Artifacts) (*Artifacts, error) {
	if artifacts.workspace == nil || artifacts.materialized {
		return artifacts, nil
	}
	materialized := snapshot.Clone()
	materialized.workspace = artifacts.workspace
	materialized.workspaceID = artifacts.workspaceID
	materialized.materialized = true
	var err error
	for _, filter := range artifacts.filters {
		if filter.nonNull {
			materialized, err = materialized.FilterDimension(filter.dimension)
		} else {
			materialized, err = materialized.FilterCoordinates(filter.dimension, filter.values)
		}
		if err != nil {
			return nil, err
		}
	}
	return materialized, nil
}

func (artifacts *Artifacts) dimensionIndex(name string) (int, error) {
	for i, dimension := range artifacts.dimensions {
		if dimension.Name == name {
			return i, nil
		}
	}
	return 0, fmt.Errorf("artifact dimension %q not found", name)
}

func (artifact *Artifact) Coordinates() []dagql.Nullable[dagql.String] {
	return append([]dagql.Nullable[dagql.String](nil), artifact.coordinates...)
}

func (artifact *Artifact) CoordinateDimensions() []string {
	return slices.Clone(artifact.coordinateDimensions)
}

func (artifact *Artifact) Coordinate(name string) (dagql.Nullable[dagql.String], error) {
	index, err := artifact.scope.dimensionIndex(name)
	if err != nil {
		return dagql.Null[dagql.String](), err
	}
	return artifact.coordinates[index], nil
}

func (artifact *Artifact) HasCoordinate(name string) (bool, error) {
	coordinate, err := artifact.Coordinate(name)
	if err != nil {
		return false, err
	}
	return coordinate.Valid, nil
}

func (artifact *Artifact) Scope() *Artifacts {
	return artifact.scope
}

func (artifact *Artifact) targetScope() *Artifacts {
	target := artifact.scope.Clone()
	target.rows = []*artifactRow{{
		coordinates:          append([]dagql.Nullable[dagql.String](nil), artifact.coordinates...),
		coordinateDimensions: slices.Clone(artifact.coordinateDimensions),
		rootField:            artifact.rootField,
		rootType:             artifact.rootType,
		sourceModuleName:     artifact.sourceModuleName,
		selectorPath:         cloneSelectors(artifact.selectorPath),
		targetPatterns:       slices.Clone(artifact.targetPatterns),
		collectionPath:       cloneSelectors(artifact.collectionPath),
		collectionKey:        artifact.collectionKey,
		collectionKeyType:    cloneTypeDef(artifact.collectionKeyType),
		collectionDimension:  artifact.collectionDimension,
		collectionType:       artifact.collectionType,
		collectionBatchType:  artifact.collectionBatchType,
		collectionOccurrence: artifact.collectionOccurrence,
	}}
	return target
}

func (artifacts *Artifacts) discoverArtifactShape() error {
	var walk func(*TypeDef, map[string]struct{}, map[string]struct{}) error
	walk = func(
		typeDef *TypeDef,
		artifactDimensions map[string]struct{},
		stack map[string]struct{},
	) error {
		typeDef = artifacts.fullObjectTypeDef(typeDef)
		object := objectTypeDef(typeDef)
		if object == nil {
			return nil
		}
		if typeDef.AsCollection.Valid {
			collection := typeDef.AsCollection.Value
			itemType := artifacts.fullObjectTypeDef(collection.ValueType)
			itemObject := objectTypeDef(itemType)
			if itemObject == nil {
				return nil
			}
			dimension := artifactTypeCLIName(itemObject)
			if _, repeated := artifactDimensions[dimension]; repeated {
				return fmt.Errorf(
					"collection %q repeats artifact dimension %q",
					object.Name,
					dimension,
				)
			}
			if err := artifacts.addArtifactDimension(
				dimension,
				artifactTypeCLIName(object),
			); err != nil {
				return err
			}
			nextDimensions := cloneStringSet(artifactDimensions)
			nextDimensions[dimension] = struct{}{}
			return walk(itemType, nextDimensions, nil)
		}

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
			childType := artifacts.fullObjectTypeDef(field.TypeDef.Self())
			childObject := objectTypeDef(childType)
			if childObject == nil {
				continue
			}
			if _, boundary := artifacts.topLevelTypes[childObject.Name]; boundary {
				continue
			}
			if childType.AsCollection.Valid {
				if err := walk(childType, artifactDimensions, stack); err != nil {
					return err
				}
				continue
			}
			if isStaticArtifactField(childType) {
				dimension := artifactTypeCLIName(childObject)
				if _, repeated := artifactDimensions[dimension]; repeated {
					return fmt.Errorf(
						"static artifact field %s.%s repeats artifact dimension %q",
						object.Name,
						field.Name,
						dimension,
					)
				}
				if err := artifacts.addArtifactDimension(dimension, ""); err != nil {
					return err
				}
				nextDimensions := cloneStringSet(artifactDimensions)
				nextDimensions[dimension] = struct{}{}
				if err := walk(childType, nextDimensions, nil); err != nil {
					return err
				}
				continue
			}
			if childType.Optional || childObject.SourceModuleName == "" {
				continue
			}
			if err := walk(childType, artifactDimensions, stack); err != nil {
				return err
			}
		}

		for _, fnResult := range object.Functions {
			fn := fnResult.Self()
			if fn == nil || fn.IsCheck || fn.IsGenerator || fn.IsUp ||
				functionRequiresArgs(fn) || fn.ReturnType.Self() == nil {
				continue
			}
			childType := artifacts.fullObjectTypeDef(fn.ReturnType.Self())
			childObject := objectTypeDef(childType)
			if childObject == nil {
				continue
			}
			if _, boundary := artifacts.topLevelTypes[childObject.Name]; boundary {
				continue
			}
			if childType.AsCollection.Valid {
				if err := walk(childType, artifactDimensions, stack); err != nil {
					return err
				}
				continue
			}
			if childObject.SourceModuleName == "" {
				continue
			}
			if err := walk(childType, artifactDimensions, stack); err != nil {
				return err
			}
		}
		return nil
	}

	for _, row := range artifacts.rows {
		if root, ok := artifacts.typeDefs[row.rootType]; ok {
			if err := walk(root, nil, nil); err != nil {
				return err
			}
		}
	}
	return nil
}

func (artifacts *Artifacts) addArtifactDimension(name, collectionType string) error {
	if name == ArtifactTypeDimension {
		return fmt.Errorf("artifact dimension %q is reserved", name)
	}
	for _, dimension := range artifacts.dimensions {
		if dimension.Name != name {
			continue
		}
		if collectionType != "" &&
			!slices.Contains(dimension.collectionTypes, collectionType) {
			dimension.collectionTypes = append(dimension.collectionTypes, collectionType)
			sort.Strings(dimension.collectionTypes)
		}
		return nil
	}
	dimension := &ArtifactDimension{
		Name: name,
		KeyType: &TypeDef{
			Kind: TypeDefKindString,
		},
	}
	if collectionType != "" {
		dimension.collectionTypes = []string{collectionType}
	}
	artifacts.dimensions = append(artifacts.dimensions, dimension)
	return nil
}

func isStaticArtifactField(typeDef *TypeDef) bool {
	object := objectTypeDef(typeDef)
	return typeDef != nil &&
		!typeDef.Optional &&
		!typeDef.AsCollection.Valid &&
		object != nil &&
		object.SourceModuleName != ""
}

func artifactCLIName(name string) string {
	runes := []rune(strings.TrimSpace(name))
	var normalized strings.Builder
	for i, current := range runes {
		if current == '-' || current == '_' || unicode.IsSpace(current) {
			if normalized.Len() > 0 {
				normalized.WriteByte('-')
			}
			continue
		}
		if unicode.IsUpper(current) && i > 0 {
			previous := runes[i-1]
			nextIsLower := i+1 < len(runes) && unicode.IsLower(runes[i+1])
			if unicode.IsLower(previous) ||
				unicode.IsUpper(previous) && nextIsLower {
				normalized.WriteByte('-')
			}
		}
		normalized.WriteRune(unicode.ToLower(current))
	}
	return strings.Trim(normalized.String(), "-")
}

func artifactTypeCLIName(object *ObjectTypeDef) string {
	if object == nil {
		return ""
	}
	if object.IsMainObject {
		return artifactCLIName(object.Name)
	}
	moduleName := artifactCLIName(object.SourceModuleName)
	originalName := artifactCLIName(object.OriginalName)
	if moduleName == "" || originalName == "" {
		return artifactCLIName(object.Name)
	}
	if moduleName == originalName {
		return moduleName
	}
	return moduleName + "-" + originalName
}

func artifactFieldCoordinate(object *ObjectTypeDef, field *FieldTypeDef) string {
	objectName := object.OriginalName
	if objectName == "" {
		objectName = object.Name
	}
	return artifactCLIName(objectName) + ":" + strcase.ToKebab(field.Name)
}

func artifactFieldTargetPatterns(
	parentPatterns []string,
	object *ObjectTypeDef,
	field string,
) []string {
	field = strcase.ToKebab(field)
	patterns := make([]string, 0, len(parentPatterns)+1)
	patterns = append(patterns, artifactTypeCLIName(object)+":"+field)
	for _, parent := range parentPatterns {
		pattern := parent + ":" + field
		if !slices.Contains(patterns, pattern) {
			patterns = append(patterns, pattern)
		}
	}
	return patterns
}

func (artifacts *Artifacts) discoverStaticArtifactRows(
	root *artifactRow,
	object *ObjectTypeDef,
	selectorPath []dagql.Selector,
	coordinates []dagql.Nullable[dagql.String],
	targetPatterns []string,
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
		childType := artifacts.fullObjectTypeDef(field.TypeDef.Self())
		childObject := objectTypeDef(childType)
		if childObject == nil {
			continue
		}
		if _, boundary := artifacts.topLevelTypes[childObject.Name]; boundary {
			continue
		}
		if childType.AsCollection.Valid {
			continue
		}
		childPath := appendSelector(selectorPath, dagql.Selector{Field: field.Name})
		childTargetPatterns := artifactFieldTargetPatterns(
			targetPatterns,
			object,
			field.Name,
		)
		if isStaticArtifactField(childType) {
			dimension := artifactTypeCLIName(childObject)
			dimensionIndex, err := artifacts.dimensionIndex(dimension)
			if err != nil {
				return err
			}
			if coordinates[dimensionIndex].Valid {
				return fmt.Errorf(
					"static artifact field %s.%s repeats artifact dimension %q",
					object.Name,
					field.Name,
					dimension,
				)
			}
			fieldCoordinate := artifactFieldCoordinate(object, field)
			childCoordinates := append(
				[]dagql.Nullable[dagql.String](nil),
				coordinates...,
			)
			childCoordinates[0] = dagql.NonNull(
				dagql.NewString(artifactTypeCLIName(childObject)),
			)
			childCoordinates[dimensionIndex] = dagql.NonNull(
				dagql.NewString(fieldCoordinate),
			)
			sourceModuleName := childObject.SourceModuleName
			if sourceModuleName == "" {
				sourceModuleName = root.sourceModuleName
			}
			childRow := &artifactRow{
				coordinates: childCoordinates,
				coordinateDimensions: append(
					slices.Clone(root.coordinateDimensions),
					dimension,
				),
				rootField:        root.rootField,
				rootType:         childObject.Name,
				sourceModuleName: sourceModuleName,
				selectorPath:     childPath,
				targetPatterns:   childTargetPatterns,
			}
			artifacts.rows = append(artifacts.rows, childRow)
			if err := artifacts.discoverStaticArtifactRows(
				childRow,
				childObject,
				childPath,
				childCoordinates,
				childTargetPatterns,
				nil,
			); err != nil {
				return err
			}
			continue
		}
		if childType.Optional || childObject.SourceModuleName == "" {
			continue
		}
		if err := artifacts.discoverStaticArtifactRows(
			root,
			childObject,
			childPath,
			coordinates,
			childTargetPatterns,
			stack,
		); err != nil {
			return err
		}
	}

	for _, fnResult := range object.Functions {
		fn := fnResult.Self()
		if fn == nil || fn.IsCheck || fn.IsGenerator || fn.IsUp ||
			functionRequiresArgs(fn) || fn.ReturnType.Self() == nil {
			continue
		}
		childType := artifacts.fullObjectTypeDef(fn.ReturnType.Self())
		childObject := objectTypeDef(childType)
		if childObject == nil {
			continue
		}
		if _, boundary := artifacts.topLevelTypes[childObject.Name]; boundary {
			continue
		}
		if childType.AsCollection.Valid || childObject.SourceModuleName == "" {
			continue
		}
		childTargetPatterns := artifactFieldTargetPatterns(
			targetPatterns,
			object,
			fn.Name,
		)
		if err := artifacts.discoverStaticArtifactRows(
			root,
			childObject,
			appendSelector(selectorPath, dagql.Selector{Field: fn.Name}),
			coordinates,
			childTargetPatterns,
			stack,
		); err != nil {
			return err
		}
	}
	return nil
}

func (artifacts *Artifacts) discoverCollectionRows(
	ctx context.Context,
	srv *dagql.Server,
	root *artifactRow,
	object *ObjectTypeDef,
	objectResult dagql.AnyObjectResult,
	selectorPath []dagql.Selector,
	coordinates []dagql.Nullable[dagql.String],
	stack map[string]struct{},
) error {
	if _, found := stack[object.Name]; found {
		return nil
	}
	stack = cloneStringSet(stack)
	stack[object.Name] = struct{}{}
	resolvedObject := objectResult
	resolveObject := func() (dagql.AnyObjectResult, error) {
		if resolvedObject != nil {
			return resolvedObject, nil
		}
		if err := srv.Select(
			ctx,
			srv.Root(),
			&resolvedObject,
			selectorPath...,
		); err != nil {
			return nil, fmt.Errorf(
				"resolve artifact object %s: %w",
				selectorPathString(selectorPath),
				err,
			)
		}
		return resolvedObject, nil
	}

	for _, child := range artifactTraversalChildren(object) {
		childTypeDef := artifacts.fullObjectTypeDef(child.typeDef)
		childObject := objectTypeDef(childTypeDef)
		if childObject == nil {
			continue
		}
		if _, boundary := artifacts.topLevelTypes[childObject.Name]; boundary {
			continue
		}
		if child.staticField && isStaticArtifactField(childTypeDef) {
			continue
		}
		if !childTypeDef.AsCollection.Valid &&
			!artifacts.objectHasReachableCollection(childObject, stack) {
			continue
		}

		childPath := appendSelector(selectorPath, dagql.Selector{Field: child.field})
		parentResult, err := resolveObject()
		if err != nil {
			return err
		}
		var childResult dagql.AnyObjectResult
		if err := srv.Select(
			ctx,
			parentResult,
			&childResult,
			dagql.Selector{Field: child.field},
		); err != nil {
			return fmt.Errorf(
				"resolve artifact field %s: %w",
				selectorPathString(childPath),
				err,
			)
		}
		if childTypeDef.AsCollection.Valid {
			if err := artifacts.discoverCollectionOccurrence(
				ctx,
				srv,
				root,
				childTypeDef,
				childResult,
				childPath,
				coordinates,
			); err != nil {
				return err
			}
			continue
		}
		if err := artifacts.discoverCollectionRows(
			ctx,
			srv,
			root,
			childObject,
			childResult,
			childPath,
			coordinates,
			stack,
		); err != nil {
			return err
		}
	}
	return nil
}

func (artifacts *Artifacts) discoverCollectionOccurrence(
	ctx context.Context,
	srv *dagql.Server,
	root *artifactRow,
	collectionTypeDef *TypeDef,
	collectionObject dagql.AnyObjectResult,
	collectionPath []dagql.Selector,
	parentCoordinates []dagql.Nullable[dagql.String],
) error {
	collectionTypeObject := objectTypeDef(collectionTypeDef)
	if collectionTypeObject == nil {
		return fmt.Errorf("collection occurrence has no object type")
	}
	collection := collectionTypeDef.AsCollection.Value
	itemType := artifacts.fullObjectTypeDef(collection.ValueType)
	itemObject := objectTypeDef(itemType)
	if itemObject == nil {
		return fmt.Errorf(
			"collection %q has no object value type",
			collectionTypeObject.Name,
		)
	}

	var keysResult dagql.AnyResult
	if err := srv.Select(
		ctx,
		collectionObject,
		&keysResult,
		dagql.Selector{Field: collectionKeysFieldName},
	); err != nil {
		return fmt.Errorf(
			"resolve keys for collection occurrence %s: %w",
			selectorPathString(collectionPath),
			err,
		)
	}
	keys, ok := dagql.UnwrapAs[dagql.Enumerable](keysResult)
	if !ok {
		return fmt.Errorf(
			"collection %q keys returned %T, expected a list",
			collectionTypeObject.Name,
			keysResult.Unwrap(),
		)
	}

	dimension := artifactTypeCLIName(itemObject)
	dimensionIndex, err := artifacts.dimensionIndex(dimension)
	if err != nil {
		return err
	}
	occurrence := selectorPathString(collectionPath)
	seenKeys := make(map[string]struct{}, keys.Len())
	type collectionItem struct {
		key         dagql.Input
		selector    dagql.Selector
		path        []dagql.Selector
		coordinates []dagql.Nullable[dagql.String]
	}
	items := make([]collectionItem, 0, keys.Len())
	for index := 1; index <= keys.Len(); index++ {
		keyResult, err := keysResult.NthValue(ctx, index)
		if err != nil {
			return fmt.Errorf(
				"resolve key %d for collection occurrence %s: %w",
				index,
				occurrence,
				err,
			)
		}
		key, ok := keyResult.Unwrap().(dagql.Input)
		if !ok {
			return fmt.Errorf(
				"collection %q key %d has unsupported type %T",
				collectionTypeObject.Name,
				index,
				keyResult.Unwrap(),
			)
		}
		keyID, err := collectionKeyID(key)
		if err != nil {
			return fmt.Errorf(
				"identify key %d for collection occurrence %s: %w",
				index,
				occurrence,
				err,
			)
		}
		if _, exists := seenKeys[keyID]; exists {
			return fmt.Errorf(
				"collection %q contains duplicate key %s",
				collectionTypeObject.Name,
				keyID,
			)
		}
		seenKeys[keyID] = struct{}{}
		coordinate, err := collectionCoordinate(key)
		if err != nil {
			return fmt.Errorf(
				"render key %d for collection occurrence %s: %w",
				index,
				occurrence,
				err,
			)
		}
		coordinate = artifactCollectionCoordinate(coordinate)

		itemCoordinates := append(
			[]dagql.Nullable[dagql.String](nil),
			parentCoordinates...,
		)
		if itemCoordinates[dimensionIndex].Valid {
			return fmt.Errorf(
				"collection occurrence %s repeats artifact dimension %q",
				occurrence,
				dimension,
			)
		}
		itemCoordinates[0] = dagql.NonNull(
			dagql.NewString(artifactTypeCLIName(itemObject)),
		)
		itemCoordinates[dimensionIndex] = dagql.NonNull(dagql.NewString(coordinate))

		itemSelector := dagql.Selector{
			Field: collectionGetFunctionName,
			Args: []dagql.NamedInput{{
				Name:  collectionKeyArgName,
				Value: key,
			}},
		}
		items = append(items, collectionItem{
			key:         key,
			selector:    itemSelector,
			path:        appendSelector(collectionPath, itemSelector),
			coordinates: itemCoordinates,
		})
	}

	branches := make([][]*artifactRow, len(items))
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(16)
	for index, item := range items {
		group.Go(func() error {
			branch := *artifacts
			branch.rows = nil
			itemRow := &artifactRow{
				coordinates:          item.coordinates,
				coordinateDimensions: append(slices.Clone(root.coordinateDimensions), dimension),
				rootField:            root.rootField,
				rootType:             itemObject.Name,
				sourceModuleName:     root.sourceModuleName,
				selectorPath:         item.path,
				collectionPath:       cloneSelectors(collectionPath),
				collectionKey:        item.key,
				collectionKeyType:    collection.KeyType.Clone(),
				collectionDimension:  dimension,
				collectionType:       artifactTypeCLIName(objectTypeDef(collectionTypeDef)),
				collectionOccurrence: occurrence,
			}
			if batchObject := objectTypeDef(collection.BatchType); batchObject != nil {
				itemRow.collectionBatchType = batchObject.Name
			}
			branch.rows = append(branch.rows, itemRow)

			firstStaticRow := len(branch.rows)
			if err := branch.discoverStaticArtifactRows(
				itemRow,
				itemObject,
				item.path,
				item.coordinates,
				nil,
				nil,
			); err != nil {
				return err
			}
			dynamicRows := append(
				[]*artifactRow{itemRow},
				branch.rows[firstStaticRow:]...,
			)
			for dynamicIndex, dynamicRow := range dynamicRows {
				dynamicObject, ok := branch.objects[dynamicRow.rootType]
				if !ok {
					continue
				}
				var dynamicObjectResult dagql.AnyObjectResult
				if dynamicIndex == 0 &&
					branch.objectHasReachableCollection(dynamicObject, nil) {
					if err := srv.Select(
						groupCtx,
						collectionObject,
						&dynamicObjectResult,
						item.selector,
					); err != nil {
						return fmt.Errorf(
							"resolve collection item %s: %w",
							selectorPathString(item.path),
							err,
						)
					}
				}
				if err := branch.discoverCollectionRows(
					groupCtx,
					srv,
					dynamicRow,
					dynamicObject,
					dynamicObjectResult,
					dynamicRow.selectorPath,
					dynamicRow.coordinates,
					nil,
				); err != nil {
					return err
				}
			}
			branches[index] = branch.rows
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return err
	}
	for _, rows := range branches {
		artifacts.rows = append(artifacts.rows, rows...)
	}
	return nil
}

func (artifacts *Artifacts) objectHasReachableCollection(
	object *ObjectTypeDef,
	stack map[string]struct{},
) bool {
	if _, found := stack[object.Name]; found {
		return false
	}
	stack = cloneStringSet(stack)
	stack[object.Name] = struct{}{}

	for _, child := range artifactTraversalChildren(object) {
		childTypeDef := artifacts.fullObjectTypeDef(child.typeDef)
		childObject := objectTypeDef(childTypeDef)
		if childObject == nil {
			continue
		}
		if _, boundary := artifacts.topLevelTypes[childObject.Name]; boundary {
			continue
		}
		if child.staticField && isStaticArtifactField(childTypeDef) {
			continue
		}
		if childTypeDef.AsCollection.Valid ||
			artifacts.objectHasReachableCollection(childObject, stack) {
			return true
		}
	}
	return false
}

func artifactCollectionCoordinate(coordinate string) string {
	coordinate = strings.ReplaceAll(coordinate, "%", "%25")
	return strings.ReplaceAll(coordinate, ":", "%3A")
}

type artifactTraversalChild struct {
	field       string
	typeDef     *TypeDef
	staticField bool
}

func artifactTraversalChildren(object *ObjectTypeDef) []artifactTraversalChild {
	var children []artifactTraversalChild
	for _, fieldResult := range object.Fields {
		field := fieldResult.Self()
		if field == nil || field.TypeDef.Self() == nil {
			continue
		}
		typeDef := field.TypeDef.Self()
		if objectTypeDef(typeDef) != nil {
			children = append(children, artifactTraversalChild{
				field:       field.Name,
				typeDef:     typeDef,
				staticField: true,
			})
		}
	}
	for _, fnResult := range object.Functions {
		fn := fnResult.Self()
		if fn == nil || fn.IsCheck || fn.IsGenerator || functionRequiresArgs(fn) ||
			fn.ReturnType.Self() == nil || objectTypeDef(fn.ReturnType.Self()) == nil {
			continue
		}
		children = append(children, artifactTraversalChild{
			field:   fn.Name,
			typeDef: fn.ReturnType.Self(),
		})
	}
	sort.SliceStable(children, func(i, j int) bool {
		return children[i].field < children[j].field
	})
	return children
}

func (artifacts *Artifacts) fullObjectTypeDef(typeDef *TypeDef) *TypeDef {
	if object := objectTypeDef(typeDef); object != nil {
		if full, ok := artifacts.typeDefs[object.Name]; ok {
			full = full.Clone()
			full.Optional = typeDef.Optional
			return full
		}
	}
	return typeDef
}

func objectTypeDef(typeDef *TypeDef) *ObjectTypeDef {
	if typeDef == nil || !typeDef.AsObject.Valid {
		return nil
	}
	return typeDef.AsObject.Value.Self()
}

func appendSelector(path []dagql.Selector, selector dagql.Selector) []dagql.Selector {
	next := cloneSelectors(path)
	selector.Args = slices.Clone(selector.Args)
	return append(next, selector)
}

func cloneSelectors(selectors []dagql.Selector) []dagql.Selector {
	cloned := make([]dagql.Selector, len(selectors))
	for i, selector := range selectors {
		cloned[i] = selector
		cloned[i].Args = slices.Clone(selector.Args)
	}
	return cloned
}

func cloneTypeDef(typeDef *TypeDef) *TypeDef {
	if typeDef == nil {
		return nil
	}
	return typeDef.Clone()
}

func selectorPathString(selectors []dagql.Selector) string {
	parts := make([]string, len(selectors))
	for i, selector := range selectors {
		parts[i] = selector.String()
	}
	return strings.Join(parts, ".")
}

func validateArtifactCoordinate(dimension *ArtifactDimension, value string) error {
	if dimension.KeyType.Kind == TypeDefKindEnum {
		enum := dimension.KeyType.AsEnum.Value.Self()
		found := enum != nil && slices.ContainsFunc(
			enum.Members,
			func(memberResult dagql.ObjectResult[*EnumMemberTypeDef]) bool {
				member := memberResult.Self()
				return member != nil && member.Name == value
			},
		)
		if !found {
			return fmt.Errorf(
				"invalid coordinate %q for artifact dimension %q",
				value,
				dimension.Name,
			)
		}
		return nil
	}
	if _, err := dimension.KeyType.ToInput().Decoder().DecodeInput(value); err != nil {
		return fmt.Errorf(
			"invalid coordinate %q for artifact dimension %q: %w",
			value,
			dimension.Name,
			err,
		)
	}
	return nil
}
