package schema

import (
	"context"
	"fmt"

	"github.com/dagger/dagger/core"
	"github.com/dagger/dagger/dagql"
	"github.com/dagger/dagger/dagql/call"
)

type artifactsSchema struct{}

var _ SchemaResolvers = &artifactsSchema{}

func (s *artifactsSchema) Install(srv *dagql.Server) {
	dagql.Fields[*core.Workspace]{
		dagql.Func("artifacts", s.artifacts).
			Doc("A filterable view of all artifacts in this workspace."),
	}.Install(srv)

	dagql.Fields[*core.Artifacts]{
		dagql.Func("filterDimension", s.filterDimension).
			Doc("Keep artifacts with a non-null coordinate for the given dimension.").
			Args(
				dagql.Arg("dimension").Doc("The dimension to filter by."),
			),
		dagql.Func("filterCoordinates", s.filterCoordinates).
			Doc("Keep artifacts whose coordinate matches one of the given values.").
			Args(
				dagql.Arg("dimension").Doc("The dimension to filter by."),
				dagql.Arg("values").Doc("The accepted coordinate values."),
			),
		dagql.Func("dimensions", s.dimensions).
			Doc("Ordered filterable dimensions for this scope."),
		dagql.Func("items", s.items).
			Doc("Artifacts matching the current filters."),
	}.Install(srv)

	dagql.Fields[*core.ArtifactDimension]{
		dagql.Func("name", s.dimensionName).
			Doc("The dimension name used by filters and CLI flags."),
		dagql.Func("keyType", s.dimensionKeyType).
			Doc("The type used to parse and validate dimension keys."),
	}.Install(srv)

	dagql.Fields[*core.Artifact]{
		dagql.Func("coordinates", s.coordinates).
			Doc("Ordered coordinate row for this artifact."),
		dagql.Func("coordinate", s.coordinate).
			Doc("Look up this artifact's coordinate for one dimension.").
			Args(
				dagql.Arg("name").Doc("The dimension name."),
			),
		dagql.Func("scope", s.scope).
			Doc("The Artifacts scope that produced this row."),
	}.Install(srv)
}

func (s *artifactsSchema) artifacts(
	ctx context.Context,
	workspace *core.Workspace,
	_ struct{},
) (*core.Artifacts, error) {
	currentCall := dagql.CurrentCall(ctx)
	if currentCall == nil {
		return nil, fmt.Errorf("artifacts: current call not found")
	}
	if currentCall.Receiver == nil || currentCall.Receiver.ResultID == 0 {
		return nil, fmt.Errorf("artifacts: attached workspace receiver not found")
	}
	workspaceID := call.NewEngineResultID(
		currentCall.Receiver.ResultID,
		call.NewType(workspace.Type()),
	)
	return core.NewWorkspaceArtifacts(workspace, workspaceID), nil
}

func materializeArtifacts(
	ctx context.Context,
	artifacts *core.Artifacts,
	include []core.FunctionPattern,
	bestEffort bool,
) (*core.Artifacts, []string, error) {
	if artifacts.IsMaterialized() {
		return artifacts, nil, nil
	}
	workspace := artifacts.Workspace()
	if workspace == nil {
		return artifacts, nil, nil
	}
	if isSyntheticWorkspace(workspace) {
		materialized, err := artifacts.Materialize(core.NewArtifactsFromTypeDefs(nil))
		return materialized, nil, err
	}

	workspaceCtx, err := withWorkspaceClientContext(ctx, workspace)
	if err != nil {
		return nil, nil, err
	}
	loadFailures, err := ensureWorkspaceModulesLoaded(
		workspaceCtx,
		artifacts.WorkspaceModuleSelectors(include),
		bestEffort,
	)
	if err != nil {
		return nil, nil, err
	}

	snapshot, err := workspaceArtifactsSnapshot(workspaceCtx)
	if err != nil {
		return nil, nil, err
	}
	materialized, err := artifacts.Materialize(snapshot)
	if err != nil {
		return nil, nil, err
	}
	return materialized, loadFailures, nil
}

func workspaceArtifactsSnapshot(ctx context.Context) (*core.Artifacts, error) {
	query, err := core.CurrentQuery(ctx)
	if err != nil {
		return nil, err
	}
	served, err := query.Server.CurrentServedDeps(ctx)
	if err != nil {
		return nil, fmt.Errorf("get workspace modules: %w", err)
	}
	dag, err := served.Schema(ctx)
	if err != nil {
		return nil, fmt.Errorf("get workspace schema: %w", err)
	}
	// Entrypoint schemas replace the module constructor with flattened proxy
	// fields. Artifact discovery needs the canonical module root instead.
	dag = dag.Canonical()
	typeDefs, err := buildTypeDefsFromSchema(ctx, dag)
	if err != nil {
		return nil, fmt.Errorf("introspect workspace schema: %w", err)
	}
	return core.NewArtifactsFromTypeDefs(typeDefs), nil
}

func (s *artifactsSchema) filterDimension(
	_ context.Context,
	artifacts *core.Artifacts,
	args struct {
		Dimension string
	},
) (*core.Artifacts, error) {
	return artifacts.FilterDimension(args.Dimension)
}

func (s *artifactsSchema) filterCoordinates(
	_ context.Context,
	artifacts *core.Artifacts,
	args struct {
		Dimension string
		Values    []string
	},
) (*core.Artifacts, error) {
	return artifacts.FilterCoordinates(args.Dimension, args.Values)
}

func (s *artifactsSchema) dimensions(
	ctx context.Context,
	artifacts *core.Artifacts,
	_ struct{},
) ([]*core.ArtifactDimension, error) {
	materialized, _, err := materializeArtifacts(ctx, artifacts, nil, false)
	if err != nil {
		return nil, err
	}
	return materialized.Dimensions(), nil
}

func (s *artifactsSchema) items(
	ctx context.Context,
	artifacts *core.Artifacts,
	_ struct{},
) ([]*core.Artifact, error) {
	materialized, _, err := materializeArtifacts(ctx, artifacts, nil, false)
	if err != nil {
		return nil, err
	}
	return materialized.Items(), nil
}

func (s *artifactsSchema) dimensionName(
	_ context.Context,
	dimension *core.ArtifactDimension,
	_ struct{},
) (string, error) {
	return dimension.Name, nil
}

func (s *artifactsSchema) dimensionKeyType(
	_ context.Context,
	dimension *core.ArtifactDimension,
	_ struct{},
) (*core.TypeDef, error) {
	return dimension.KeyType, nil
}

func (s *artifactsSchema) coordinates(
	_ context.Context,
	artifact *core.Artifact,
	_ struct{},
) ([]dagql.Nullable[dagql.String], error) {
	return artifact.Coordinates(), nil
}

func (s *artifactsSchema) coordinate(
	_ context.Context,
	artifact *core.Artifact,
	args struct {
		Name string
	},
) (dagql.Nullable[dagql.String], error) {
	return artifact.Coordinate(args.Name)
}

func (s *artifactsSchema) scope(
	_ context.Context,
	artifact *core.Artifact,
	_ struct{},
) (*core.Artifacts, error) {
	return artifact.Scope(), nil
}
