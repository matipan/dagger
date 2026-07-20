package core

// These tests cover `dagger check`, which discovers and runs module check
// functions. They verify listing and running checks from SDK modules, legacy
// compat blueprints, and workspace-installed modules.
//
// See also:
// - generators_test.go: generator discovery and execution.
// - workspace_modules_test.go: installing modules into workspaces.

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	"dagger.io/dagger"
	"github.com/dagger/testctx"
	"github.com/stretchr/testify/require"
)

type ChecksSuite struct{}

func TestChecks(t *testing.T) {
	testctx.New(t, Middleware()...).RunTests(ChecksSuite{})
}

func checksTestEnv(t *testctx.T, c *dagger.Client) (*dagger.Container, error) {
	return specificTestEnv(t, c, "checks")
}

func specificTestEnv(t *testctx.T, c *dagger.Client, subfolder string) (*dagger.Container, error) {
	// java SDK is not embedded in the engine, so we mount the java sdk to be able
	// to test non released features
	javaSdkSrc, err := filepath.Abs("../../sdk/java")
	if err != nil {
		return nil, err
	}
	return c.Container().
			From(alpineImage).
			// init git in a directory containing both the modules and the java SDK
			// that way dagger sees this directory as the root
			WithWorkdir("/work").
			WithExec([]string{"apk", "add", "git"}).
			WithExec([]string{"git", "init"}).
			WithWorkdir("/work/modules/").
			WithMountedFile(testCLIBinPath, daggerCliFile(t, c)).
			WithDirectory(".", c.Host().Directory("./testdata/"+subfolder)).
			WithMountedDirectory("/work/sdk/java", c.Host().Directory(javaSdkSrc)).
			WithDirectory("app", c.Directory()),
		nil
}

func (ChecksSuite) TestChecksDirectSDK(ctx context.Context, t *testctx.T) {
	c := connect(ctx, t)
	for _, tc := range []struct {
		name string
		path string
	}{
		{"go", "hello-with-checks"},
		{"typescript", "hello-with-checks-ts"},
		{"python", "hello-with-checks-py"},
		{"java", "hello-with-checks-java"},
	} {
		t.Run(tc.name, func(ctx context.Context, t *testctx.T) {
			modGen, err := checksTestEnv(t, c)
			require.NoError(t, err)
			modGen = modGen.
				WithWorkdir(tc.path)
			// list checks
			out, err := modGen.
				With(daggerExec("check", "-l")).
				CombinedOutput(ctx)
			require.NoError(t, err, out)
			require.Contains(t, out, "passing-check")
			require.Contains(t, out, "failing-check")
			require.Contains(t, out, "passing-container")
			require.Contains(t, out, "failing-container")
			require.Contains(t, out, "test:lint")
			require.Contains(t, out, "test:unit")
			// run a specific passing check
			out, err = modGen.
				With(daggerExec("--progress=report", "check", "passing*")).
				CombinedOutput(ctx)
			require.NoError(t, err)
			require.Regexp(t, `passing-check.*OK`, out)
			require.Regexp(t, `passing-container.*OK`, out)
			// run a specific failing check
			out, err = modGen.
				With(daggerExecFail("--progress=report", "check", "failing*")).
				CombinedOutput(ctx)
			require.Regexp(t, "failing-check.*ERROR", out)
			require.Regexp(t, "failing-container.*ERROR", out)
			require.NoError(t, err)
			// run all checks
			out, err = modGen.
				With(daggerExecFail("--progress=report", "check")).
				CombinedOutput(ctx)
			require.Regexp(t, `passing-check.*OK`, out)
			require.Regexp(t, `passing-container.*OK`, out)
			require.Regexp(t, "failing-check.*ERROR", out)
			require.Regexp(t, "failing-container.*ERROR", out)
			require.NoError(t, err)
		})
	}
}

func (ChecksSuite) TestChecksNoMatch(ctx context.Context, t *testctx.T) {
	c := connect(ctx, t)
	modGen, err := checksTestEnv(t, c)
	require.NoError(t, err)

	out, err := modGen.
		WithWorkdir("hello-with-checks").
		With(daggerExecFail("--progress=report", "check", "missing-check")).
		CombinedOutput(ctx)
	require.NoError(t, err)
	require.Contains(t, out, `no checks matched pattern "missing-check"`)
}

