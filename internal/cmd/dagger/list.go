package daggercmd

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"dagger.io/dagger"
	"github.com/dagger/dagger/engine/client"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

const artifactTypeDimension = "type"

type artifactListDimension struct {
	Name    string
	Aliases []string
}

type artifactFilterSelection struct {
	coordinates map[string][]string
	presence    map[string]bool
}

type artifactFilterFlagValues struct {
	coordinates     map[string]*[]string
	presence        map[string]*bool
	aliasDimensions map[string]string
}

var listNeedsHelp bool
var listArtifactArgs []string
var listCheckScope bool
var listGenerateScope bool

func init() {
	moduleAddFlags(listCmd, listCmd.PersistentFlags(), true)
	listCmd.Flags().BoolVar(&listCheckScope, "check", false, "List values in check scope")
	listCmd.Flags().BoolVar(&listGenerateScope, "generate", false, "List values in generate scope")
	listCmd.MarkFlagsMutuallyExclusive("check", "generate")
}

var listCmd = &cobra.Command{
	Use:   "list <dimension> [flags]",
	Short: "List artifacts by dimension",

	// Dimensions and their filter flags come from the Artifacts API, so Cobra
	// cannot parse or render help until the workspace schema is loaded.
	DisableFlagParsing:    true,
	DisableFlagsInUseLine: true,

	PreRunE: func(cmd *cobra.Command, args []string) error {
		var err error
		listArtifactArgs, listNeedsHelp, err = prepareExecutionPlanCommand(cmd, args)
		return err
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
				helpArgs := stripHelpArgs(listArtifactArgs)
				if len(helpArgs) > 0 {
					target, _, err = parseArtifactListArgs(helpArgs, dimensions)
					if err != nil {
						return err
					}
				}
				printArtifactListHelp(cmd, dimensions, target)
				return nil
			}

			target, filters, err := parseArtifactListArgs(listArtifactArgs, dimensions)
			if err != nil {
				return err
			}
			var verb dagger.Verb
			switch {
			case listCheckScope:
				verb = dagger.VerbCheck
			case listGenerateScope:
				verb = dagger.VerbGenerate
			}
			return printArtifactDimensionValues(ctx, cmd, artifacts, target, filters, verb)
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
		aliases, err := apiDimensions[i].CollectionTypes(ctx)
		if err != nil {
			return nil, err
		}
		dimensions = append(dimensions, artifactListDimension{
			Name:    name,
			Aliases: aliases,
		})
	}
	return dimensions, nil
}

func parseArtifactListArgs(
	args []string,
	dimensions []artifactListDimension,
) (string, artifactFilterSelection, error) {
	flags := pflag.NewFlagSet("list", pflag.ContinueOnError)
	flags.SetInterspersed(true)
	flags.SetOutput(stderr)

	filterValues, err := addArtifactFilterFlags(flags, dimensions)
	if err != nil {
		return "", artifactFilterSelection{}, err
	}
	if err := filterValues.parse(flags, stripHelpArgs(args)); err != nil {
		return "", artifactFilterSelection{}, err
	}

	positionals := flags.Args()
	if len(positionals) != 1 {
		return "", artifactFilterSelection{}, fmt.Errorf("accepts 1 arg(s), received %d", len(positionals))
	}

	target := positionals[0]
	if target == "types" {
		target = artifactTypeDimension
	}
	if !slices.ContainsFunc(dimensions, func(dimension artifactListDimension) bool {
		return dimension.Name == target
	}) {
		return "", artifactFilterSelection{}, fmt.Errorf("artifact dimension %q not found", positionals[0])
	}

	return target, filterValues.selection(dimensions), nil
}

