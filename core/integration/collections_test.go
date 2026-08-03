package core

import (
	"context"
	"encoding/json"
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
		modulePath := "toolchains/" + name
		return ctr.
			WithNewFile("dagger.toml", "[modules]\n").
			WithNewFile(
				modulePath+"/dagger.json",
				`{"name":"`+name+`","engineVersion":"latest","sdk":{"source":"go"}}`,
			).
			WithNewFile(modulePath+"/main.go", source).
			With(daggerExec("install", "./"+modulePath))
	}
}

func (WorkspaceSuite) TestCollectionArtifactsAndPlans(ctx context.Context, t *testctx.T) {
	c := connect(ctx, t)
	mod := workspaceBase(t, c).
		With(initStandaloneDangModule("go", `
# Collection integration fixture.
type Go {
  pub engine: Test!

  new() {
    self.engine = Test()
    self
  }

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
							"coordinates": ["go-test", "go:engine"],
							"coordinate": "go:engine"
						}, {
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
									"coordinates": ["go-test", "go:engine"]
								}]}
							}, {
								"functionPath": ["run"],
								"collectionBatched": false,
								"target": {"items": [{
									"coordinates": ["go-test", "go:engine"]
								}]}
							}, {
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

		out, err = mod.With(daggerQuery(`{
			currentWorkspace {
				artifacts {
					filterCoordinates(dimension: "type", values: ["go-test"]) {
						filterCoordinates(dimension: "go-test", values: ["unit"]) {
							plan(verb: CHECK, include: ["go-test:run"]) {
								nodes {
									functionPath
									collectionBatched
									target { items { coordinates } }
								}
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
						"filterCoordinates": {
							"plan": {
								"nodes": [{
									"functionPath": ["run"],
									"collectionBatched": false,
									"target": {"items": [{
										"coordinates": ["go-test", "unit"]
									}]}
								}]
							}
						}
					}
				}
			}
		}`, out)
	})

	t.Run("nested collection coordinates", func(ctx context.Context, t *testctx.T) {
		nested := workspaceBase(t, c).
			With(initStandaloneDangModule("go", `
type Go {
  pub modules: Modules! {
    Modules(paths: ["api"])
  }
}

type Modules @collection {
  pub paths: [String!]! @keys

  new(paths: [String!]!) {
    self.paths = paths
    self
  }

  pub module(path: String!): Module! @get {
    Module(path: path)
  }
}

type Module {
  pub path: String!

  new(path: String!) {
    self.path = path
    self
  }

  pub directories: Directories! {
    Directories(modulePath: path, paths: [path + "/auth"])
  }
}

type Directories @collection {
  pub paths: [String!]! @keys
  let modulePath: String!

  new(modulePath: String!, paths: [String!]!) {
    self.modulePath = modulePath
    self.paths = paths
    self
  }

  pub directory(path: String!): Directory! @get {
    Directory(modulePath: modulePath, path: path)
  }
}

type Directory {
  let modulePath: String!
  pub path: String!

  new(modulePath: String!, path: String!) {
    self.modulePath = modulePath
    self.path = path
    self
  }

  pub tests: Tests! {
    Tests(modulePath: modulePath, directoryPath: path, names: ["TestAuth"])
  }
}

type Tests @collection {
  pub names: [String!]! @keys
  let modulePath: String!
  let directoryPath: String!

  new(modulePath: String!, directoryPath: String!, names: [String!]!) {
    self.modulePath = modulePath
    self.directoryPath = directoryPath
    self.names = names
    self
  }

  pub test(name: String!): Test! @get {
    Test(name: name)
  }
}

type Test {
  pub name: String!

  new(name: String!) {
    self.name = name
    self
  }

  pub execute: Void @check {
    null
  }
}
`))

		out, err := nested.With(daggerQuery(`{
			currentWorkspace {
				artifacts {
					dimensions { name }
					filterCoordinates(dimension: "go-test", values: ["TestAuth"]) {
						items { coordinates coordinateDimensions }
						plan(verb: CHECK) {
							nodes {
								functionPath
								target { items { coordinates coordinateDimensions } }
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
					"dimensions": [
						{"name": "type"},
						{"name": "go-module"},
						{"name": "go-directory"},
						{"name": "go-test"}
					],
					"filterCoordinates": {
						"items": [{
							"coordinates": ["go-test", "api", "api/auth", "TestAuth"],
							"coordinateDimensions": ["type", "go-module", "go-directory", "go-test"]
						}],
						"plan": {"nodes": [{
							"functionPath": ["execute"],
							"target": {"items": [{
								"coordinates": ["go-test", "api", "api/auth", "TestAuth"],
								"coordinateDimensions": ["type", "go-module", "go-directory", "go-test"]
							}]}
						}]}
					}
				}
			}
		}`, out)

		out, err = nested.With(daggerExec("check", "-l")).Stdout(ctx)
		require.NoError(t, err)
		require.Equal(t, `# Empty target runs everything. Otherwise use:
--go-module=api --go-directory=api/auth --go-test=TestAuth go-test:execute
`, out)
	})

	t.Run("identical keys in separate occurrences", func(ctx context.Context, t *testctx.T) {
		duplicateOccurrences := workspaceBase(t, c).
			With(initStandaloneDangModule("go", `
type Go {
  pub first: Tests! {
    Tests(names: ["unit"])
  }

  pub second: Tests! {
    Tests(names: ["unit"])
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

		out, err := duplicateOccurrences.With(daggerQuery(`{
			currentWorkspace {
				artifacts {
					filterCoordinates(dimension: "go-test", values: ["unit"]) {
						items {
							coordinate(name: "go-test")
						}
						plan(verb: CHECK) {
							nodes {
								collectionBatched
							}
							run
						}
					}
				}
			}
		}`)).Stdout(ctx)
		require.NoError(t, err)

		var result struct {
			CurrentWorkspace struct {
				Artifacts struct {
					FilterCoordinates struct {
						Items []struct {
							Coordinate string `json:"coordinate"`
						} `json:"items"`
						Plan struct {
							Nodes []struct {
								CollectionBatched bool `json:"collectionBatched"`
							} `json:"nodes"`
						} `json:"plan"`
					} `json:"filterCoordinates"`
				} `json:"artifacts"`
			} `json:"currentWorkspace"`
		}
		require.NoError(t, json.Unmarshal([]byte(out), &result))

		filtered := result.CurrentWorkspace.Artifacts.FilterCoordinates
		require.Len(t, filtered.Items, 2)
		for _, item := range filtered.Items {
			require.Equal(t, "unit", item.Coordinate)
		}
		require.Len(t, filtered.Plan.Nodes, 4)
		batched := 0
		for _, node := range filtered.Plan.Nodes {
			if node.CollectionBatched {
				batched++
			}
		}
		require.Zero(t, batched)

		out, err = duplicateOccurrences.With(daggerExec("check", "-l")).Stdout(ctx)
		require.NoError(t, err)
		require.Equal(t, "# Empty target runs everything. Otherwise use:\n", out)
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
		require.Equal(t, "go:engine\nintegration\nunit\n", out)

		out, err = mod.With(
			daggerExec("list", "go-test", "--go-test=unit"),
		).Stdout(ctx)
		require.NoError(t, err)
		require.Equal(t, "unit\n", out)

		out, err = mod.With(
			daggerExec("list", "go-test", "--go-tests"),
		).Stdout(ctx)
		require.NoError(t, err)
		require.Equal(t, "go:engine\nintegration\nunit\n", out)

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
		require.Equal(t, "go:engine\nintegration\nunit\n", out)

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
		require.Contains(t, out, "go:engine")
		require.Contains(t, out, "integration")
		require.Contains(t, out, "unit")

		out, err = mod.With(
			daggerExec("check", "go-test:run"),
		).CombinedOutput(ctx)
		require.NoError(t, err, out)

		_, err = mod.With(
			daggerExec("list", "go-test", "--go-tests=false"),
		).Stdout(ctx)
		requireErrOut(t, err, "flag --go-tests does not accept a value")
	})
}
