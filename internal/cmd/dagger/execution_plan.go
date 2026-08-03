package daggercmd

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"unicode"

	"dagger.io/dagger"
	"github.com/juju/ansiterm/tabwriter"
	"github.com/muesli/termenv"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

type executionPlanRow struct {
	id                   dagger.ID
	coordinates          []string
	coordinateDimensions []string
	coordinateValid      []bool
	action               string
	after                []dagger.ID
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
) (*dagger.Artifacts, []dagger.TargetPattern, error) {
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
	include := make([]dagger.TargetPattern, len(positionals))
	for i, pattern := range positionals {
		include[i] = dagger.TargetPattern(pattern)
	}
	return artifacts, include, nil
}

func executionPlanModuleScope(args []string) string {
	var typeRoot string
	for _, target := range executionPlanTargetPatterns(args) {
		candidate, fieldPath, found := strings.Cut(string(target), ":")
		if !found || candidate == "" || fieldPath == "" {
			continue
		}
		if strings.ContainsAny(candidate, `*?[\`) {
			return ""
		}
		if typeRoot != "" && typeRoot != candidate {
			return ""
		}
		typeRoot = candidate
	}
	return typeRoot
}

func executionPlanTargetPatterns(args []string) []dagger.TargetPattern {
	var targets []dagger.TargetPattern
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if strings.HasPrefix(arg, "-") {
			if !strings.Contains(arg, "=") {
				// Dynamic filter flags are not registered until artifact
				// dimensions are loaded. This may be either a valued filter or a
				// boolean collection alias, so guessing could mistake a target
				// for a value and under-load a multi-module request.
				return nil
			}
			continue
		}
		targets = append(targets, dagger.TargetPattern(arg))
	}
	return targets
}

func printExecutionPlan(
	ctx context.Context,
	cmd *cobra.Command,
	dag *dagger.Client,
	plan *dagger.Plan,
	dimensions []artifactListDimension,
	showDependencies bool,
) error {
	rows, err := loadExecutionPlanRows(ctx, dag, plan, dimensions)
	if err != nil {
		return err
	}

	targets := map[string]struct{}{}
	for _, row := range rows {
		targets[row.coordinateKey()] = struct{}{}
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
				value := "\x00"
				if row.hasCoordinate(dimension) {
					value = "\x01" + row.coordinates[dimension]
				}
				values[value] = struct{}{}
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

func printExecutionPlanRecipes(
	ctx context.Context,
	cmd *cobra.Command,
	dag *dagger.Client,
	plan *dagger.Plan,
	dimensions []artifactListDimension,
) error {
	rows, err := loadExecutionPlanRows(ctx, dag, plan, dimensions)
	if err != nil {
		return err
	}
	fmt.Fprintln(cmd.OutOrStdout(), "# Empty target runs everything. Otherwise use:")
	for _, recipe := range executionPlanRecipes(dimensions, rows) {
		fmt.Fprintln(cmd.OutOrStdout(), recipe)
	}
	return nil
}

func executionPlanRecipes(
	dimensions []artifactListDimension,
	rows []executionPlanRow,
) []string {
	typeDimension := slices.IndexFunc(dimensions, func(dimension artifactListDimension) bool {
		return dimension.Name == artifactTypeDimension
	})
	dimensionIndexes := make(map[string]int, len(dimensions))
	for index, dimension := range dimensions {
		dimensionIndexes[dimension.Name] = index
	}
	type recipeCandidate struct {
		filterCount int
		occurrences int
	}
	recipes := map[string]recipeCandidate{}
	for _, row := range rows {
		filterIndexes := make([]int, 0, len(row.coordinateDimensions))
		seenDimensions := make(map[string]struct{}, len(row.coordinateDimensions))
		for _, dimensionName := range row.coordinateDimensions {
			if dimensionName == artifactTypeDimension {
				continue
			}
			index, found := dimensionIndexes[dimensionName]
			if !found || !row.hasCoordinate(index) {
				continue
			}
			filterIndexes = append(filterIndexes, index)
			seenDimensions[dimensionName] = struct{}{}
		}

		complete := true
		for index, dimension := range dimensions {
			if dimension.Name == artifactTypeDimension || !row.hasCoordinate(index) {
				continue
			}
			if _, found := seenDimensions[dimension.Name]; !found {
				complete = false
				break
			}
		}
		if !complete {
			continue
		}

		if len(filterIndexes) == 0 &&
			typeDimension >= 0 &&
			typeDimension < len(row.coordinates) &&
			row.hasCoordinate(typeDimension) {
			filterIndexes = append(filterIndexes, typeDimension)
		}

		filters := make([]string, 0, len(filterIndexes))
		representable := true
		for _, index := range filterIndexes {
			filter, ok := artifactFilterRecipe(
				dimensions[index].Name,
				row.coordinates[index],
			)
			if !ok {
				representable = false
				break
			}
			filters = append(filters, filter)
		}
		if !representable || len(filters) == 0 {
			continue
		}
		if typeDimension < 0 || !row.hasCoordinate(typeDimension) {
			continue
		}
		target := row.coordinates[typeDimension] + ":" + row.action
		recipe := strings.Join(append(filters, target), " ")
		candidate := recipes[recipe]
		candidate.filterCount = len(filters)
		candidate.occurrences++
		recipes[recipe] = candidate
	}

	type uniqueRecipe struct {
		value       string
		filterCount int
	}
	unique := make([]uniqueRecipe, 0, len(recipes))
	for recipe, candidate := range recipes {
		if candidate.occurrences == 1 {
			unique = append(unique, uniqueRecipe{recipe, candidate.filterCount})
		}
	}
	sort.Slice(unique, func(i, j int) bool {
		if unique[i].filterCount != unique[j].filterCount {
			return unique[i].filterCount < unique[j].filterCount
		}
		return unique[i].value < unique[j].value
	})
	sorted := make([]string, len(unique))
	for index, recipe := range unique {
		sorted[index] = recipe.value
	}
	return sorted
}

func artifactFilterRecipe(dimension, coordinate string) (string, bool) {
	if strings.IndexByte(coordinate, 0) >= 0 {
		return "", false
	}
	return "--" + dimension + "=" + shellRecipeArgument(coordinate), true
}

func shellRecipeArgument(value string) string {
	if value == "" {
		return "''"
	}
	if strings.IndexFunc(value, func(character rune) bool {
		return unicode.IsControl(character)
	}) >= 0 {
		var quoted strings.Builder
		quoted.WriteString("$'")
		for _, character := range value {
			switch character {
			case '\\':
				quoted.WriteString(`\\`)
			case '\'':
				quoted.WriteString(`\'`)
			case '\n':
				quoted.WriteString(`\n`)
			case '\r':
				quoted.WriteString(`\r`)
			case '\t':
				quoted.WriteString(`\t`)
			default:
				if unicode.IsControl(character) {
					if character <= 0x7f {
						fmt.Fprintf(&quoted, `\x%02x`, character)
					} else {
						for _, characterByte := range []byte(string(character)) {
							fmt.Fprintf(&quoted, `\x%02x`, characterByte)
						}
					}
				} else {
					quoted.WriteRune(character)
				}
			}
		}
		quoted.WriteByte('\'')
		return quoted.String()
	}
	for _, character := range value {
		if unicode.IsLetter(character) || unicode.IsDigit(character) ||
			strings.ContainsRune("-._/:@%+", character) {
			continue
		}
		return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
	}
	return value
}

