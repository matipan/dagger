package daggercmd

import (
	"context"
	"fmt"
	"strings"

	"dagger.io/dagger"
	"github.com/juju/ansiterm/tabwriter"
	"github.com/muesli/termenv"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

type executionPlanRow struct {
	id          dagger.ID
	coordinates []string
	action      string
	after       []dagger.ID
}

func prepareExecutionPlanCommand(
	cmd *cobra.Command,
	args []string,
) (artifactArgs []string, needsHelp bool, rerr error) {
	knownArgs, artifactArgs, needsHelp, err := partitionExecutionPlanArgs(cmd, args)
	if err != nil {
		return nil, false, err
	}
	cmd.DisableFlagParsing = false
	cmd.Flags().SetInterspersed(true)
	if err := cmd.ParseFlags(knownArgs); err != nil {
		return nil, false, cmd.FlagErrorFunc()(cmd, err)
	}
	return artifactArgs, needsHelp, nil
}

func partitionExecutionPlanArgs(
	cmd *cobra.Command,
	args []string,
) (knownArgs, artifactArgs []string, needsHelp bool, rerr error) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--help" || arg == "-h":
			needsHelp = true

		case arg == "--":
			artifactArgs = append(artifactArgs, args[i+1:]...)
			return knownArgs, artifactArgs, needsHelp, nil

		case strings.HasPrefix(arg, "--"):
			name, hasValue := strings.CutPrefix(arg, "--")
			if at := strings.IndexByte(name, '='); at >= 0 {
				name, hasValue = name[:at], true
			} else {
				hasValue = false
			}
			flag := lookupCommandFlag(cmd, name)
			if flag == nil {
				artifactArgs = append(artifactArgs, arg)
				continue
			}
			knownArgs = append(knownArgs, arg)
			if !hasValue && flag.NoOptDefVal == "" {
				if i+1 >= len(args) {
					return nil, nil, false, fmt.Errorf("flag needs an argument: %s", arg)
				}
				i++
				knownArgs = append(knownArgs, args[i])
			}

		case strings.HasPrefix(arg, "-") && arg != "-":
			needsValue, known := shortFlagNeedsSeparateValue(cmd, arg)
			if !known {
				return nil, nil, false, fmt.Errorf("unknown shorthand flag: %s", arg)
			}
			knownArgs = append(knownArgs, arg)
			if needsValue {
				if i+1 >= len(args) {
					return nil, nil, false, fmt.Errorf("flag needs an argument: %s", arg)
				}
				i++
				knownArgs = append(knownArgs, args[i])
			}

		default:
			artifactArgs = append(artifactArgs, arg)
		}
	}
	return knownArgs, artifactArgs, needsHelp, nil
}

func lookupCommandFlag(cmd *cobra.Command, name string) *pflag.Flag {
	if flag := cmd.Flags().Lookup(name); flag != nil {
		return flag
	}
	return cmd.InheritedFlags().Lookup(name)
}

func shortFlagNeedsSeparateValue(cmd *cobra.Command, arg string) (bool, bool) {
	body := strings.TrimPrefix(arg, "-")
	if at := strings.IndexByte(body, '='); at >= 0 {
		body = body[:at]
	}
	for i, shorthand := range body {
		flag := cmd.Flags().ShorthandLookup(string(shorthand))
		if flag == nil {
			flag = cmd.InheritedFlags().ShorthandLookup(string(shorthand))
		}
		if flag == nil {
			return false, false
		}
		if flag.NoOptDefVal == "" {
			return i == len([]rune(body))-1 && !strings.Contains(arg, "="), true
		}
	}
	return false, true
}

func parseExecutionPlanArgs(
	args []string,
	artifacts *dagger.Artifacts,
	dimensions []artifactListDimension,
) (*dagger.Artifacts, []dagger.FunctionPattern, error) {
	flags := pflag.NewFlagSet("execution-plan", pflag.ContinueOnError)
	flags.SetInterspersed(true)
	flags.SetOutput(stderr)
	filterValues, err := addArtifactFilterFlags(flags, dimensions)
	if err != nil {
		return nil, nil, err
	}
	if err := filterValues.parse(flags, args); err != nil {
		return nil, nil, err
	}
	artifacts = filterValues.selection(dimensions).apply(artifacts, dimensions)
	positionals := flags.Args()
	include := make([]dagger.FunctionPattern, len(positionals))
	for i, pattern := range positionals {
		include[i] = dagger.FunctionPattern(pattern)
	}
	return artifacts, include, nil
}

