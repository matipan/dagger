package daggercmd

import (
	"context"

	"github.com/spf13/cobra"

	"dagger.io/dagger"
	"github.com/dagger/dagger/dagql/dagui"
	"github.com/dagger/dagger/engine/client"
	"github.com/dagger/dagger/engine/slog"
	telemetry "github.com/dagger/otel-go"
)

var (
	upListMode bool
	upPlanMode bool
)

func init() {
	upCmd.Flags().BoolVarP(&upListMode, "list", "l", false, "List available services")
	upCmd.Flags().BoolVar(&upPlanMode, "plan", false, "Display the compiled execution plan")
}

var upCmd = &cobra.Command{
	Use:                "up [options] [pattern...]",
	Short:              "Run your project's services for local development - databases, APIs, dev servers, etc.",
	DisableFlagParsing: true,
	Long: `Run your project's services for local development - databases, APIs, dev servers, etc.

Examples:
  dagger up                       # Start all services
  dagger up -l                    # List all available services
  dagger up app:web               # Start only the 'web' service
  dagger up --type=app app:web    # Filter artifacts, then start 'web'
`,
	RunE: func(cmd *cobra.Command, args []string) error {
		artifactArgs, needsHelp, err := prepareExecutionPlanCommand(cmd, args)
		if err != nil {
			return err
		}

		params := initModuleParams(args)
		params.WorkspaceModuleScope = executionPlanModuleScope(artifactArgs)
		return withEngine(
			cmd.Context(),
			params,
			func(ctx context.Context, engineClient *client.Client) error {
				dag := engineClient.Dagger()
				artifacts := dag.CurrentWorkspace().Artifacts()
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
					dagger.VerbUp,
					dagger.ArtifactsPlanOpts{Include: include},
				)
				if upListMode || upPlanMode {
					return printExecutionPlan(ctx, cmd, dag, plan, dimensions, upPlanMode)
				}
				return runUpPlan(ctx, plan)
			},
		)
	},
}

func runUpPlan(ctx context.Context, plan *dagger.Plan) error {
	ctx, zoomSpan := Tracer().Start(ctx, "services", telemetry.Passthrough())
	defer zoomSpan.End()
	Frontend.SetPrimary(dagui.SpanID{SpanID: zoomSpan.SpanContext().SpanID()})
	slog.SetDefault(slog.SpanLogger(ctx, InstrumentationLibrary))

	err := plan.Run(ctx)
	if ctx.Err() != nil {
		return nil
	}
	return err
}
