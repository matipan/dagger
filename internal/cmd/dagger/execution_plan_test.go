package daggercmd

import (
	"os"
	"os/exec"
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
		"go:tests:**",
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
		"go:tests:**",
	}, artifactArgs)
}

func TestPrepareExecutionPlanCommandHelpIsDeferred(t *testing.T) {
	command := &cobra.Command{Use: "generate"}

	artifactArgs, needsHelp, err := prepareExecutionPlanCommand(command, []string{
		"go:codegen",
		"--help",
	})
	require.NoError(t, err)
	require.True(t, needsHelp)
	require.Equal(t, []string{"go:codegen"}, artifactArgs)
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

	artifactArgs, _, err := prepareExecutionPlanCommand(command, []string{"-m.", "go:lint"})
	require.NoError(t, err)
	require.Equal(t, ".", module)
	require.Equal(t, []string{"go:lint"}, artifactArgs)
}

func TestPrepareExecutionPlanCommandDoesNotConsumeCollectionAliasValue(t *testing.T) {
	command := &cobra.Command{
		Use:                "check",
		DisableFlagParsing: true,
	}

	artifactArgs, _, err := prepareExecutionPlanCommand(
		command,
		[]string{"--go-tests", "go-test:lint"},
	)
	require.NoError(t, err)
	require.Equal(t, []string{"--go-tests", "go-test:lint"}, artifactArgs)
}

func TestExecutionPlanModuleScope(t *testing.T) {
	t.Run("uses positional target type", func(t *testing.T) {
		require.Equal(t, "go-test", executionPlanModuleScope([]string{
			"--go-module=api",
			"go-test:test",
		}))
	})

	t.Run("ambiguous separate filter disables narrowing", func(t *testing.T) {
		require.Empty(t, executionPlanModuleScope([]string{
			"--e2e-test-suite", "sdk-dev:go",
			"e2e-test-suite:verify",
		}))
	})

	t.Run("same type targets retain scope", func(t *testing.T) {
		require.Equal(t, "go-test", executionPlanModuleScope([]string{
			"go-test:test",
			"go-test:lint",
		}))
	})

	t.Run("different type targets disable narrowing", func(t *testing.T) {
		require.Empty(t, executionPlanModuleScope([]string{
			"go-test:test",
			"python-test:test",
		}))
	})

	t.Run("globbed type target disables narrowing", func(t *testing.T) {
		require.Empty(t, executionPlanModuleScope([]string{
			"go-*:test",
		}))
	})

	t.Run("filters without targets do not set scope", func(t *testing.T) {
		require.Empty(t, executionPlanModuleScope([]string{
			"--go-test=TestAuth",
		}))
	})
}

