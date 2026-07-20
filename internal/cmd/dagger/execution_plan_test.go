package daggercmd

import (
	"testing"

	"dagger.io/dagger"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func TestPrepareExecutionPlanCommand(t *testing.T) {
	root := &cobra.Command{Use: "dagger"}
	var progress string
	root.PersistentFlags().StringVar(&progress, "progress", "auto", "")

	command := &cobra.Command{
		Use:                "check",
		DisableFlagParsing: true,
	}
	var list bool
	var module string
	command.Flags().BoolVarP(&list, "list", "l", false, "")
	command.Flags().StringVarP(&module, "mod", "m", "", "")
	root.AddCommand(command)

	artifactArgs, needsHelp, err := prepareExecutionPlanCommand(command, []string{
		"--type", "go",
		"go:lint",
		"-l",
		"tests:**",
		"--mod=.",
		"--progress", "plain",
	})
	require.NoError(t, err)
	require.False(t, needsHelp)
	require.True(t, list)
	require.Equal(t, ".", module)
	require.Equal(t, "plain", progress)
	require.Equal(t, []string{
		"--type", "go",
		"go:lint",
		"tests:**",
	}, artifactArgs)
}

func TestPrepareExecutionPlanCommandHelpIsDeferred(t *testing.T) {
	command := &cobra.Command{Use: "generate"}

	artifactArgs, needsHelp, err := prepareExecutionPlanCommand(command, []string{
		"codegen",
		"--help",
	})
	require.NoError(t, err)
	require.True(t, needsHelp)
	require.Equal(t, []string{"codegen"}, artifactArgs)
}

func TestPrepareExecutionPlanCommandRejectsUnknownShorthand(t *testing.T) {
	command := &cobra.Command{Use: "check"}

	_, _, err := prepareExecutionPlanCommand(command, []string{"-x"})
	require.EqualError(t, err, "unknown shorthand flag: -x")
}

func TestPrepareExecutionPlanCommandAcceptsAttachedShorthandValue(t *testing.T) {
	command := &cobra.Command{
		Use:                "check",
		DisableFlagParsing: true,
	}
	var module string
	command.Flags().StringVarP(&module, "mod", "m", "", "")

	artifactArgs, _, err := prepareExecutionPlanCommand(command, []string{"-m.", "lint"})
	require.NoError(t, err)
	require.Equal(t, ".", module)
	require.Equal(t, []string{"lint"}, artifactArgs)
}

func TestParseExecutionPlanFilters(t *testing.T) {
	filters, include, err := parseExecutionPlanFilters([]string{
		"--type=go",
		"lint",
		"--go-test", "TestFoo",
		"--type", "js",
		"tests:**",
	})
	require.NoError(t, err)
	require.Equal(t, []executionPlanDimensionFilter{
		{name: "type", values: []string{"go", "js"}},
		{name: "go-test", values: []string{"TestFoo"}},
	}, filters)
	require.Equal(t, []dagger.FunctionPattern{"lint", "tests:**"}, include)
}

func TestParseExecutionPlanFiltersErrors(t *testing.T) {
	_, _, err := parseExecutionPlanFilters([]string{"--"})
	require.EqualError(t, err, `invalid artifact filter "--"`)

	_, _, err = parseExecutionPlanFilters([]string{"--type"})
	require.EqualError(t, err, "flag needs an argument: --type")
}

func TestExecutionPlanRowLabelUsesOnlyVaryingCoordinates(t *testing.T) {
	row := executionPlanRow{
		coordinates: []string{"go", "module-a"},
		action:      "tests:unit",
	}
	require.Equal(
		t,
		"module-a:tests:unit",
		executionPlanRowLabel(row, []bool{false, true}),
	)
}