func (ChecksSuite) TestChecksViaLegacyBlueprintConfig(ctx context.Context, t *testctx.T) {
	c := connect(ctx, t)
	for _, tc := range []struct {
		name string
		path string
	}{
		{"go", "hello-with-checks"},
		{"typescript", "hello-with-checks-ts"},
		{"python", "hello-with-checks-py"},
		{"java", "hello-with-checks-java"},
	} {
		t.Run(tc.name, func(ctx context.Context, t *testctx.T) {
			modGen, err := checksTestEnv(t, c)
			require.NoError(t, err)
			modGen = modGen.WithWorkdir("app").
				WithNewFile("dagger.json", `{"name":"app","blueprint":{"name":"blueprint","source":"../`+tc.path+`"}}`)
			// list checks
			out, err := modGen.
				With(daggerExec("check", "-l")).
				CombinedOutput(ctx)
			require.NoError(t, err)
			require.Contains(t, out, "passing-check")
			require.Contains(t, out, "failing-check")
			// run a specific passing check
			out, err = modGen.
				With(daggerExec("--progress=report", "check", "passing-check")).
				CombinedOutput(ctx)
			require.NoError(t, err)
			require.Regexp(t, `passing-check.*OK`, out)
			// run a specific failing check
			out, err = modGen.
				With(daggerExecFail("--progress=report", "check", "failing-check")).
				CombinedOutput(ctx)
			require.Regexp(t, "failing-check.*ERROR", out)
			require.NoError(t, err)
			// run all checks
			out, err = modGen.
				With(daggerExecFail("--progress=report", "check")).
				CombinedOutput(ctx)
			require.Regexp(t, `passing-check.*OK`, out)
			require.Regexp(t, `failing-check.*ERROR`, out)
			require.NoError(t, err)
		})
	}
}

func (ChecksSuite) TestChecksSkipFlag(ctx context.Context, t *testctx.T) {
	c := connect(ctx, t)
	modGen, err := checksTestEnv(t, c)
	require.NoError(t, err)
	modGen = modGen.WithWorkdir("hello-with-checks")

	t.Run("list with skip excludes matching checks", func(ctx context.Context, t *testctx.T) {
		out, err := modGen.
			With(daggerExec("check", "-l", "--skip", "failing-*")).
			CombinedOutput(ctx)
		require.NoError(t, err)
		require.Contains(t, out, "passing-check")
		require.Contains(t, out, "passing-container")
		require.Contains(t, out, "test:lint")
		require.Contains(t, out, "test:unit")
		require.NotContains(t, out, "failing-check")
		require.NotContains(t, out, "failing-container")
	})

	t.Run("list with glob skip pattern", func(ctx context.Context, t *testctx.T) {
		out, err := modGen.
			With(daggerExec("check", "-l", "--skip", "**:unit")).
			CombinedOutput(ctx)
		require.NoError(t, err)
		require.Contains(t, out, "test:lint")
		require.NotContains(t, out, "test:unit")
	})

	t.Run("list with prefix skip pattern", func(ctx context.Context, t *testctx.T) {
		out, err := modGen.
			With(daggerExec("check", "-l", "--skip", "test")).
			CombinedOutput(ctx)
		require.NoError(t, err)
		require.Contains(t, out, "passing-check")
		require.NotContains(t, out, "test:lint")
		require.NotContains(t, out, "test:unit")
	})

	t.Run("list with include and skip combined", func(ctx context.Context, t *testctx.T) {
		out, err := modGen.
			With(daggerExec("check", "-l", "test", "--skip", "**:unit")).
			CombinedOutput(ctx)
		require.NoError(t, err)
		require.Contains(t, out, "test:lint")
		require.NotContains(t, out, "test:unit")
		require.NotContains(t, out, "passing-check")
	})

	t.Run("run with skip excludes matching checks", func(ctx context.Context, t *testctx.T) {
		out, err := modGen.
			With(daggerExec("--progress=report", "check", "--skip", "failing-*")).
			CombinedOutput(ctx)
		require.NoError(t, err)
		require.Regexp(t, `passing-check.*OK`, out)
		require.Regexp(t, `passing-container.*OK`, out)
		require.NotContains(t, out, "failing-check")
		require.NotContains(t, out, "failing-container")
	})
}

func (ChecksSuite) TestWorkspaceCheckSkip(ctx context.Context, t *testctx.T) {
	c := connect(ctx, t)
	modGen, err := checksTestEnv(t, c)
	require.NoError(t, err)

	ctr := modGen.WithNewFile("dagger.toml", `[modules.hello-with-checks]
source = "hello-with-checks"
check.skip = ["failing-check", "failing-container"]
`)

	out, err := ctr.With(daggerExec("check", "-l")).CombinedOutput(ctx)
	require.NoError(t, err)
	require.Contains(t, out, "passing-check")
	require.Contains(t, out, "passing-container")
	require.NotContains(t, out, "failing-check")
	require.NotContains(t, out, "failing-container")
}

