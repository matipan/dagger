package dangv2

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadDangTypeDirectives(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "main.dang"),
		[]byte(`
type Plain {}

type Tests @collection {
  pub names: [String!]! @keys
}
`),
		0o600,
	))

	directives, err := loadDangTypeDirectives(dir)
	require.NoError(t, err)
	require.Empty(t, directives["Plain"])
	require.Len(t, directives["Tests"], 1)
	require.Equal(t, "collection", directives["Tests"][0].Name)
}
