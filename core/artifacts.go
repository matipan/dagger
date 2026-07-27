package core

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/dagger/dagger/dagql"
	"github.com/dagger/dagger/dagql/call"
	"github.com/iancoleman/strcase"
	"github.com/vektah/gqlparser/v2/ast"
)

const ArtifactTypeDimension = "type"

// Artifacts is an immutable, filterable view over workspace artifacts.
type Artifacts struct {
	dimensions   []*ArtifactDimension
	rows         []*artifactRow
	objects      map[string]*ObjectTypeDef
	typeDefs     map[string]*TypeDef
	rootTypes    map[string]struct{}
	workspace    *Workspace
	workspaceID  *call.ID
	filters      []artifactFilter
	materialized bool
}

type artifactFilter struct {
	dimension string
	values    []string
	nonNull   bool
}

type artifactRow struct {
	coordinates          []dagql.Nullable[dagql.String]
	rootField            string
	rootType             string
	sourceModuleName     string
	selectorPath         []dagql.Selector
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
	scope                *Artifacts
	rootField            string
	rootType             string
	sourceModuleName     string
	selectorPath         []dagql.Selector
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
		objects:     map[string]*ObjectTypeDef{},
		typeDefs:    map[string]*TypeDef{},
		rootTypes:   map[string]struct{}{},
		workspace:   workspace,
		workspaceID: workspaceID,
	}
}