func (ChecksSuite) TestWorkspaceCheckSkipRemote(ctx context.Context, t *testctx.T) {
	c := connect(ctx, t)
	remoteRef := workspaceSelectionRemoteRef(ctx, t, c, c.Directory().
		WithNewFile("dagger.toml", `[modules.hello-with-checks]
source = ".dagger/modules/hello-with-checks"
check.skip = ["failing-check", "failing-container"]
`).
		WithDirectory(".dagger/modules/hello-with-checks", c.Host().Directory(testDataPath(t, "checks", "hello-with-checks"))))

	out, err := c.Container().From(alpineImage).
		WithMountedFile(testCLIBinPath, daggerCliFile(t, c)).
		WithWorkdir("/empty").
		With(workspaceSelectionDaggerExec("-W", remoteRef, "check", "-l")).
		CombinedOutput(ctx)
	require.NoError(t, err, out)
	require.Contains(t, out, "passing-check")
	require.Contains(t, out, "passing-container")
	require.NotContains(t, out, "failing-check")
	require.NotContains(t, out, "failing-container")
}

func (ChecksSuite) TestChecksFailFast(ctx context.Context, t *testctx.T) {
	c := connect(ctx, t)
	modGen, err := checksTestEnv(t, c)
	require.NoError(t, err)
	modGen = modGen.WithWorkdir("hello-with-checks")

	out, err := modGen.
		With(daggerExecFail("--progress=report", "check", "--failfast")).
		CombinedOutput(ctx)
	require.NoError(t, err)
	require.Contains(t, out, "ERROR")
	require.Contains(t, out, "context canceled")
}

func (ChecksSuite) TestChecksAsToolchain(ctx context.Context, t *testctx.T) {
	c := connect(ctx, t)
	for _, tc := range []struct {
		name string
		path string
	}{
		{"go", "hello-with-checks"},
		{"typescript", "hello-with-checks-ts"},
		{"python", "hello-with-checks-py"},
		{"java", "hello-with-checks-java"},
	} {
		t.Run(tc.name, func(ctx context.Context, t *testctx.T) {
			// Install hello-with-checks into the current workspace.
			modGen, err := checksTestEnv(t, c)
			require.NoError(t, err)
			modGen = modGen.
				WithWorkdir("app").
				WithNewFile("dagger.toml", fmt.Sprintf(`[modules.%s]
source = "../%s"
`, tc.path, tc.path))
			// list checks
			out, err := modGen.
				With(daggerExec("check", "-l")).
				CombinedOutput(ctx)
			require.NoError(t, err)
			require.Contains(t, out, "passing-check")
			require.Contains(t, out, "failing-check")
			require.Contains(t, out, "test:lint")
			require.Contains(t, out, "test:unit")
			// run a specific passing check
			out, err = modGen.
				With(daggerExec("--progress=report", "check", tc.path+":passing-check")).
				CombinedOutput(ctx)
			require.NoError(t, err)
			// run a specific failing check
			out, err = modGen.
				With(daggerExecFail("--progress=report", "check", tc.path+":failing-check")).
				CombinedOutput(ctx)
			require.NoError(t, err)
			// run all checks
			out, err = modGen.
				With(daggerExecFail("--progress=report", "check")).
				CombinedOutput(ctx)
			require.NoError(t, err)
		})
	}
}

