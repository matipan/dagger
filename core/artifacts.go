package core

import (
	"fmt"
	"sort"

	"github.com/dagger/dagger/dagql"
	"github.com/iancoleman/strcase"
	"github.com/vektah/gqlparser/v2/ast"
)

const ArtifactTypeDimension = "type"

// Artifacts is an immutable, filterable view over workspace artifacts.
type Artifacts struct {
	dimensions []*ArtifactDimension
	rows       [][]dagql.Nullable[dagql.String]
}

// ArtifactDimension describes one coordinate axis in an Artifacts scope.
type ArtifactDimension struct {
	Name    string
	KeyType *TypeDef
}

// Artifact is one coordinate row in an Artifacts scope.
type Artifact struct {
	coordinates []dagql.Nullable[dagql.String]
	scope       *Artifacts
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

// NewArtifactsFromTypeDefs discovers artifact roots from module-owned object
// fields on Query. Callers should pass type definitions from the canonical
// schema so entrypoint proxies are not interpreted as roots.
func NewArtifactsFromTypeDefs(typeDefs dagql.ObjectResultArray[*TypeDef]) *Artifacts {
	typeNames := map[string]struct{}{}
	for _, typeDefResult := range typeDefs {
		typeDef := typeDefResult.Self()
		if typeDef == nil || !typeDef.AsObject.Valid || typeDef.AsObject.Value.Self() == nil {
			continue
		}
		obj := typeDef.AsObject.Value.Self()
		if obj.Name != "Query" {
			continue
		}
		for _, fnResult := range obj.Functions {
			fn := fnResult.Self()
			if fn == nil || fn.SourceModuleName == "" || fn.ReturnType.Self() == nil {
				continue
			}
			returnType := fn.ReturnType.Self()
			if !returnType.AsObject.Valid || returnType.AsObject.Value.Self() == nil {
				continue
			}
			typeNames[returnType.AsObject.Value.Self().Name] = struct{}{}
		}
	}

	sortedTypeNames := make([]string, 0, len(typeNames))
	for typeName := range typeNames {
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
		rows: make([][]dagql.Nullable[dagql.String], 0, len(sortedTypeNames)),
	}
	for _, typeName := range sortedTypeNames {
		artifacts.rows = append(artifacts.rows, []dagql.Nullable[dagql.String]{
			dagql.NonNull(dagql.NewString(strcase.ToKebab(typeName))),
		})
	}
	return artifacts
}

func (artifacts *Artifacts) Clone() *Artifacts {
	cp := &Artifacts{
		dimensions: make([]*ArtifactDimension, len(artifacts.dimensions)),
		rows:       make([][]dagql.Nullable[dagql.String], len(artifacts.rows)),
	}
	for i, dimension := range artifacts.dimensions {
		cp.dimensions[i] = dimension.Clone()
	}
	for i, row := range artifacts.rows {
		cp.rows[i] = append([]dagql.Nullable[dagql.String](nil), row...)
	}
	return cp
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
			coordinates: append([]dagql.Nullable[dagql.String](nil), row...),
			scope:       artifacts,
		}
	}
	return items
}

func (artifacts *Artifacts) FilterDimension(dimension string) (*Artifacts, error) {
	index, err := artifacts.dimensionIndex(dimension)
	if err != nil {
		return nil, err
	}

	filtered := artifacts.Clone()
	filtered.rows = filtered.rows[:0]
	for _, row := range artifacts.rows {
		if row[index].Valid {
			filtered.rows = append(filtered.rows, append([]dagql.Nullable[dagql.String](nil), row...))
		}
	}
	return filtered, nil
}

func (artifacts *Artifacts) FilterCoordinates(dimension string, values []string) (*Artifacts, error) {
	index, err := artifacts.dimensionIndex(dimension)
	if err != nil {
		return nil, err
	}
	if len(values) == 0 {
		return nil, fmt.Errorf("values must not be empty")
	}

	accepted := make(map[string]struct{}, len(values))
	for _, value := range values {
		accepted[value] = struct{}{}
	}

	filtered := artifacts.Clone()
	filtered.rows = filtered.rows[:0]
	for _, row := range artifacts.rows {
		if !row[index].Valid {
			continue
		}
		if _, ok := accepted[row[index].Value.String()]; ok {
			filtered.rows = append(filtered.rows, append([]dagql.Nullable[dagql.String](nil), row...))
		}
	}
	return filtered, nil
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
