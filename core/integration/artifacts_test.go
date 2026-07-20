package core

import (
	"context"

	"dagger.io/dagger"
	"github.com/dagger/testctx"
	"github.com/stretchr/testify/require"
)

func initDangModule(name, source string) dagger.WithContainerFunc {
	return func(ctr *dagger.Container) *dagger.Container {
		return ctr.
			WithNewFile("dagger.toml", "[modules]\n").
			With(daggerExec("sdk", "install", "dang")).
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

	base := workspaceBase(t, c).
		With(initDangModule("lint", `
type Lint {
  pub report: LintReport! {
    LintReport()
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

  pub ownArtifacts: Artifacts! {
    currentWorkspace.artifacts.filterCoordinates(
      dimension: "type",
      values: ["test"],
    )
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
			test {
				ownArtifacts {
					items {
						coordinate(name: "type")
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
					}],
					"items": [{
						"coordinates": ["lint"],
						"coordinate": "lint",
						"scope": {"dimensions": [{"name": "type"}]}
					}, {
						"coordinates": ["test"],
						"coordinate": "test",
						"scope": {"dimensions": [{"name": "type"}]}
					}, {
						"coordinates": ["work"],
						"coordinate": "work",
						"scope": {"dimensions": [{"name": "type"}]}
					}],
					"filterCoordinates": {
						"filterDimension": {
							"items": [{"coordinate": "test"}]
						}
					}
				}
			},
			"test": {
				"ownArtifacts": {
					"items": [{"coordinate": "test"}]
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
		require.Equal(t, "lint\ntest\nwork\n", out)

		out, err = base.With(daggerExec("list", "types", "--type=test")).Stdout(ctx)
		require.NoError(t, err)
		require.Equal(t, "test\n", out)

		out, err = base.With(daggerExec("list", "--help")).Stdout(ctx)
		require.NoError(t, err)
		require.Equal(t, `Usage:
  dagger list <dimension> [flags]

Available dimensions:
  types     List available artifact types
`, out)

		out, err = base.With(daggerExec("list", "types", "--help")).Stdout(ctx)
		require.NoError(t, err)
		require.Contains(t, out, "dagger list types [flags]")
		require.Contains(t, out, "--type stringArray")
	})
}