func addArtifactFilterFlags(
	flags *pflag.FlagSet,
	dimensions []artifactListDimension,
) (*artifactFilterFlagValues, error) {
	filterNames := make(map[string]string, len(dimensions))
	for _, dimension := range dimensions {
		if existing, found := filterNames[dimension.Name]; found {
			return nil, fmt.Errorf(
				"artifact filter %q conflicts with filter %q",
				dimension.Name,
				existing,
			)
		}
		if existing := flags.Lookup(dimension.Name); existing != nil {
			return nil, fmt.Errorf(
				"artifact filter %q conflicts with filter %q",
				dimension.Name,
				existing.Name,
			)
		}
		filterNames[dimension.Name] = dimension.Name
	}
	for _, dimension := range dimensions {
		for _, alias := range dimension.Aliases {
			if existing, found := filterNames[alias]; found {
				return nil, fmt.Errorf(
					"artifact collection alias %q conflicts with filter %q",
					alias,
					existing,
				)
			}
			if existing := flags.Lookup(alias); existing != nil {
				return nil, fmt.Errorf(
					"artifact collection alias %q conflicts with filter %q",
					alias,
					existing.Name,
				)
			}
			filterNames[alias] = alias
		}
	}

	values := &artifactFilterFlagValues{
		coordinates:     make(map[string]*[]string, len(dimensions)),
		presence:        map[string]*bool{},
		aliasDimensions: map[string]string{},
	}
	for _, dimension := range dimensions {
		coordinates := new([]string)
		values.coordinates[dimension.Name] = coordinates
		flags.StringArrayVar(
			coordinates,
			dimension.Name,
			nil,
			fmt.Sprintf("Filter by artifact dimension %q", dimension.Name),
		)
		for _, alias := range dimension.Aliases {
			present := new(bool)
			values.presence[alias] = present
			values.aliasDimensions[alias] = dimension.Name
			flags.BoolVar(
				present,
				alias,
				false,
				fmt.Sprintf("Filter to artifacts in collection %q", alias),
			)
		}
	}
	return values, nil
}

func (values *artifactFilterFlagValues) parse(flags *pflag.FlagSet, args []string) error {
	for _, arg := range args {
		if !strings.HasPrefix(arg, "--") {
			continue
		}
		nameValue := strings.TrimPrefix(arg, "--")
		name, _, hasValue := strings.Cut(nameValue, "=")
		if _, alias := values.aliasDimensions[name]; alias && hasValue {
			return fmt.Errorf("flag --%s does not accept a value", name)
		}
	}
	if err := flags.Parse(args); err != nil {
		return err
	}
	for dimension, coordinates := range values.coordinates {
		for _, coordinate := range *coordinates {
			if strings.Contains(coordinate, ",") {
				return fmt.Errorf(
					"flag --%s must be repeated for multiple values; comma-separated values are not supported",
					dimension,
				)
			}
		}
	}
	return nil
}

func (values *artifactFilterFlagValues) selection(
	dimensions []artifactListDimension,
) artifactFilterSelection {
	selection := artifactFilterSelection{
		coordinates: make(map[string][]string, len(values.coordinates)),
		presence:    make(map[string]bool, len(dimensions)),
	}
	for dimension, coordinates := range values.coordinates {
		selection.coordinates[dimension] = slices.Clone(*coordinates)
	}
	for alias, present := range values.presence {
		if *present {
			selection.presence[values.aliasDimensions[alias]] = true
		}
	}
	return selection
}

func (selection artifactFilterSelection) apply(
	artifacts *dagger.Artifacts,
	dimensions []artifactListDimension,
) *dagger.Artifacts {
	for _, dimension := range dimensions {
		if coordinates := selection.coordinates[dimension.Name]; len(coordinates) > 0 {
			artifacts = artifacts.FilterCoordinates(dimension.Name, coordinates)
		} else if selection.presence[dimension.Name] {
			artifacts = artifacts.FilterDimension(dimension.Name)
		}
	}
	return artifacts
}

func printArtifactDimensionValues(
	ctx context.Context,
	cmd *cobra.Command,
	artifacts *dagger.Artifacts,
	target string,
	filters artifactFilterSelection,
	verb dagger.Verb,
) error {
	dimensions, err := artifacts.Dimensions(ctx)
	if err != nil {
		return err
	}
	dimensionNames := make([]artifactListDimension, 0, len(dimensions))
	for i := range dimensions {
		name, err := dimensions[i].Name(ctx)
		if err != nil {
			return err
		}
		dimensionNames = append(dimensionNames, artifactListDimension{Name: name})
	}
	artifacts = filters.apply(artifacts, dimensionNames)
	artifacts = artifacts.FilterDimension(target)

	items, err := artifactListItems(ctx, artifacts, verb)
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

func artifactListItems(
	ctx context.Context,
	artifacts *dagger.Artifacts,
	verb dagger.Verb,
) ([]dagger.Artifact, error) {
	if verb == "" {
		return artifacts.Items(ctx)
	}

	nodes, err := artifacts.Plan(verb).Nodes(ctx)
	if err != nil {
		return nil, err
	}
	var items []dagger.Artifact
	for i := range nodes {
		targetItems, err := nodes[i].Target().Items(ctx)
		if err != nil {
			return nil, err
		}
		items = append(items, targetItems...)
	}
	return items, nil
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
				for _, alias := range dimension.Aliases {
					fmt.Fprintf(out, "  --%s\n", alias)
				}
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