func printExecutionPlan(
	ctx context.Context,
	cmd *cobra.Command,
	plan *dagger.Plan,
	showDependencies bool,
) error {
	dimensions, rows, err := loadExecutionPlanRows(ctx, plan)
	if err != nil {
		return err
	}

	targets := map[string]struct{}{}
	for _, row := range rows {
		targets[strings.Join(row.coordinates, "\x00")] = struct{}{}
	}
	if len(targets) <= 1 && !showDependencies {
		for _, row := range rows {
			fmt.Fprintln(cmd.OutOrStdout(), row.action)
		}
		return nil
	}

	varying := make([]bool, len(dimensions))
	for dimension := range dimensions {
		values := map[string]struct{}{}
		for _, row := range rows {
			if dimension < len(row.coordinates) {
				values[row.coordinates[dimension]] = struct{}{}
			}
		}
		varying[dimension] = len(values) > 1
	}

	labels := make(map[dagger.ID]string, len(rows))
	for _, row := range rows {
		if _, found := labels[row.id]; !found {
			labels[row.id] = executionPlanRowLabel(row, varying)
		}
	}

	tw := tabwriter.NewWriter(
		cmd.OutOrStdout(),
		0,
		0,
		3,
		' ',
		tabwriter.DiscardEmptyColumns,
	)
	var headers []string
	for i, dimension := range dimensions {
		if varying[i] {
			headers = append(
				headers,
				strings.ToUpper(strings.ReplaceAll(dimension.Name, "-", " ")),
			)
		}
	}
	headers = append(headers, "ACTION")
	if showDependencies {
		headers = append(headers, "AFTER")
	}
	for i, header := range headers {
		headers[i] = termenv.String(header).Bold().String()
	}
	fmt.Fprintln(tw, strings.Join(headers, "\t"))

	for _, row := range rows {
		var values []string
		for dimension := range dimensions {
			if varying[dimension] {
				values = append(values, row.coordinates[dimension])
			}
		}
		values = append(values, row.action)
		if showDependencies {
			after := make([]string, len(row.after))
			for i, id := range row.after {
				if label, found := labels[id]; found {
					after[i] = label
				} else {
					after[i] = string(id)
				}
			}
			values = append(values, strings.Join(after, ", "))
		}
		fmt.Fprintln(tw, strings.Join(values, "\t"))
	}
	return tw.Flush()
}

func loadExecutionPlanRows(
	ctx context.Context,
	plan *dagger.Plan,
) ([]artifactListDimension, []executionPlanRow, error) {
	nodes, err := plan.Nodes(ctx)
	if err != nil {
		return nil, nil, err
	}
	var dimensions []artifactListDimension
	var rows []executionPlanRow
	for i := range nodes {
		functionPath, err := nodes[i].FunctionPath(ctx)
		if err != nil {
			return nil, nil, err
		}
		target := nodes[i].Target()
		if dimensions == nil {
			dimensions, err = loadArtifactListDimensions(ctx, nil, target, false)
			if err != nil {
				return nil, nil, err
			}
		}
		targets, err := target.Items(ctx)
		if err != nil {
			return nil, nil, err
		}
		collectionBatched, err := nodes[i].CollectionBatched(ctx)
		if err != nil {
			return nil, nil, err
		}
		if len(targets) == 0 {
			return nil, nil, fmt.Errorf(
				"action %q has no targets",
				strings.Join(functionPath, ":"),
			)
		}
		if !collectionBatched && len(targets) != 1 {
			return nil, nil, fmt.Errorf(
				"unbatched action %q has %d targets",
				strings.Join(functionPath, ":"),
				len(targets),
			)
		}
		id, err := nodes[i].ID(ctx)
		if err != nil {
			return nil, nil, err
		}
		after, err := nodes[i].After(ctx)
		if err != nil {
			return nil, nil, err
		}
		for _, target := range targets {
			coordinates, err := target.Coordinates(ctx)
			if err != nil {
				return nil, nil, err
			}
			rows = append(rows, executionPlanRow{
				id:          id,
				coordinates: coordinates,
				action:      strings.Join(functionPath, ":"),
				after:       after,
			})
		}
	}
	return dimensions, rows, nil
}

func executionPlanRowLabel(row executionPlanRow, varying []bool) string {
	var parts []string
	for i, coordinate := range row.coordinates {
		if i < len(varying) && varying[i] && coordinate != "" {
			parts = append(parts, coordinate)
		}
	}
	return strings.Join(append(parts, row.action), ":")
}

func printExecutionPlanHelp(
	cmd *cobra.Command,
	dimensions []artifactListDimension,
) error {
	if err := cmd.Help(); err != nil {
		return err
	}
	if len(dimensions) == 0 {
		return nil
	}
	fmt.Fprintln(cmd.OutOrStdout(), "\nArtifact filters:")
	for _, dimension := range dimensions {
		fmt.Fprintf(cmd.OutOrStdout(), "  --%s stringArray\n", dimension.Name)
		for _, alias := range dimension.Aliases {
			fmt.Fprintf(cmd.OutOrStdout(), "  --%s\n", alias)
		}
	}
	return nil
}
