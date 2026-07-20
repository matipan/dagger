package daggercmd

import (
	"context"
	"errors"
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
	generateListMode    bool
	generatePlanMode    bool
	generateRequireLoad bool
	generateNoApply     bool
)

func init() {
	generateCmd.Flags().BoolVarP(&generateListMode, "list", "l", false, "List available generators")
	generateCmd.Flags().BoolVar(&generatePlanMode, "plan", false, "Display the compiled execution plan")
	generateCmd.Flags().BoolVar(&generateRequireLoad, "require-load", false, "Fail if any workspace module cannot be loaded (default: report as a warning and generate the rest)")
	generateCmd.Flags().BoolVar(&generateNoApply, "no-apply", false, "Compute and show a summary of generated changes without applying them")
}

var generateCmd = &cobra.Command{
	Use:                "generate [options] [pattern...]",
	Short:              "Generate assets of your project",
	DisableFlagParsing: true,
	Long: `Generate assets of your project

Examples:
  dagger generate                            # Generate all assets
  dagger generate -l                         # List all available generators
  dagger generate go:bin                     # Generate one selected asset
  dagger generate --type=go bin              # Filter artifacts, then generate bin
`,
	RunE: func(cmd *cobra.Command, args []string) error {
		artifactArgs, needsHelp, err := prepareExecutionPlanCommand(cmd, args)
		if err != nil {
			return err
		}
		disposition := changesetDispositionPrompt
		if !needsHelp {
			disposition, err = generateChangesetDisposition(
				generateListMode,
				autoApply,
				generateNoApply,
				idtui.RunningInAgent(),
			)
			if err != nil {
				return err
			}
		}

		params := initModuleParams(args)
		return withEngine(
			cmd.Context(),
			params,
			func(ctx context.Context, engineClient *client.Client) error {
				dag := engineClient.Dagger()
				artifacts := dag.CurrentWorkspace().Artifacts()
				if needsHelp {
					dimensions, err := loadArtifactListDimensions(ctx, artifacts)
					if err != nil {
						return err
					}
					return printExecutionPlanHelp(cmd, dimensions)
				}

				artifacts, include, err := parseExecutionPlanArgs(
					artifactArgs,
					artifacts,
				)
				if err != nil {
					return err
				}
				plan := artifacts.Plan(
					dagger.VerbGenerate,
					dagger.ArtifactsPlanOpts{Include: include},
				)
				if generateRequireLoad {
					loadFailures, err := plan.LoadFailures(ctx)
					if err != nil {
						return err
					}
					if len(loadFailures) > 0 {
						return fmt.Errorf(
							"%d workspace module(s) could not be loaded (--require-load)",
							len(loadFailures),
						)
					}
				}
				if generateListMode || generatePlanMode {
					return printExecutionPlan(ctx, cmd, plan, generatePlanMode)
				}
				return runGeneratePlan(ctx, dag, plan, cmd, disposition)
			},
		)
	},
}

func generateChangesetDisposition(
	list, apply, noApply, runningInAgent bool,
) (changesetDisposition, error) {
	if apply && noApply {
		return changesetDispositionPrompt, errors.New("--auto-apply and --no-apply cannot be used together")
	}
	if list {
		return changesetDispositionPrompt, nil
	}
	if apply {
		return changesetDispositionApply, nil
	}
	if noApply {
		return changesetDispositionNoApply, nil
	}
	if runningInAgent {
		return changesetDispositionPrompt, errors.New(`dagger generate requires an explicit changeset choice when run by a coding agent:
  pass -y/--auto-apply to apply generated changes
  pass --no-apply to show generated changes without applying them

For an up-to-date check that fails on pending changes, use dagger check --generate`)
	}
	return changesetDispositionPrompt, nil
}

func runGeneratePlan(
	ctx context.Context,
	dag *dagger.Client,
	plan *dagger.Plan,
	cmd *cobra.Command,
	disposition changesetDisposition,
) (rerr error) {
	ctx, zoomSpan := Tracer().Start(ctx, "generators", telemetry.Passthrough())
	defer zoomSpan.End()
	Frontend.SetPrimary(dagui.SpanID{SpanID: zoomSpan.SpanContext().SpanID()})
	slog.SetDefault(slog.SpanLogger(ctx, InstrumentationLibrary))

	changes, err := plan.Changes(dagger.PlanChangesOpts{
		OnConflict: dagger.ChangesetsMergeConflictFailEarly,
	}).Sync(ctx)
	if err != nil {
		return err
	}
	previewOut := cmd.ErrOrStderr()
	if disposition == changesetDispositionNoApply {
		previewStdio := telemetry.SpanStdio(ctx, InstrumentationLibrary)
		defer previewStdio.Close()
		previewOut = previewStdio.Stderr
	}
	err = handleChangesetResponseWithDisposition(ctx, dag, changes, disposition, previewOut)
	if errors.Is(err, idtui.ErrNonInteractive) {
		return fmt.Errorf(
			"%w; pass -y/--auto-apply to apply changes, or --no-apply to show them without applying",
			idtui.ErrNonInteractive,
		)
	}
	return err
}
