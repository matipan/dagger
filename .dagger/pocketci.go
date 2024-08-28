package main

import (
	"context"
	"slices"

	"github.com/dagger/dagger/.dagger/internal/dagger"
)

func (dev *DaggerDev) OnGithubPullRequest(ctx context.Context, filter string, src *dagger.Directory, eventTrigger *dagger.File) error {
	// only run the pipeline if the PR was opened, pushed to or reopened
	if !slices.Contains([]string{"opened", "synchronize", "reopened"}, filter) {
		return nil
	}

	return dev.check(ctx)
}

func (dev *DaggerDev) OnGithubPushMain(ctx context.Context, src *dagger.Directory, eventTrigger *dagger.File) error {
	return dev.check(ctx)
}

func (dev *DaggerDev) check(ctx context.Context) error {
	sdk := dev.SDK().All()
	err := sdk.Lint(ctx)
	if err != nil {
		return err
	}
	return sdk.Test(ctx)
}
