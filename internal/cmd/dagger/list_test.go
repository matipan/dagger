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
	require.Equal(t, []string{"go", "js"}, filters[artifactTypeDimension])
	require.Equal(t, []string{"TestFoo"}, filters["go-test"])
}

func TestParseArtifactListArgsDimension(t *testing.T) {
	dimensions := []artifactListDimension{
		{Name: artifactTypeDimension},
		{Name: "go-test"},
	}

	target, filters, err := parseArtifactListArgs([]string{"go-test"}, dimensions)
	require.NoError(t, err)
	require.Equal(t, "go-test", target)
	require.Empty(t, filters[artifactTypeDimension])
	require.Empty(t, filters["go-test"])
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
