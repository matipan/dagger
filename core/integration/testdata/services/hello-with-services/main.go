// A module for HelloWithServices functions
package main

import (
	"context"
	"sort"
	"strings"

	"dagger/hello-with-services/internal/dagger"
)

type HelloWithServices struct{}

// Returns a web server service
// +up
func (m *HelloWithServices) Web() *dagger.Service {
	return dag.Container().
		From("nginx:alpine").
		WithExposedPort(80).
		AsService()
}

// Returns a redis service
// +up
func (m *HelloWithServices) Redis() *dagger.Service {
	return dag.Container().
		From("redis:alpine").
		WithExposedPort(6379).
		AsService()
}

// Returns the names of all services visible in the current UP plan.
func (m *HelloWithServices) CurrentPlanServices(ctx context.Context, ws *dagger.Workspace) ([]string, error) {
	actions, err := ws.
		Artifacts().
		Plan(dagger.VerbUp).
		Nodes(ctx)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(actions))
	for _, action := range actions {
		path, err := action.FunctionPath(ctx)
		if err != nil {
			return nil, err
		}
		targets, err := action.Target().Items(ctx)
		if err != nil {
			return nil, err
		}
		if len(targets) != 1 {
			continue
		}
		coordinates, err := targets[0].Coordinates(ctx)
		if err != nil {
			return nil, err
		}
		names = append(names, strings.Join(append(coordinates, path...), ":"))
	}
	sort.Strings(names)
	return names, nil
}

func (m *HelloWithServices) Infra() *Infra {
	return &Infra{}
}

type Infra struct{}

// +up
func (i *Infra) Database() *dagger.Service {
	return dag.Container().
		From("postgres:alpine").
		WithEnvVariable("POSTGRES_PASSWORD", "test").
		WithExposedPort(5432).
		AsService()
}
