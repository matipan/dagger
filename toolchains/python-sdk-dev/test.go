package main

import (
	"context"

	"dagger/python-sdk-dev/internal/dagger"
)

// PythonTestMatrix is a reusable test matrix keyed by Python version.
// +collection
type PythonTestMatrix struct {
	// Python versions in this matrix
	// +keys
	Versions []string

	// The base container to run the tests
	// +private
	Container *dagger.Container
}

func newPythonTestMatrix(container *dagger.Container, versions []string) *PythonTestMatrix {
	return &PythonTestMatrix{
		Versions:  append([]string(nil), versions...),
		Container: container,
	}
}

// Get a test suite for one Python version.
// +get
func (matrix *PythonTestMatrix) Get(version string) *PythonTest {
	return &PythonTest{
		Container: matrix.Container,
		Version:   version,
	}
}

// PythonTest runs a project's tests with one Python version.
type PythonTest struct {
	// The base container to run the tests
	// +private
	Container *dagger.Container
	// The Python version to test against
	Version string
}

// Run Python slow tests.
// +check
func (t *PythonTest) Slow(ctx context.Context) error {
	return t.Run(ctx, []string{"-Wd", "-l", "-m", "slow and not provision"})
}

// Run Python unit tests.
// +check
func (t *PythonTest) Unit(ctx context.Context) error {
	return t.Run(ctx, []string{"-m", "not slow and not provision"})
}

// Run the pytest command.
func (t *PythonTest) Run(
	ctx context.Context,
	// Arguments to pass to pytest
	args []string,
) error {
	return dag.Pytest(dagger.PytestOpts{
		Container: t.Container,
		Source:    t.Container.Directory("/src/sdk/python"),
	}).Test(ctx, dagger.PytestTestOpts{
		Version: t.Version,
		Args:    args,
	})
}