func loadExecutionPlanRows(
	ctx context.Context,
	dag *dagger.Client,
	plan *dagger.Plan,
	dimensions []artifactListDimension,
) ([]executionPlanRow, error) {
	planID, err := plan.ID(ctx)
	if err != nil {
		return nil, err
	}

	var response struct {
		Node struct {
			Nodes []struct {
				ID                dagger.ID
				FunctionPath      []string
				CollectionBatched bool
				After             []dagger.ID
				Target            struct {
					Items []struct {
						Coordinates          []*string
						CoordinateDimensions []string
					}
				}
			}
		}
	}
	if err := dag.Do(ctx, &dagger.Request{
		Query: `query ExecutionPlanRows($plan: ID!) {
  node(id: $plan) {
    ... on Plan {
      nodes {
        id
        functionPath
        collectionBatched
        after
        target {
          items {
            coordinates
            coordinateDimensions
          }
        }
      }
    }
  }
}`,
		Variables: map[string]any{"plan": planID},
		OpName:    "ExecutionPlanRows",
	}, &dagger.Response{Data: &response}); err != nil {
		return nil, err
	}

	var rows []executionPlanRow
	for _, node := range response.Node.Nodes {
		if len(node.Target.Items) == 0 {
			return nil, fmt.Errorf(
				"action %q has no targets",
				strings.Join(node.FunctionPath, ":"),
			)
		}
		if !node.CollectionBatched && len(node.Target.Items) != 1 {
			return nil, fmt.Errorf(
				"unbatched action %q has %d targets",
				strings.Join(node.FunctionPath, ":"),
				len(node.Target.Items),
			)
		}
		for _, target := range node.Target.Items {
			coordinates := make([]string, len(target.Coordinates))
			coordinateValid := make([]bool, len(dimensions))
			for index, coordinate := range target.Coordinates {
				if coordinate == nil {
					continue
				}
				coordinates[index] = *coordinate
				if index < len(coordinateValid) {
					coordinateValid[index] = true
				}
			}
			rows = append(rows, executionPlanRow{
				id:                   node.ID,
				coordinates:          coordinates,
				coordinateDimensions: target.CoordinateDimensions,
				coordinateValid:      coordinateValid,
				action:               strings.Join(node.FunctionPath, ":"),
				after:                node.After,
			})
		}
	}
	return rows, nil
}

func executionPlanRowLabel(row executionPlanRow, varying []bool) string {
	var parts []string
	for i, coordinate := range row.coordinates {
		if i < len(varying) && varying[i] && row.hasCoordinate(i) {
			parts = append(parts, coordinate)
		}
	}
	return strings.Join(append(parts, row.action), ":")
}

func (row executionPlanRow) hasCoordinate(index int) bool {
	if index < 0 || index >= len(row.coordinates) {
		return false
	}
	if index < len(row.coordinateValid) {
		return row.coordinateValid[index]
	}
	return row.coordinates[index] != ""
}

func (row executionPlanRow) coordinateKey() string {
	parts := make([]string, len(row.coordinates))
	for i, coordinate := range row.coordinates {
		if row.hasCoordinate(i) {
			parts[i] = "\x01" + coordinate
		} else {
			parts[i] = "\x00"
		}
	}
	return strings.Join(parts, "\x02")
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
