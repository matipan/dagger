package core

import (
	"context"
	"strings"

	"dagger.io/dagger"
	"github.com/dagger/testctx"
	"github.com/stretchr/testify/require"
)

func initDangModule(name, source string) dagger.WithContainerFunc {
	return func(ctr *dagger.Container) *dagger.Container {
		return ctr.
			With(daggerExec("-y", "module", "init", "dang", name, "--path", "toolchains/"+name)).
			WithNewFile("toolchains/"+name+"/main.dang", source).
			With(daggerExec("install", "./toolchains/"+name))
	}
}

func initStandaloneDangModule(name, source string) dagger.WithContainerFunc {
	return func(ctr *dagger.Container) *dagger.Container {
		return ctr.
			WithNewFile("dagger.toml", "[modules]\n").
			With(daggerExec("sdk", "install", "dang")).
			With(daggerExec("-y", "module", "init", "dang", name, "--path", ".")).
			WithNewFile("main.dang", source).
			With(daggerExec("install", "."))
	}
}

func (WorkspaceSuite) TestArtifacts(ctx context.Context, t *testctx.T) {
	c := connect(ctx, t)

	base := nativeWorkspaceBase(t, c).
		With(daggerExec("sdk", "install", "dang")).
		With(initDangModule("lint", `
type Lint {
  pub report: LintReport!

  new() {
    self.report = LintReport()
    self
  }
}

type LintReport {
  pub summary: String! {
    "clean"
  }
}
`)).
		With(initDangModule("test", `
type Test {
  pub run: String! {
    "passed"
  }
}
`))

	t.Run("api", func(ctx context.Context, t *testctx.T) {
		out, err := base.With(daggerQuery(`{
			currentWorkspace {
				artifacts {
					dimensions {
						name
						keyType { kind }
					}
					items {
						coordinates
						coordinate(name: "type")
						scope {
							dimensions { name }
						}
					}
					filterCoordinates(dimension: "type", values: ["test"]) {
						filterDimension(dimension: "type") {
							items {
								coordinate(name: "type")
							}
						}
					}
				}
			}
			}`)).Stdout(ctx)
		require.NoError(t, err)
		require.JSONEq(t, `{
			"currentWorkspace": {
				"artifacts": {
					"dimensions": [{
						"name": "type",
						"keyType": {"kind": "STRING_KIND"}
					}, {
						"name": "lint-lint-report",
						"keyType": {"kind": "STRING_KIND"}
					}],
					"items": [{
						"coordinates": ["dagger-dang-sdk", null],
						"coordinate": "dagger-dang-sdk",
						"scope": {"dimensions": [
							{"name": "type"},
							{"name": "lint-lint-report"}
						]}
					}, {
						"coordinates": ["lint", null],
						"coordinate": "lint",
						"scope": {"dimensions": [
							{"name": "type"},
							{"name": "lint-lint-report"}
						]}
					}, {
						"coordinates": ["lint-lint-report", "lint:report"],
						"coordinate": "lint-lint-report",
						"scope": {"dimensions": [
							{"name": "type"},
							{"name": "lint-lint-report"}
						]}
					}, {
						"coordinates": ["test", null],
						"coordinate": "test",
						"scope": {"dimensions": [
							{"name": "type"},
							{"name": "lint-lint-report"}
						]}
					}],
					"filterCoordinates": {
						"filterDimension": {
							"items": [{"coordinate": "test"}]
						}
					}
				}
			}
			}`, out)
	})

	t.Run("api rejects invalid filters", func(ctx context.Context, t *testctx.T) {
		for _, test := range []struct {
			name  string
			query string
			err   string
		}{
			{
				name:  "empty coordinates",
				query: `{currentWorkspace{artifacts{filterCoordinates(dimension:"type",values:[]){items{id}}}}}`,
				err:   "values must not be empty",
			},
			{
				name:  "unknown dimension",
				query: `{currentWorkspace{artifacts{filterDimension(dimension:"missing"){items{id}}}}}`,
				err:   `artifact dimension "missing" not found`,
			},
		} {
			t.Run(test.name, func(ctx context.Context, t *testctx.T) {
				_, err := base.With(daggerQuery(test.query)).Stdout(ctx)
				requireErrOut(t, err, test.err)
			})
		}
	})

	t.Run("cli", func(ctx context.Context, t *testctx.T) {
		out, err := base.With(daggerExec("list", "types")).Stdout(ctx)
		require.NoError(t, err)
		require.Equal(t, "dagger-dang-sdk\nlint\nlint-lint-report\ntest\n", out)

		out, err = base.With(daggerExec("list", "types", "--type=test")).Stdout(ctx)
		require.NoError(t, err)
		require.Equal(t, "test\n", out)

		out, err = base.With(daggerExec("list", "--help")).Stdout(ctx)
		require.NoError(t, err)
		require.Equal(t, `Usage:
  dagger list <dimension> [flags]

Available dimensions:
  types     List available artifact types
  lint-lint-report List values for the artifact dimension "lint-lint-report"
`, out)

		out, err = base.With(daggerExec("list", "types", "--help")).Stdout(ctx)
		require.NoError(t, err)
		require.Contains(t, out, "dagger list types [flags]")
		require.Contains(t, out, "--type stringArray")
	})
}

