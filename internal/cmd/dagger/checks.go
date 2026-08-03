package daggercmd

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"dagger.io/dagger"
	"github.com/dagger/dagger/dagql/dagui"
	"github.com/dagger/dagger/dagql/idtui"
	"github.com/dagger/dagger/engine/client"
	"github.com/dagger/dagger/engine/slog"
	telemetry "github.com/dagger/otel-go"
)

var (
	checksListMode     bool
	checksPlanMode     bool
	checksFailFast     bool
	checksNoGenerate   bool
	checksOnlyGenerate bool
	checksSkip         []string
)

func init() {
	checksCmd.Flags().BoolVarP(&checksListMode, "list", "l", false, "List available checks")
	checksCmd.Flags().BoolVar(&checksPlanMode, "plan", false, "Display the compiled execution plan")
	checksCmd.Flags().BoolVar(&checksFailFast, "failfast", false, "Cancel remaining checks on first failure")
	checksCmd.Flags().BoolVar(&checksNoGenerate, "no-generate", false, "Only run annotated check functions, skip generate-as-checks")
	checksCmd.Flags().BoolVar(&checksOnlyGenerate, "generate", false, "Only run generate-as-checks, skip annotated check functions")
	checksCmd.Flags().StringArrayVar(&checksSkip, "skip", nil, "Skip checks matching the specified patterns")
	checksCmd.MarkFlagsMutuallyExclusive("no-generate", "generate")
}

var checksCmd = &cobra.Command{
	Aliases:            []string{"checks"},
	Use:                "check [options] [pattern...]",
	Short:              "Check the state of your project by running tests, linters, etc.",
	DisableFlagParsing: true,
	Long: `Check the state of your project by running tests, linters, etc.

Examples:
  dagger check                    # Run all checks
  dagger check -l                 # List all available checks
  dagger check go:lint            # Run the go:lint check
  dagger check --type=go go:lint  # Filter artifacts, then run go:lint
`,
	RunE: func(cmd *cobra.Command, args []string) error {
		artifactArgs, needsHelp, err := prepareExecutionPlanCommand(cmd, args)
		if err != nil {
			return err
		}

		params := initModuleParams(args)
		params.EnableCloudScaleOut = enableScaleOut
		params.WorkspaceModuleScope = executionPlanModuleScope(artifactArgs)
		targets := executionPlanTargetPatterns(artifactArgs)
		return withEngine(
			cmd.Context(),
			params,
			func(ctx context.Context, engineClient *client.Client) error {
				dag := engineClient.Dagger()
				artifacts := dag.CurrentWorkspace().Artifacts()
				materialized := artifacts.Materialize(
					dagger.ArtifactsMaterializeOpts{
						Include:    targets,
						BestEffort: needsHelp,
					},
				)
				materializedID, err := materialized.ID(ctx)
				if err != nil {
					return err
				}
				artifacts = dagger.Ref[*dagger.Artifacts](dag, materializedID)
				dimensions, err := loadArtifactListDimensions(ctx, artifacts)
				if err != nil {
					return err
				}
				if needsHelp {
					return printExecutionPlanHelp(cmd, dimensions)
				}

				artifacts, include, err := parseExecutionPlanArgs(
					artifactArgs,
					artifacts,
					dimensions,
				)
				if err != nil {
					return err
				}
				plan := artifacts.Plan(
					dagger.VerbCheck,
					dagger.ArtifactsPlanOpts{
						Include: include,
						Exclude: targetPatterns(checksSkip),
					},
				)
				if checksListMode {
					return printExecutionPlanRecipes(ctx, cmd, dag, plan, dimensions)
				}
				if checksPlanMode {
					return printExecutionPlan(ctx, cmd, dag, plan, dimensions, true)
				}
				return runCheckPlan(ctx, plan, include)
			},
		)
	},
}

func targetPatterns(patterns []string) []dagger.TargetPattern {
	result := make([]dagger.TargetPattern, len(patterns))
	for i, pattern := range patterns {
		result[i] = dagger.TargetPattern(pattern)
	}
	return result
}

func runCheckPlan(
	ctx context.Context,
	plan *dagger.Plan,
	include []dagger.TargetPattern,
) error {
	ctx, zoomSpan := Tracer().Start(ctx, "checks", telemetry.Passthrough())
	defer zoomSpan.End()
	Frontend.SetPrimary(dagui.SpanID{SpanID: zoomSpan.SpanContext().SpanID()})
	slog.SetDefault(slog.SpanLogger(ctx, InstrumentationLibrary))

	nodes, err := plan.Nodes(ctx)
	if err != nil {
		return err
	}
	if err := validateCheckSelection(include, len(nodes)); err != nil {
		return err
	}
	if err := plan.Run(ctx, dagger.PlanRunOpts{FailFast: checksFailFast}); err != nil {
		return idtui.ExitError{OriginalCode: 1, Original: err}
	}
	return nil
}

func validateCheckSelection(include []dagger.TargetPattern, selected int) error {
	if len(include) == 0 || selected > 0 {
		return nil
	}
	if len(include) == 1 {
		return fmt.Errorf("no checks matched pattern %q", include[0])
	}
	return fmt.Errorf("no checks matched patterns %q", include)
}
