package schema

import (
	"testing"

	"github.com/dagger/dagger/core"
	"github.com/dagger/dagger/core/workspace"
	"github.com/stretchr/testify/require"
)

func TestGeneratedCheckSelectionFromConfig(t *testing.T) {
	truthy := true
	falsy := false

	for _, test := range []struct {
		name              string
		mode              core.GeneratedChecksMode
		cfg               *workspace.Config
		includeChecks     bool
		includeGenerators bool
	}{
		{"auto defaults on", core.GeneratedChecksAuto, &workspace.Config{}, true, true},
		{"auto enabled", core.GeneratedChecksAuto, &workspace.Config{CheckGenerated: &truthy}, true, true},
		{"auto disabled", core.GeneratedChecksAuto, &workspace.Config{CheckGenerated: &falsy}, true, false},
		{"include overrides config", core.GeneratedChecksInclude, &workspace.Config{CheckGenerated: &falsy}, true, true},
		{"exclude overrides config", core.GeneratedChecksExclude, &workspace.Config{CheckGenerated: &truthy}, true, false},
		{"only generators", core.GeneratedChecksOnly, &workspace.Config{CheckGenerated: &falsy}, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			checks, generators, err := generatedCheckSelectionFromConfig(test.mode, test.cfg)
			require.NoError(t, err)
			require.Equal(t, test.includeChecks, checks)
			require.Equal(t, test.includeGenerators, generators)
		})
	}
}
