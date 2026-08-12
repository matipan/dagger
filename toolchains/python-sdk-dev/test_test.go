package main

import (
	"slices"
	"testing"
)

func TestValidatePythonVersions(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name     string
		versions []string
		err      string
	}{
		{
			name:     "valid",
			versions: []string{"3.14", "3.13", "3.12", "3.11", "3.10"},
		},
		{
			name: "empty",
			err:  "python versions must not be empty",
		},
		{
			name:     "blank",
			versions: []string{"3.13", ""},
			err:      `invalid Python version ""`,
		},
		{
			name:     "surrounding whitespace",
			versions: []string{" 3.13"},
			err:      `invalid Python version " 3.13"`,
		},
		{
			name:     "duplicate",
			versions: []string{"3.13", "3.13"},
			err:      `duplicate Python version "3.13"`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validatePythonVersions(test.versions)
			if test.err == "" {
				if err != nil {
					t.Fatalf("validate versions: %v", err)
				}
				return
			}
			if err == nil || err.Error() != test.err {
				t.Fatalf("got error %v, want %q", err, test.err)
			}
		})
	}
}

func TestPythonTestMatrix(t *testing.T) {
	t.Parallel()

	versions := []string{"3.13", "3.12"}
	matrix := newPythonTestMatrix(nil, versions)
	versions[0] = "changed"

	if !slices.Equal(matrix.Versions, []string{"3.13", "3.12"}) {
		t.Fatalf("matrix versions = %v", matrix.Versions)
	}
	if version := matrix.Get("3.12").Version; version != "3.12" {
		t.Fatalf("test version = %q, want 3.12", version)
	}
}