func (WorkspaceSuite) TestGoStaticFieldArtifactsAndTargets(ctx context.Context, t *testctx.T) {
	c := connect(ctx, t)
	mod := workspaceBase(t, c).
		With(initStandaloneGoModule("e2e", `package main

type E2e struct {
	Engine  *TestSuite
	CLI     *TestSuite
	SDKs    *SDKDev
	SDKsARM *SDKDev
}

func New() *E2e {
	return &E2e{
		Engine: &TestSuite{Name: "engine"},
		CLI:    &TestSuite{Name: "cli"},
		SDKs: &SDKDev{
			Go:     &TestSuite{Name: "go"},
			Python: &TestSuite{Name: "python"},
		},
		SDKsARM: &SDKDev{
			Go:     &TestSuite{Name: "go-arm"},
			Python: &TestSuite{Name: "python-arm"},
		},
	}
}

type SDKDev struct {
	Go     *TestSuite
	Python *TestSuite
}

type TestSuite struct {
	Name string
}

// +check
func (suite *TestSuite) Run() error {
	return nil
}

// +check
func (suite *TestSuite) Verify() error {
	return nil
}
`))

	out, err := mod.With(daggerQuery(`{
		currentWorkspace {
			artifacts {
				filterCoordinates(
					dimension: "e2e-test-suite",
					values: ["sdk-dev:go"],
				) {
					items { coordinates }
					plan(verb: CHECK, include: ["verify"]) {
						nodes {
							functionPath
							target { items { coordinates } }
						}
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
						"coordinates": ["e2e-test-suite", "sdk-dev:go", "e2e:sdks"]
					}, {
						"coordinates": ["e2e-test-suite", "sdk-dev:go", "e2e:sdks-arm"]
					}],
					"plan": {
						"nodes": [{
							"functionPath": ["verify"],
							"target": {"items": [{
								"coordinates": ["e2e-test-suite", "sdk-dev:go", "e2e:sdks"]
							}]}
						}, {
							"functionPath": ["verify"],
							"target": {"items": [{
								"coordinates": ["e2e-test-suite", "sdk-dev:go", "e2e:sdks-arm"]
							}]}
						}]
					}
				}
			}
		}
	}`, out)

	for _, args := range [][]string{
		{"check", "--e2e-test-suite=sdk-dev:go", "verify"},
		{"check", "e2e-test-suite:verify"},
		{
			"check",
			"--e2e-sdk-dev=e2e:sdks-arm",
			"--e2e-test-suite=sdk-dev:go",
			"verify",
		},
		{"check", "sdk-dev:go"},
	} {
		out, err = mod.With(daggerExec(args...)).CombinedOutput(ctx)
		require.NoError(t, err, out)
	}

	out, err = mod.With(daggerExec("check", "-l")).Stdout(ctx)
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	require.Contains(
		t,
		lines,
		"--e2e-sdk-dev=e2e:sdks-arm --e2e-test-suite=sdk-dev:go verify",
	)
	require.NotContains(t, lines, "verify")
	require.NotContains(t, lines, "--e2e-test-suite=sdk-dev:go verify")
	require.NotContains(t, lines, "--e2e-sdk-dev=e2e:sdks-arm --e2e-test-suite=sdk-dev:go")
}
