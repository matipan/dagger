package core

import (
	"context"
	"errors"

	"dagger.io/dagger"
	"github.com/dagger/testctx"
	"github.com/stretchr/testify/require"
)

const goCollectionModuleSource = `package main

import "strings"

type Test struct{}

func (m *Test) Tests() *GoTests {
	return &GoTests{
		Keys: []string{"unit", "lint", "integration"},
	}
}

type GoTest struct {
	Value string ` + "`json:\"value\"`" + `
}

func (test *GoTest) Name() string {
	return test.Value
}

// +collection
type GoTests struct {
	// +keys
	Keys []string ` + "`json:\"keys\"`" + `
}

// +get
func (tests *GoTests) Lookup(name string) *GoTest {
	return &GoTest{Value: name}
}

func (tests *GoTests) Names() string {
	return strings.Join(tests.Keys, ",")
}
`

const collectionKeysOutput = "unit\nlint\nintegration\n"
const collectionSubsetKeysOutput = "unit\nintegration\n"
const collectionBatchOutput = "unit,integration"

func initStandaloneGoModule(name, source string) dagger.WithContainerFunc {
	return func(ctr *dagger.Container) *dagger.Container {
		return ctr.
			WithNewFile("dagger.toml", "[modules]\n").
			With(daggerExec("sdk", "install", "go")).
			With(daggerExec("-y", "module", "init", "go", name, "--path", ".")).
			WithNewFile("main.go", source).
			With(daggerExec("install", ".")).
			With(daggerExec("-y", "generate"))
	}
}

func (WorkspaceSuite) TestCollectionArtifactsAndPlans(ctx context.Context, t *testctx.T) {
	c := connect(ctx, t)
	mod := workspaceBase(t, c).
		With(initStandaloneDangModule("go", `
# Collection integration fixture.
type Go {
  pub tests: Tests! {
    Tests(names: ["unit", "integration"])
  }
}

type Test {
  pub lint: Void @check {
    null
  }

  pub run: Void @check {
    null
  }
}

type Tests @collection {
  pub names: [String!]! @keys

  new(names: [String!]!) {
    self.names = names
    self
  }

  pub lookup(name: String!): Test! @get {
    Test()
  }

  pub run: Void @check {
    null
  }
}
`))

	t.Run("artifacts api", func(ctx context.Context, t *testctx.T) {
		out, err := mod.With(daggerQuery(`{
			currentWorkspace {
				artifacts {
					dimensions {
						name
						keyType { kind }
					}
					filterCoordinates(dimension: "type", values: ["go-test"]) {
						items {
							coordinates
							coordinate(name: "go-test")
						}
						plan(verb: CHECK) {
							nodes {
								functionPath
								collectionBatched
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
					"dimensions": [{
						"name": "type",
						"keyType": {"kind": "STRING_KIND"}
					}, {
						"name": "go-test",
						"keyType": {"kind": "STRING_KIND"}
					}],
					"filterCoordinates": {
						"items": [{
							"coordinates": ["go-test", "integration"],
							"coordinate": "integration"
						}, {
							"coordinates": ["go-test", "unit"],
							"coordinate": "unit"
						}],
						"plan": {
							"nodes": [{
								"functionPath": ["lint"],
								"collectionBatched": false,
								"target": {"items": [{
									"coordinates": ["go-test", "integration"]
								}]}
							}, {
								"functionPath": ["run"],
								"collectionBatched": true,
								"target": {"items": [{
									"coordinates": ["go-test", "integration"]
								}, {
									"coordinates": ["go-test", "unit"]
								}]}
							}, {
								"functionPath": ["lint"],
								"collectionBatched": false,
								"target": {"items": [{
									"coordinates": ["go-test", "unit"]
								}]}
							}],
							"run": null
						}
					}
				}
			}
		}`, out)
	})

	t.Run("cli filters and aliases", func(ctx context.Context, t *testctx.T) {
		listAll := mod.With(daggerExec("list", "go-test"))
		out, err := listAll.Stdout(ctx)
		if err != nil {
			var execErr *dagger.ExecError
			if errors.As(err, &execErr) {
				require.NoErrorf(
					t,
					err,
					"stdout:\n%s\nstderr:\n%s",
					execErr.Stdout,
					execErr.Stderr,
				)
			}
			require.NoError(t, err)
		}
		require.Equal(t, "integration\nunit\n", out)

		out, err = mod.With(
			daggerExec("list", "go-test", "--go-test=unit"),
		).Stdout(ctx)
		require.NoError(t, err)
		require.Equal(t, "unit\n", out)

		out, err = mod.With(
			daggerExec("list", "go-test", "--go-tests"),
		).Stdout(ctx)
		require.NoError(t, err)
		require.Equal(t, "integration\nunit\n", out)

		listCheck := mod.With(daggerExec("list", "go-test", "--check"))
		out, err = listCheck.Stdout(ctx)
		if err != nil {
			var execErr *dagger.ExecError
			if errors.As(err, &execErr) {
				require.NoErrorf(
					t,
					err,
					"stdout:\n%s\nstderr:\n%s",
					execErr.Stdout,
					execErr.Stderr,
				)
			}
			require.NoError(t, err)
		}
		require.Equal(t, "integration\nunit\n", out)

		out, err = mod.With(
			daggerExec("check", "-l", "--go-test=unit"),
		).Stdout(ctx)
		require.NoError(t, err)
		require.Contains(t, out, "lint")
		require.Contains(t, out, "run")

		out, err = mod.With(
			daggerExec("check", "-l", "--go-tests"),
		).Stdout(ctx)
		require.NoError(t, err)
		require.Contains(t, out, "integration")
		require.Contains(t, out, "unit")

		_, err = mod.With(
			daggerExec("list", "go-test", "--go-tests=false"),
		).Stdout(ctx)
		requireErrOut(t, err, "flag --go-tests does not accept a value")
	})
}
