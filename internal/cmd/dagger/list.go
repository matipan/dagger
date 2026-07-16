package daggercmd

import (
	"context"
	"fmt"
	"slices"
	"sort"

	"dagger.io/dagger"
	"github.com/dagger/dagger/engine/client"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

const artifactTypeDimension = "type"

type artifactListDimension struct {
	Name string
}

var listNeedsHelp bool

func init() {
	moduleAddFlags(listCmd, listCmd.PersistentFlags(), true)
}

var listCmd = &cobra.Command{
	Use:   "list <dimension> [flags]",
	Short: "List artifacts by dimension",

	// Dimensions and their filter flags come from the Artifacts API, so Cobra
	// cannot parse or render help until the workspace schema is loaded.
	DisableFlagParsing:    true,
	DisableFlagsInUseLine: true,

	PreRunE: func(cmd *cobra.Command, args []string) error {
		cmd.DisableFlagParsing = false
		listNeedsHelp = slices.Contains(args, "--help") || slices.Contains(args, "-h")

		cmd.Flags().SetInterspersed(false)
		cmd.FParseErrWhitelist.UnknownFlags = true
		if err := cmd.ParseFlags(args); err != nil {
			return cmd.FlagErrorFunc()(cmd, err)
		}
		cmd.FParseErrWhitelist.UnknownFlags = false
		return nil
	},

	RunE: func(cmd *cobra.Command, args []string) error {
		params := initModuleParams(args)
		return withEngine(cmd.Context(), params, func(ctx context.Context, engineClient *client.Client) error {
			dag := engineClient.Dagger()
			artifacts := dag.CurrentWorkspace().Artifacts()

			dimensions, err := loadArtifactListDimensions(ctx, artifacts)
			if err != nil {
				return err
			}
			if listNeedsHelp {
				target := ""
				helpArgs := stripHelpArgs(cmd.Flags().Args())
				if len(helpArgs) > 0 {
					target, _, err = parseArtifactListArgs(helpArgs, dimensions)
					if err != nil {
						return err
					}
				}
				printArtifactListHelp(cmd, dimensions, target)
				return nil
			}

			target, filters, err := parseArtifactListArgs(cmd.Flags().Args(), dimensions)
			if err != nil {
				return err
			}
			return printArtifactDimensionValues(ctx, cmd, artifacts, target, filters)
		})
	},
}

func loadArtifactListDimensions(
	ctx context.Context,
	artifacts *dagger.Artifacts,
) ([]artifactListDimension, error) {
	apiDimensions, err := artifacts.Dimensions(ctx)
	if err != nil {
		return nil, err
	}

	dimensions := make([]artifactListDimension, 0, len(apiDimensions))
	for i := range apiDimensions {
		name, err := apiDimensions[i].Name(ctx)
		if err != nil {
			return nil, err
		}
		dimensions = append(dimensions, artifactListDimension{Name: name})
	}
	return dimensions, nil
}

func parseArtifactListArgs(
	args []string,
	dimensions []artifactListDimension,
) (string, map[string][]string, error) {
	flags := pflag.NewFlagSet("list", pflag.ContinueOnError)
	flags.SetInterspersed(true)
	flags.SetOutput(stderr)

	filterValues := make(map[string]*[]string, len(dimensions))
	for _, dimension := range dimensions {
		values := new([]string)
		filterValues[dimension.Name] = values
		flags.StringArrayVar(
			values,
			dimension.Name,
			nil,
			fmt.Sprintf("Filter by artifact dimension %q", dimension.Name),
		)
	}
	if err := flags.Parse(stripHelpArgs(args)); err != nil {
		return "", nil, err
	}

	positionals := flags.Args()
	if len(positionals) != 1 {
		return "", nil, fmt.Errorf("accepts 1 arg(s), received %d", len(positionals))
	}

	target := positionals[0]
	if target == "types" {
		target = artifactTypeDimension
	}
	if !slices.ContainsFunc(dimensions, func(dimension artifactListDimension) bool {
		return dimension.Name == target
	}) {
		return "", nil, fmt.Errorf("artifact dimension %q not found", positionals[0])
	}

	filters := make(map[string][]string, len(filterValues))
	for dimension, values := range filterValues {
		filters[dimension] = *values
	}
	return target, filters, nil
}

func printArtifactDimensionValues(
	ctx context.Context,
	cmd *cobra.Command,
	artifacts *dagger.Artifacts,
	target string,
	filters map[string][]string,
) error {
	filterDimensions := make([]string, 0, len(filters))
	for dimension := range filters {
		filterDimensions = append(filterDimensions, dimension)
	}
	sort.Strings(filterDimensions)
	for _, dimension := range filterDimensions {
		values := filters[dimension]
		if len(values) > 0 {
			artifacts = artifacts.FilterCoordinates(dimension, values)
		}
	}
	artifacts = artifacts.FilterDimension(target)

	items, err := artifacts.Items(ctx)
	if err != nil {
		return err
	}

	seen := map[string]struct{}{}
	for i := range items {
		value, err := items[i].Coordinate(ctx, target)
		if err != nil {
			return err
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		fmt.Fprintln(cmd.OutOrStdout(), value)
	}
	return nil
}

func printArtifactListHelp(
	cmd *cobra.Command,
	dimensions []artifactListDimension,
	target string,
) {
	out := cmd.OutOrStdout()
	if target != "" {
		if target == artifactTypeDimension {
			target = "types"
		}
		fmt.Fprintf(out, "Usage:\n  %s list %s [flags]\n", cmd.Root().Name(), target)
		if len(dimensions) > 0 {
			fmt.Fprintln(out, "\nFlags:")
			for _, dimension := range dimensions {
				fmt.Fprintf(out, "  --%s stringArray\n", dimension.Name)
			}
		}
		return
	}

	fmt.Fprintf(out, "Usage:\n  %s list <dimension> [flags]\n\n", cmd.Root().Name())
	fmt.Fprintln(out, "Available dimensions:")
	for _, dimension := range dimensions {
		name := dimension.Name
		description := fmt.Sprintf("List values for the artifact dimension %q", dimension.Name)
		if dimension.Name == artifactTypeDimension {
			name = "types"
			description = "List available artifact types"
		}
		fmt.Fprintf(out, "  %-9s %s\n", name, description)
	}
}
