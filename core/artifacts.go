package core

import (
	"fmt"
	"slices"
	"sort"

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
	coordinates      []dagql.Nullable[dagql.String]
	rootField        string
	rootType         string
	sourceModuleName string
}

// ArtifactDimension describes one coordinate axis in an Artifacts scope.
type ArtifactDimension struct {
	Name    string
	KeyType *TypeDef
}

// Artifact is one coordinate row in an Artifacts scope.
type Artifact struct {
	coordinates      []dagql.Nullable[dagql.String]
	scope            *Artifacts
	rootField        string
	rootType         string
	sourceModuleName string
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
		rootTypes:   map[string]struct{}{},
		workspace:   workspace,
		workspaceID: workspaceID,
	}
}

// NewArtifactsFromTypeDefs discovers artifact roots from module-owned object
// fields on Query. Callers should pass type definitions from the canonical
// schema so entrypoint proxies are not interpreted as roots.
func NewArtifactsFromTypeDefs(typeDefs dagql.ObjectResultArray[*TypeDef]) *Artifacts {
	objects := map[string]*ObjectTypeDef{}
	for _, typeDefResult := range typeDefs {
		typeDef := typeDefResult.Self()
		if typeDef == nil || !typeDef.AsObject.Valid || typeDef.AsObject.Value.Self() == nil {
			continue
		}
		objectTypeDef := typeDef.AsObject.Value.Self()
		objects[objectTypeDef.Name] = objectTypeDef.Clone()
	}

	roots := map[string]*artifactRow{}
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
			roots[typeName] = &artifactRow{
				rootField:        fn.Name,
				rootType:         typeName,
				sourceModuleName: fn.SourceModuleName,
			}
		}
	}

	sortedTypeNames := make([]string, 0, len(roots))
	for typeName := range roots {
		sortedTypeNames = append(sortedTypeNames, typeName)
	}
	sort.Strings(sortedTypeNames)

	artifacts := &Artifacts{
		dimensions: []*ArtifactDimension{
			{
				Name: ArtifactTypeDimension,
				KeyType: &TypeDef{
					Kind: TypeDefKindString,
				},
			},
		},
		rows:      make([]*artifactRow, 0, len(sortedTypeNames)),
		objects:   objects,
		rootTypes: make(map[string]struct{}, len(sortedTypeNames)),
	}
	for _, typeName := range sortedTypeNames {
		row := roots[typeName]
		row.coordinates = []dagql.Nullable[dagql.String]{
			dagql.NonNull(dagql.NewString(strcase.ToKebab(typeName))),
		}
		artifacts.rows = append(artifacts.rows, row)
		artifacts.rootTypes[typeName] = struct{}{}
	}
	return artifacts
}

func (artifacts *Artifacts) Clone() *Artifacts {
	cp := &Artifacts{
		dimensions:   make([]*ArtifactDimension, len(artifacts.dimensions)),
		rows:         make([]*artifactRow, len(artifacts.rows)),
		objects:      artifacts.objects,
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
	return &cp
}

func (dimension *ArtifactDimension) Clone() *ArtifactDimension {
	cp := *dimension
	cp.KeyType = dimension.KeyType.Clone()
	return &cp
}

func (artifact *Artifact) Clone() *Artifact {
	cp := *artifact
	cp.coordinates = append([]dagql.Nullable[dagql.String](nil), artifact.coordinates...)
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
			coordinates:      append([]dagql.Nullable[dagql.String](nil), row.coordinates...),
			scope:            artifacts,
			rootField:        row.rootField,
			rootType:         row.rootType,
			sourceModuleName: row.sourceModuleName,
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
		coordinates:      append([]dagql.Nullable[dagql.String](nil), artifact.coordinates...),
		rootField:        artifact.rootField,
		rootType:         artifact.rootType,
		sourceModuleName: artifact.sourceModuleName,
	}}
	return target
}