func (ChecksSuite) TestDangExecutionPlans(ctx context.Context, t *testctx.T) {
	c := connect(ctx, t)
	mod := workspaceBase(t, c).
		With(initStandaloneDangModule("plans-fixture", `
type PlansFixture {
  pub state: Nested!

  new() {
    self.state = Nested()
    self
  }

  pub passing: Void @check {
    null
  }

  pub nested: Nested! {
    Nested()
  }

  pub configured(required: String!): Void @check {
    null
  }

  pub generateFile: Changeset! @generate {
    directory
      .withNewFile("generated.txt", "generated by artifact plan")
      .changes(directory)
  }
}

type Nested {
  pub passing: Void @check {
    null
  }
}
`))

	out, err := mod.
		With(daggerExec("check", "-l", "--type=plans-fixture")).
		CombinedOutput(ctx)
	require.NoError(t, err, out)
	require.Contains(t, out, "passing")
	require.Contains(t, out, "nested:passing")
	require.Contains(t, out, "state:passing")
	require.NotContains(t, out, "configured")

	out, err = mod.With(daggerQuery(`{
		currentWorkspace {
			artifacts {
				filterCoordinates(dimension: "type", values: ["plans-fixture"]) {
					items {
						actions(verbs: [CHECK]) {
							verb
							functionPath
							collectionBatched
							target { items { coordinates } }
							after
						}
						action(verb: CHECK, functionPath: ["nested", "passing"]) {
							verb
							functionPath
							collectionBatched
							target { items { coordinates } }
							after
							withAfter(actions: []) { after }
							run
						}
					}
					plan(verb: CHECK, include: ["nested:*"]) {
						verb
						nodes {
							functionPath
							target { items { coordinates } }
						}
						run
					}
				}
			}
		}
	}`)).Stdout(ctx)
	require.NoError(t, err)
	require.JSONEq(t, `{
		"currentWorkspace": {
			"artifacts": {
				"filterCoordinates": {
					"items": [{
						"actions": [{
							"verb": "CHECK",
							"functionPath": ["nested", "passing"],
							"collectionBatched": false,
							"target": {"items": [{"coordinates": ["plans-fixture"]}]},
							"after": []
						}, {
							"verb": "CHECK",
							"functionPath": ["passing"],
							"collectionBatched": false,
							"target": {"items": [{"coordinates": ["plans-fixture"]}]},
							"after": []
						}, {
							"verb": "CHECK",
							"functionPath": ["state", "passing"],
							"collectionBatched": false,
							"target": {"items": [{"coordinates": ["plans-fixture"]}]},
							"after": []
						}],
						"action": {
							"verb": "CHECK",
							"functionPath": ["nested", "passing"],
							"collectionBatched": false,
							"target": {"items": [{"coordinates": ["plans-fixture"]}]},
							"after": [],
							"withAfter": {"after": []},
							"run": null
						}
					}],
					"plan": {
						"verb": "CHECK",
						"nodes": [{
							"functionPath": ["nested", "passing"],
							"target": {"items": [{"coordinates": ["plans-fixture"]}]}
						}],
						"run": null
					}
				}
			}
		}
		}`, out)

	actionIDJSON, err := mod.With(daggerQuery(`{
		currentWorkspace {
			artifacts {
				filterCoordinates(dimension: "type", values: ["plans-fixture"]) {
					items {
						action(verb: CHECK, functionPath: ["passing"]) {
							id
						}
					}
				}
			}
		}
	}`)).Stdout(ctx)
	require.NoError(t, err)
	var actionIDResult struct {
		CurrentWorkspace struct {
			Artifacts struct {
				Filtered struct {
					Items []struct {
						Action struct {
							ID string
						}
					}
				} `json:"filterCoordinates"`
			}
		}
	}
	require.NoError(t, json.Unmarshal([]byte(actionIDJSON), &actionIDResult))
	require.Len(t, actionIDResult.CurrentWorkspace.Artifacts.Filtered.Items, 1)
	actionID := actionIDResult.CurrentWorkspace.Artifacts.Filtered.Items[0].Action.ID
	require.NotEmpty(t, actionID)

	out, err = mod.With(daggerQuery(fmt.Sprintf(`{
		currentWorkspace {
			artifacts {
				filterCoordinates(dimension: "type", values: ["plans-fixture"]) {
					items {
						action(verb: CHECK, functionPath: ["passing"]) {
							withAfter(actions: [%q]) {
								run
							}
						}
					}
				}
			}
		}
	}`, actionID))).CombinedOutput(ctx)
	require.Error(t, err, out)

	out, err = mod.
		With(daggerExec("check", "--plan", "--type=plans-fixture", "nested:*")).
		CombinedOutput(ctx)
	require.NoError(t, err, out)
	require.Contains(t, out, "nested:passing")
	require.NotContains(t, out, "\tpassing\t")

	out, err = mod.
		With(daggerExec("--progress=report", "check", "--type=plans-fixture", "passing")).
		CombinedOutput(ctx)
	require.NoError(t, err, out)
	require.Regexp(t, `passing.*OK`, out)

	out, err = mod.
		With(daggerExec("generate", "-l", "--type=plans-fixture")).
		CombinedOutput(ctx)
	require.NoError(t, err, out)
	require.Contains(t, out, "generate-file")

	mod = mod.With(daggerExec(
		"--progress=plain",
		"generate",
		"--type=plans-fixture",
		"generate-file",
		"-y",
	))
	contents, err := mod.File("generated.txt").Contents(ctx)
	require.NoError(t, err)
	require.Equal(t, "generated by artifact plan", contents)
}