// NewArtifactsFromTypeDefs discovers the schema-stable artifact shape without
// evaluating dynamic collection keys.
func NewArtifactsFromTypeDefs(typeDefs dagql.ObjectResultArray[*TypeDef]) *Artifacts {
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
		objectTypeDef := typeDef.AsObject.Value.Self()
		if objectTypeDef.Name != "Query" {
			continue
		}
		for _, fnResult := range objectTypeDef.Functions {
			fn := fnResult.Self()
			if fn == nil || fn.SourceModuleName == "" || fn.ReturnType.Self() == nil {
				continue
			}
			returnType := fn.ReturnType.Self()
			if !returnType.AsObject.Valid || returnType.AsObject.Value.Self() == nil {
				continue
			}
			typeName := returnType.AsObject.Value.Self().Name
			roots = append(roots, &artifactRow{
				rootField:        fn.Name,
				rootType:         typeName,
				sourceModuleName: fn.SourceModuleName,
				selectorPath:     []dagql.Selector{{Field: fn.Name}},
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
		rows:      make([]*artifactRow, 0, len(roots)),
		objects:   objects,
		typeDefs:  objectTypeDefs,
		rootTypes: make(map[string]struct{}, len(roots)),
	}
	for _, row := range roots {
		row.coordinates = []dagql.Nullable[dagql.String]{
			dagql.NonNull(dagql.NewString(strcase.ToKebab(row.rootType))),
		}
		artifacts.rows = append(artifacts.rows, row)
		artifacts.rootTypes[row.rootType] = struct{}{}
	}
	artifacts.discoverCollectionDimensions(typeDefs)
	for _, row := range artifacts.rows {
		row.coordinates = slices.Grow(row.coordinates, len(artifacts.dimensions)-1)
		for len(row.coordinates) < len(artifacts.dimensions) {
			row.coordinates = append(row.coordinates, dagql.Null[dagql.String]())
		}
	}
	return artifacts
}

// NewArtifacts discovers artifact roots and evaluates all reachable collection
// occurrences against the canonical workspace schema.
func NewArtifacts(
	ctx context.Context,
	srv *dagql.Server,
	typeDefs dagql.ObjectResultArray[*TypeDef],
) (*Artifacts, error) {
	artifacts := NewArtifactsFromTypeDefs(typeDefs)
	topLevelRows := slices.Clone(artifacts.rows)
	for _, row := range topLevelRows {
		rootType, ok := artifacts.objects[row.rootType]
		if !ok {
			continue
		}
		if err := artifacts.discoverCollectionRows(
			ctx,
			srv,
			row,
			rootType,
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
		dimensions:   make([]*ArtifactDimension, len(artifacts.dimensions)),
		rows:         make([]*artifactRow, len(artifacts.rows)),
		objects:      artifacts.objects,
		typeDefs:     artifacts.typeDefs,
		rootTypes:    artifacts.rootTypes,
		workspace:    artifacts.workspace,
		workspaceID:  artifacts.workspaceID,
		filters:      make([]artifactFilter, len(artifacts.filters)),
		materialized: artifacts.materialized,
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
	cp.selectorPath = cloneSelectors(row.selectorPath)
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

func (artifact *Artifact) Clone() *Artifact {
	cp := *artifact
	cp.coordinates = append([]dagql.Nullable[dagql.String](nil), artifact.coordinates...)
	cp.selectorPath = cloneSelectors(artifact.selectorPath)
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
			scope:                artifacts,
			rootField:            row.rootField,
			rootType:             row.rootType,
			sourceModuleName:     row.sourceModuleName,
			selectorPath:         cloneSelectors(row.selectorPath),
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

// WorkspaceModuleSelectors returns the narrowest selectors known before the
// workspace schema is loaded. An explicit type filter takes precedence over
// action patterns; otherwise the action patterns preserve legacy
// type-qualified selectors such as "go:lint".
func (artifacts *Artifacts) WorkspaceModuleSelectors(include []FunctionPattern) []string {
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

func (artifact *Artifact) Coordinate(name string) (dagql.Nullable[dagql.String], error) {
	index, err := artifact.scope.dimensionIndex(name)
	if err != nil {
		return dagql.Null[dagql.String](), err
	}
	return artifact.coordinates[index], nil
}

func (artifact *Artifact) Scope() *Artifacts {
	return artifact.scope
}

func (artifact *Artifact) targetScope() *Artifacts {
	target := artifact.scope.Clone()
	target.rows = []*artifactRow{{
		coordinates:          append([]dagql.Nullable[dagql.String](nil), artifact.coordinates...),
		rootField:            artifact.rootField,
		rootType:             artifact.rootType,
		sourceModuleName:     artifact.sourceModuleName,
		selectorPath:         cloneSelectors(artifact.selectorPath),
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

func (artifacts *Artifacts) discoverCollectionDimensions(
	typeDefs dagql.ObjectResultArray[*TypeDef],
) {
	fullTypeDefs := make(map[string]*TypeDef, len(typeDefs))
	for _, typeDefResult := range typeDefs {
		typeDef := typeDefResult.Self()
		object := objectTypeDef(typeDef)
		if object != nil {
			fullTypeDefs[object.Name] = typeDef
		}
	}

	seenDimensions := map[string]*ArtifactDimension{}
	var walk func(*TypeDef, map[string]struct{})
	walk = func(typeDef *TypeDef, stack map[string]struct{}) {
		typeDef = fullObjectTypeDef(typeDef, fullTypeDefs)
		object := objectTypeDef(typeDef)
		if object == nil {
			return
		}
		if typeDef.AsCollection.Valid {
			collection := typeDef.AsCollection.Value
			valueObject := objectTypeDef(collection.ValueType)
			if valueObject == nil {
				return
			}
			dimensionName := strcase.ToKebab(valueObject.Name)
			dimension, found := seenDimensions[dimensionName]
			if !found {
				dimension = &ArtifactDimension{
					Name:    dimensionName,
					KeyType: collection.KeyType.Clone(),
				}
				seenDimensions[dimensionName] = dimension
				artifacts.dimensions = append(artifacts.dimensions, dimension)
			}
			collectionType := strcase.ToKebab(object.Name)
			if !slices.Contains(dimension.collectionTypes, collectionType) {
				dimension.collectionTypes = append(dimension.collectionTypes, collectionType)
				sort.Strings(dimension.collectionTypes)
			}
			walk(collection.ValueType, stack)
			return
		}

		if _, found := stack[object.Name]; found {
			return
		}
		stack = cloneStringSet(stack)
		stack[object.Name] = struct{}{}

		for _, child := range artifactTraversalChildren(object) {
			childDef := fullObjectTypeDef(child.typeDef, fullTypeDefs)
			childObject := objectTypeDef(childDef)
			if childObject == nil {
				continue
			}
			if _, boundary := artifacts.rootTypes[childObject.Name]; boundary {
				continue
			}
			walk(childDef, stack)
		}
	}

	for _, row := range artifacts.rows {
		if root, ok := fullTypeDefs[row.rootType]; ok {
			walk(root, nil)
		}
	}
}

func (artifacts *Artifacts) discoverCollectionRows(
	ctx context.Context,
	srv *dagql.Server,
	root *artifactRow,
	object *ObjectTypeDef,
	selectorPath []dagql.Selector,
	coordinates []dagql.Nullable[dagql.String],
	stack map[string]struct{},
) error {
	if _, found := stack[object.Name]; found {
		return nil
	}
	stack = cloneStringSet(stack)
	stack[object.Name] = struct{}{}

	for _, child := range artifactTraversalChildren(object) {
		childTypeDef := artifacts.fullObjectTypeDef(child.typeDef)
		childObject := objectTypeDef(childTypeDef)
		if childObject == nil {
			continue
		}
		if _, boundary := artifacts.rootTypes[childObject.Name]; boundary {
			continue
		}

		childPath := appendSelector(selectorPath, dagql.Selector{Field: child.field})
		if childTypeDef.AsCollection.Valid {
			if err := artifacts.discoverCollectionOccurrence(
				ctx,
				srv,
				root,
				childTypeDef,
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
	collectionPath []dagql.Selector,
	parentCoordinates []dagql.Nullable[dagql.String],
) error {
	collection := collectionTypeDef.AsCollection.Value
	itemType := artifacts.fullObjectTypeDef(collection.ValueType)
	itemObject := objectTypeDef(itemType)
	if itemObject == nil {
		return fmt.Errorf(
			"collection %q has no object value type",
			objectTypeDef(collectionTypeDef).Name,
		)
	}

	var collectionObject dagql.AnyObjectResult
	if err := srv.Select(ctx, srv.Root(), &collectionObject, collectionPath...); err != nil {
		return fmt.Errorf(
			"resolve collection occurrence %s: %w",
			selectorPathString(collectionPath),
			err,
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
			objectTypeDef(collectionTypeDef).Name,
			keysResult.Unwrap(),
		)
	}

	dimension := strcase.ToKebab(itemObject.Name)
	dimensionIndex, err := artifacts.dimensionIndex(dimension)
	if err != nil {
		return err
	}
	occurrence := selectorPathString(collectionPath)
	seenKeys := make(map[string]struct{}, keys.Len())
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
				objectTypeDef(collectionTypeDef).Name,
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
				objectTypeDef(collectionTypeDef).Name,
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
			dagql.NewString(strcase.ToKebab(itemObject.Name)),
		)
		itemCoordinates[dimensionIndex] = dagql.NonNull(dagql.NewString(coordinate))

		itemPath := appendSelector(collectionPath, dagql.Selector{
			Field: collectionGetFunctionName,
			Args: []dagql.NamedInput{{
				Name:  collectionKeyArgName,
				Value: key,
			}},
		})
		itemRow := &artifactRow{
			coordinates:          itemCoordinates,
			rootField:            root.rootField,
			rootType:             itemObject.Name,
			sourceModuleName:     root.sourceModuleName,
			selectorPath:         itemPath,
			collectionPath:       cloneSelectors(collectionPath),
			collectionKey:        key,
			collectionKeyType:    collection.KeyType.Clone(),
			collectionDimension:  dimension,
			collectionType:       strcase.ToKebab(objectTypeDef(collectionTypeDef).Name),
			collectionOccurrence: occurrence,
		}
		if batchObject := objectTypeDef(collection.BatchType); batchObject != nil {
			itemRow.collectionBatchType = batchObject.Name
		}
		artifacts.rows = append(artifacts.rows, itemRow)

		if err := artifacts.discoverCollectionRows(
			ctx,
			srv,
			itemRow,
			itemObject,
			itemPath,
			itemCoordinates,
			nil,
		); err != nil {
			return err
		}
	}
	return nil
}

type artifactTraversalChild struct {
	field   string
	typeDef *TypeDef
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
				field:   field.Name,
				typeDef: typeDef,
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
			return full.Clone()
		}
	}
	return typeDef
}

func fullObjectTypeDef(typeDef *TypeDef, full map[string]*TypeDef) *TypeDef {
	if object := objectTypeDef(typeDef); object != nil {
		if fullTypeDef, ok := full[object.Name]; ok {
			return fullTypeDef
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
