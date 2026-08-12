package schema

import (
	"context"
	"fmt"
	"strconv"

	"github.com/dagger/dagger/core"
	"github.com/dagger/dagger/core/modules"
	"github.com/dagger/dagger/core/workspace"
	"github.com/dagger/dagger/dagql"
)

type plansSchema struct{}

var _ SchemaResolvers = &plansSchema{}

func (s *plansSchema) Install(srv *dagql.Server) {
	core.Verbs.Install(srv)
	core.GeneratedChecksModes.Install(srv)
	srv.InstallScalar(core.TargetPattern(""))

	dagql.Fields[*core.Artifacts]{
		dagql.Func("plan", s.plan).
			Doc("Compile matching artifact actions into an execution plan.").
			Args(
				dagql.Arg("verb").Doc("The lifecycle operation to compile."),
				dagql.Arg("include").Doc("Only include matching artifact fields or lifecycle action paths."),
				dagql.Arg("exclude").Doc("Exclude matching artifact fields or lifecycle action paths."),
				dagql.Arg("generatedChecks").Doc("How generators participate when compiling a CHECK plan."),
			),
	}.Install(srv)

	dagql.Fields[*core.Artifact]{
		dagql.Func("actions", s.actions).
			Doc("Discover lifecycle actions rooted at this artifact.").
			Args(
				dagql.Arg("verbs").Doc("Only return actions for these lifecycle operations."),
			),
		dagql.Func("action", s.action).
			Doc("Select one exact lifecycle action on this artifact.").
			Args(
				dagql.Arg("verb").Doc("The lifecycle operation."),
				dagql.Arg("functionPath").Doc("The exact artifact-relative function path."),
			),
	}.Install(srv)

	dagql.Fields[*core.Action]{
		dagql.Func("verb", s.actionVerb).
			Doc("The lifecycle operation performed by this action."),
		dagql.Func("target", s.actionTarget).
			Doc("The artifact targeted by this action."),
		dagql.Func("functionPath", s.actionFunctionPath).
			Doc("The exact artifact-relative function path."),
		dagql.Func("collectionBatched", s.actionCollectionBatched).
			Doc("Whether this action was compiled for a collection as one batch."),
		dagql.Func("after", s.actionAfter).
			Doc("Actions that must complete before this action."),
		dagql.Func("withAfter", s.actionWithAfter).
			Doc("Return this action with additional dependencies.").
			Args(
				dagql.Arg("actions").Doc("Actions that must complete first."),
			),
		dagql.Func("run", s.actionRun).
			Doc("Execute this action and wait for completion."),
	}.Install(srv)

	dagql.Fields[*core.Plan]{
		dagql.Func("verb", s.planVerb).
			Doc("The lifecycle operation compiled by this plan."),
		dagql.Func("nodes", s.planNodes).
			Doc("The finite, deterministically ordered action DAG."),
		dagql.Func("loadFailures", s.planLoadFailures).
			Doc("Workspace module load failures tolerated while compiling this plan."),
		dagql.Func("services", s.planServices).
			Doc("Evaluate an UP plan and return its services in stable node order."),
		dagql.Func("service", s.planService).
			Doc("Evaluate an UP plan that contains exactly one service action."),
		dagql.NodeFunc("changes", s.planChanges).
			Doc("Merge the changes produced by a GENERATE plan.").
			Args(
				dagql.Arg("onConflict").Doc("Strategy to apply to conflicts between generated changes."),
			),
		dagql.Func("run", s.planRun).
			Doc("Execute this plan and wait for completion.").
			Args(
				dagql.Arg("failFast").Doc("Cancel remaining CHECK actions after the first failure."),
			),
	}.Install(srv)
}

type artifactsPlanArgs struct {
	Verb            core.Verb
	Include         dagql.ArrayInput[core.TargetPattern] `default:"[]"`
	Exclude         dagql.ArrayInput[core.TargetPattern] `default:"[]"`
	GeneratedChecks core.GeneratedChecksMode             `default:"AUTO"`
}

func (s *plansSchema) plan(
	ctx context.Context,
	artifacts *core.Artifacts,
	args artifactsPlanArgs,
) (*core.Plan, error) {
	include := []core.TargetPattern(args.Include)
	materialized, loadFailures, err := materializeArtifacts(
		ctx,
		artifacts,
		include,
		args.Verb == core.VerbGenerate,
	)
	if err != nil {
		return nil, err
	}
	sourceExcludes, err := planSourceExcludes(ctx, args.Verb)
	if err != nil {
		return nil, err
	}
	var plan *core.Plan
	if args.Verb == core.VerbCheck {
		var includeChecks, includeGenerators bool
		includeChecks, includeGenerators, err = generatedCheckSelection(ctx, args.GeneratedChecks)
		if err != nil {
			return nil, err
		}
		var generateSourceExcludes map[string][]core.TargetPattern
		generateSourceExcludes, err = planSourceExcludes(ctx, core.VerbGenerate)
		if err != nil {
			return nil, err
		}
		plan, err = materialized.CheckPlan(
			include,
			[]core.TargetPattern(args.Exclude),
			sourceExcludes,
			generateSourceExcludes,
			includeChecks,
			includeGenerators,
		)
	} else {
		plan, err = materialized.PlanWithSourceExcludes(
			args.Verb,
			include,
			[]core.TargetPattern(args.Exclude),
			sourceExcludes,
		)
	}
	if err != nil {
		return nil, err
	}
	plan = plan.WithLoadFailures(loadFailures)
	if args.Verb != core.VerbUp {
		return plan, nil
	}
	portMappings, err := planPortMappings(ctx)
	if err != nil {
		return nil, err
	}
	return plan.WithPortMappings(portMappings), nil
}