func TestExecutionPlanTargetPatterns(t *testing.T) {
	require.Equal(t, []dagger.TargetPattern{
		"go-test:test",
		"go-module:lint",
	}, executionPlanTargetPatterns([]string{
		"--go-module=api",
		"--go-test=TestAuth",
		"go-test:test",
		"go-module:lint",
	}))
	require.Nil(t, executionPlanTargetPatterns([]string{
		"--go-tests",
		"go-test:test",
		"python-test:test",
	}))
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

func TestExecutionPlanRecipesAreRunnableSelectors(t *testing.T) {
	dimensions := []artifactListDimension{
		{Name: artifactTypeDimension},
		{Name: "e2e-test-suite"},
		{Name: "e2e-sdk-dev"},
	}
	rows := []executionPlanRow{
		{coordinates: []string{"e2e-test-suite", "e2e:engine", ""}, coordinateDimensions: []string{"type", "e2e-test-suite"}, action: "run"},
		{coordinates: []string{"e2e-test-suite", "e2e:engine", ""}, coordinateDimensions: []string{"type", "e2e-test-suite"}, action: "verify"},
		{coordinates: []string{"e2e-test-suite", "sdk-dev:go", "e2e:sdks"}, coordinateDimensions: []string{"type", "e2e-sdk-dev", "e2e-test-suite"}, action: "run"},
		{coordinates: []string{"e2e-test-suite", "sdk-dev:go", "e2e:sdks"}, coordinateDimensions: []string{"type", "e2e-sdk-dev", "e2e-test-suite"}, action: "verify"},
		{coordinates: []string{"e2e-test-suite", "sdk-dev:go", "e2e:sdksarm"}, coordinateDimensions: []string{"type", "e2e-sdk-dev", "e2e-test-suite"}, action: "run"},
		{coordinates: []string{"e2e-test-suite", "sdk-dev:go", "e2e:sdksarm"}, coordinateDimensions: []string{"type", "e2e-sdk-dev", "e2e-test-suite"}, action: "verify"},
		{coordinates: []string{"e2e-test-suite", "fluffy", ""}, coordinateDimensions: []string{"type", "e2e-test-suite"}, action: "run"},
		{coordinates: []string{"e2e-test-suite", "fluffy", ""}, coordinateDimensions: []string{"type", "e2e-test-suite"}, action: "verify"},
	}

	require.Equal(t, []string{
		"--e2e-test-suite=e2e:engine e2e-test-suite:run",
		"--e2e-test-suite=e2e:engine e2e-test-suite:verify",
		"--e2e-test-suite=fluffy e2e-test-suite:run",
		"--e2e-test-suite=fluffy e2e-test-suite:verify",
		"--e2e-sdk-dev=e2e:sdks --e2e-test-suite=sdk-dev:go e2e-test-suite:run",
		"--e2e-sdk-dev=e2e:sdks --e2e-test-suite=sdk-dev:go e2e-test-suite:verify",
		"--e2e-sdk-dev=e2e:sdksarm --e2e-test-suite=sdk-dev:go e2e-test-suite:run",
		"--e2e-sdk-dev=e2e:sdksarm --e2e-test-suite=sdk-dev:go e2e-test-suite:verify",
	}, executionPlanRecipes(dimensions, rows))
}

func TestExecutionPlanRecipesUseEveryArtifactCoordinate(t *testing.T) {
	dimensions := []artifactListDimension{
		{Name: artifactTypeDimension},
		{Name: "go-module"},
		{Name: "go-directory"},
		{Name: "go-test"},
	}
	rows := []executionPlanRow{
		{
			coordinates:          []string{"go-module", "api", "", ""},
			coordinateDimensions: []string{"type", "go-module"},
			coordinateValid:      []bool{true, true, false, false},
			action:               "execute",
		},
		{
			coordinates:          []string{"go-module", "api", "", ""},
			coordinateDimensions: []string{"type", "go-module"},
			coordinateValid:      []bool{true, true, false, false},
			action:               "lint",
		},
		{
			coordinates:          []string{"go-directory", "api", "api/auth", ""},
			coordinateDimensions: []string{"type", "go-module", "go-directory"},
			coordinateValid:      []bool{true, true, true, false},
			action:               "execute",
		},
		{
			coordinates:          []string{"go-test", "api", "api/auth", "TestAuth"},
			coordinateDimensions: []string{"type", "go-module", "go-directory", "go-test"},
			coordinateValid:      []bool{true, true, true, true},
			action:               "execute",
		},
	}

	require.Equal(t, []string{
		"--go-module=api go-module:execute",
		"--go-module=api go-module:lint",
		"--go-module=api --go-directory=api/auth go-directory:execute",
		"--go-module=api --go-directory=api/auth --go-test=TestAuth go-test:execute",
	}, executionPlanRecipes(dimensions, rows))
}

func TestExecutionPlanRecipesUseArtifactAncestryOrder(t *testing.T) {
	dimensions := []artifactListDimension{
		{Name: artifactTypeDimension},
		{Name: "e2e-test-suite"},
		{Name: "e2e-platform"},
		{Name: "e2e-sdk-dev"},
	}
	rows := []executionPlanRow{{
		coordinates:          []string{"e2e-test-suite", "sdk-dev:go", "linux", "platform:sdk"},
		coordinateDimensions: []string{"type", "e2e-platform", "e2e-sdk-dev", "e2e-test-suite"},
		action:               "verify",
	}}

	require.Equal(t, []string{
		"--e2e-platform=linux --e2e-sdk-dev=platform:sdk --e2e-test-suite=sdk-dev:go e2e-test-suite:verify",
	}, executionPlanRecipes(dimensions, rows))
}

func TestExecutionPlanRecipesOmitNonUniqueSelectors(t *testing.T) {
	dimensions := []artifactListDimension{
		{Name: artifactTypeDimension},
		{Name: "go-test"},
	}
	row := executionPlanRow{
		coordinates:          []string{"go-test", "unit"},
		coordinateDimensions: []string{"type", "go-test"},
		action:               "run",
	}

	require.Empty(t, executionPlanRecipes(dimensions, []executionPlanRow{row, row}))
}

func TestExecutionPlanRecipesOmitNULCoordinates(t *testing.T) {
	dimensions := []artifactListDimension{
		{Name: artifactTypeDimension},
		{Name: "go-test"},
	}
	rows := []executionPlanRow{{
		coordinates:          []string{"go-test", "a\x00b"},
		coordinateDimensions: []string{"type", "go-test"},
		action:               "run",
	}}

	require.Empty(t, executionPlanRecipes(dimensions, rows))
}

func TestExecutionPlanRecipesPreserveEmptyCoordinates(t *testing.T) {
	dimensions := []artifactListDimension{
		{Name: artifactTypeDimension},
		{Name: "go-test"},
	}
	rows := []executionPlanRow{{
		coordinates:          []string{"go-test", ""},
		coordinateDimensions: []string{"type", "go-test"},
		coordinateValid:      []bool{true, true},
		action:               "run",
	}}

	require.Equal(t, []string{
		"--go-test='' go-test:run",
	}, executionPlanRecipes(dimensions, rows))
}

func TestShellRecipeArgumentQuotesUnsafeValues(t *testing.T) {
	require.Equal(t, "''", shellRecipeArgument(""))
	require.Equal(t, "fluffy", shellRecipeArgument("fluffy"))
	require.Equal(t, "'a b'", shellRecipeArgument("a b"))
	require.Equal(t, `'a'"'"'b'`, shellRecipeArgument("a'b"))
	require.Equal(t, `$'line\nbreak'`, shellRecipeArgument("line\nbreak"))
	require.Equal(t, `$'a\tb'`, shellRecipeArgument("a\tb"))
	require.Equal(t, `$'a\\b\'c\x01'`, shellRecipeArgument("a\\b'c\x01"))
	require.Equal(t, `$'a\xc2\x85b'`, shellRecipeArgument("a\u0085b"))
}

func TestShellRecipeArgumentRoundTrips(t *testing.T) {
	for _, value := range []string{
		"",
		"fluffy",
		"a b",
		"a'b",
		"line\nbreak",
		"a\tb",
		"a\\b'c\x01",
		"a\u0085b",
	} {
		t.Run(value, func(t *testing.T) {
			cmd := exec.Command("bash", "-c", "printf %s "+shellRecipeArgument(value))
			cmd.Env = append(os.Environ(), "LC_ALL=C")
			out, err := cmd.Output()
			require.NoError(t, err)
			require.Equal(t, []byte(value), out)
		})
	}
}
