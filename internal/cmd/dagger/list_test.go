package daggercmd

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseArtifactListArgs(t *testing.T) {
	dimensions := []artifactListDimension{
		{Name: artifactTypeDimension},
		{Name: "go-test"},
	}

	target, filters, err := parseArtifactListArgs([]string{
		"types",
		"--type=go",
		"--type=js",
		"--go-test", "TestFoo",
	}, dimensions)
	require.NoError(t, err)
	require.Equal(t, artifactTypeDimension, target)
	require.Equal(t, []string{"go", "js"}, filters.coordinates[artifactTypeDimension])
	require.Equal(t, []string{"TestFoo"}, filters.coordinates["go-test"])
}

func TestParseArtifactListArgsDimension(t *testing.T) {
	dimensions := []artifactListDimension{
		{Name: artifactTypeDimension},
		{Name: "go-test"},
	}

	target, filters, err := parseArtifactListArgs([]string{"go-test"}, dimensions)
	require.NoError(t, err)
	require.Equal(t, "go-test", target)
	require.Empty(t, filters.coordinates[artifactTypeDimension])
	require.Empty(t, filters.coordinates["go-test"])
}

func TestParseArtifactListArgsCollectionAlias(t *testing.T) {
	dimensions := []artifactListDimension{
		{Name: artifactTypeDimension},
		{Name: "go-test", Aliases: []string{"go-tests"}},
	}

	target, filters, err := parseArtifactListArgs(
		[]string{"go-test", "--go-tests"},
		dimensions,
	)
	require.NoError(t, err)
	require.Equal(t, "go-test", target)
	require.True(t, filters.presence["go-test"])

	_, _, err = parseArtifactListArgs(
		[]string{"go-test", "--go-tests=false"},
		dimensions,
	)
	require.EqualError(t, err, "flag --go-tests does not accept a value")
}

func TestParseArtifactListArgsRejectsCollectionAliasDimensionCollision(t *testing.T) {
	dimensions := []artifactListDimension{
		{Name: artifactTypeDimension},
		{Name: "go-test", Aliases: []string{"go-tests"}},
		{Name: "go-tests"},
	}

	_, _, err := parseArtifactListArgs([]string{"go-test"}, dimensions)
	require.EqualError(
		t,
		err,
		`artifact collection alias "go-tests" conflicts with filter "go-tests"`,
	)
}

func TestParseArtifactListArgsRejectsCommaSeparatedCoordinates(t *testing.T) {
	dimensions := []artifactListDimension{{Name: artifactTypeDimension}}
	_, _, err := parseArtifactListArgs(
		[]string{"types", "--type=go,js"},
		dimensions,
	)
	require.EqualError(
		t,
		err,
		"flag --type must be repeated for multiple values; comma-separated values are not supported",
	)
}

func TestParseArtifactListArgsErrors(t *testing.T) {
	dimensions := []artifactListDimension{{Name: artifactTypeDimension}}

	_, _, err := parseArtifactListArgs(nil, dimensions)
	require.EqualError(t, err, "accepts 1 arg(s), received 0")

	_, _, err = parseArtifactListArgs([]string{"missing"}, dimensions)
	require.EqualError(t, err, `artifact dimension "missing" not found`)

	_, _, err = parseArtifactListArgs([]string{"types", "--missing=value"}, dimensions)
	require.EqualError(t, err, "unknown flag: --missing")
}