func generatedCheckSelection(
	ctx context.Context,
	mode core.GeneratedChecksMode,
) (includeChecks bool, includeGenerators bool, _ error) {
	if mode != core.GeneratedChecksAuto {
		return generatedCheckSelectionFromConfig(mode, nil)
	}
	query, err := core.CurrentQuery(ctx)
	if err != nil {
		return false, false, err
	}
	currentWorkspace, err := query.Server.CurrentWorkspace(ctx)
	if err != nil {
		return false, false, err
	}
	cfg, err := workspaceConfigWithCompatFallback(ctx, currentWorkspace)
	if err != nil {
		return false, false, err
	}
	return generatedCheckSelectionFromConfig(mode, cfg)
}

func generatedCheckSelectionFromConfig(
	mode core.GeneratedChecksMode,
	cfg *workspace.Config,
) (includeChecks bool, includeGenerators bool, _ error) {
	switch mode {
	case core.GeneratedChecksInclude:
		return true, true, nil
	case core.GeneratedChecksExclude:
		return true, false, nil
	case core.GeneratedChecksOnly:
		return false, true, nil
	case core.GeneratedChecksAuto:
		return true, cfg == nil || cfg.CheckGenerated == nil || *cfg.CheckGenerated, nil
	default:
		return false, false, fmt.Errorf("unknown generated checks mode %q", mode)
	}
}

func planSourceExcludes(
	ctx context.Context,
	verb core.Verb,
) (map[string][]core.TargetPattern, error) {
	var workspaceSkips func(workspace.ModuleEntry) []string
	switch verb {
	case core.VerbCheck:
		workspaceSkips = func(entry workspace.ModuleEntry) []string {
			return entry.Check.Skip
		}
	case core.VerbGenerate:
		workspaceSkips = func(entry workspace.ModuleEntry) []string {
			return entry.Generate.Skip
		}
	case core.VerbUp:
		workspaceSkips = func(entry workspace.ModuleEntry) []string {
			return entry.Up.Skip
		}
	default:
		return nil, nil
	}

	query, err := core.CurrentQuery(ctx)
	if err != nil {
		return nil, err
	}
	currentWorkspace, err := query.Server.CurrentWorkspace(ctx)
	if err != nil {
		return nil, err
	}
	patterns, err := workspaceConfigSkipPatterns(
		ctx,
		currentWorkspace,
		workspaceSkips,
	)
	if err != nil {
		return nil, err
	}
	if verb == core.VerbUp {
		return decodePlanSourceExcludes(verb, patterns)
	}

	mods, err := currentWorkspacePrimaryModules(ctx)
	if err != nil {
		return nil, err
	}
	toolchainSkips := toolchainIgnorePatterns(mods, func(cfg *modules.ModuleConfigDependency) []string {
		switch verb {
		case core.VerbCheck:
			return cfg.IgnoreChecks
		case core.VerbGenerate:
			return cfg.IgnoreGenerators
		default:
			return nil
		}
	})
	for source, sourcePatterns := range toolchainSkips {
		patterns[source] = append(patterns[source], sourcePatterns...)
	}

	return decodePlanSourceExcludes(verb, patterns)
}

func toolchainIgnorePatterns(
	mods []dagql.ObjectResult[*core.Module],
	getPatterns func(*modules.ModuleConfigDependency) []string,
) map[string][]string {
	result := make(map[string][]string)
	for _, modResult := range mods {
		mod := modResult.Self()
		if mod == nil || !mod.Source.Valid || mod.Source.Value.Self() == nil {
			continue
		}
		for _, cfg := range mod.Source.Value.Self().ConfigToolchains {
			if patterns := getPatterns(cfg); len(patterns) > 0 {
				result[cfg.Name] = patterns
			}
		}
	}
	return result
}

func decodePlanSourceExcludes(
	verb core.Verb,
	patterns map[string][]string,
) (map[string][]core.TargetPattern, error) {
	excludes := make(map[string][]core.TargetPattern, len(patterns))
	for source, sourcePatterns := range patterns {
		excludes[source] = make([]core.TargetPattern, len(sourcePatterns))
		for i, pattern := range sourcePatterns {
			decoded, err := (core.TargetPattern("")).DecodeInput(pattern)
			if err != nil {
				return nil, fmt.Errorf(
					"invalid ignored %s pattern %q for module %q: %w",
					verb,
					pattern,
					source,
					err,
				)
			}
			excludes[source][i] = decoded.(core.TargetPattern)
		}
	}
	return excludes, nil
}

func planPortMappings(ctx context.Context) (map[string][]core.PortForward, error) {
	query, err := core.CurrentQuery(ctx)
	if err != nil {
		return nil, err
	}
	currentWorkspace, err := query.Server.CurrentWorkspace(ctx)
	if err != nil {
		return nil, err
	}
	cfg, err := workspaceConfigWithCompatFallback(ctx, currentWorkspace)
	if err != nil {
		return nil, err
	}

	mappings := make(map[string][]core.PortForward)
	for hostValue, mapping := range cfg.Ports {
		host, err := strconv.Atoi(hostValue)
		if err != nil {
			return nil, fmt.Errorf("workspace port key %q: %w", hostValue, err)
		}
		mappings[mapping.BackendService] = append(
			mappings[mapping.BackendService],
			core.PortForward{
				Frontend: &host,
				Backend:  mapping.BackendPort,
				Protocol: core.NetworkProtocolTCP,
			},
		)
	}
	return mappings, nil
}

type artifactActionsArgs struct {
	Verbs dagql.ArrayInput[core.Verb] `default:"[]"`
}

func (s *plansSchema) actions(
	_ context.Context,
	artifact *core.Artifact,
	args artifactActionsArgs,
) ([]*core.Action, error) {
	return artifact.Actions([]core.Verb(args.Verbs))
}

type artifactActionArgs struct {
	Verb         core.Verb
	FunctionPath []string
}

func (s *plansSchema) action(
	_ context.Context,
	artifact *core.Artifact,
	args artifactActionArgs,
) (*core.Action, error) {
	return artifact.Action(args.Verb, args.FunctionPath)
}

func (s *plansSchema) actionVerb(
	_ context.Context,
	action *core.Action,
	_ struct{},
) (core.Verb, error) {
	return action.Verb(), nil
}

func (s *plansSchema) actionTarget(
	_ context.Context,
	action *core.Action,
	_ struct{},
) (*core.Artifacts, error) {
	return action.Target(), nil
}

func (s *plansSchema) actionFunctionPath(
	_ context.Context,
	action *core.Action,
	_ struct{},
) ([]string, error) {
	return action.FunctionPath(), nil
}

func (s *plansSchema) actionCollectionBatched(
	_ context.Context,
	action *core.Action,
	_ struct{},
) (bool, error) {
	return action.CollectionBatched(), nil
}

func (s *plansSchema) actionAfter(
	_ context.Context,
	action *core.Action,
	_ struct{},
) ([]dagql.ID[*core.Action], error) {
	return action.After(), nil
}

type actionWithAfterArgs struct {
	Actions dagql.ArrayInput[dagql.ID[*core.Action]]
}

func (s *plansSchema) actionWithAfter(
	_ context.Context,
	action *core.Action,
	args actionWithAfterArgs,
) (*core.Action, error) {
	return action.WithAfter([]dagql.ID[*core.Action](args.Actions)), nil
}

func (s *plansSchema) actionRun(
	ctx context.Context,
	action *core.Action,
	_ struct{},
) (dagql.Nullable[core.Void], error) {
	return dagql.Null[core.Void](), action.Run(ctx)
}

func (s *plansSchema) planVerb(
	_ context.Context,
	plan *core.Plan,
	_ struct{},
) (core.Verb, error) {
	return plan.Verb(), nil
}

func (s *plansSchema) planNodes(
	_ context.Context,
	plan *core.Plan,
	_ struct{},
) ([]*core.Action, error) {
	return plan.Nodes(), nil
}

func (s *plansSchema) planLoadFailures(
	_ context.Context,
	plan *core.Plan,
	_ struct{},
) ([]string, error) {
	return plan.LoadFailures(), nil
}

func (s *plansSchema) planServices(
	ctx context.Context,
	plan *core.Plan,
	_ struct{},
) (dagql.ObjectResultArray[*core.Service], error) {
	return plan.Services(ctx)
}

func (s *plansSchema) planService(
	ctx context.Context,
	plan *core.Plan,
	_ struct{},
) (dagql.ObjectResult[*core.Service], error) {
	return plan.Service(ctx)
}

type planChangesArgs struct {
	OnConflict ChangesetsMergeConflict `default:"FAIL_EARLY"`
}

func (s *plansSchema) planChanges(
	ctx context.Context,
	plan dagql.ObjectResult[*core.Plan],
	args planChangesArgs,
) (*core.Changeset, error) {
	return plan.Self().Changes(ctx, mergeConflictsStrategyToCore(args.OnConflict))
}

func (s *plansSchema) planRun(
	ctx context.Context,
	plan *core.Plan,
	args struct {
		FailFast bool `default:"false"`
	},
) (dagql.Nullable[core.Void], error) {
	return dagql.Null[core.Void](), plan.Run(ctx, args.FailFast)
}
