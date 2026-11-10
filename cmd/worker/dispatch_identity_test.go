package main

import (
	"context"
	"testing"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"
)

func TestADispatchIdentityNamesWorkflowRunAndActivity(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	probe := func(ctx context.Context) (string, error) {
		id, ok := dispatchIdentity(ctx)
		if !ok {
			t.Fatal("an activity context must yield an identity")
		}
		info := activity.GetInfo(ctx)
		want := info.WorkflowExecution.ID + "/" + info.WorkflowExecution.RunID + "/" + info.ActivityID
		if id != want {
			t.Fatalf("got %q, want %q", id, want)
		}
		return id, nil
	}
	env.RegisterActivity(probe)
	if _, err := env.ExecuteActivity(probe); err != nil {
		t.Fatal(err)
	}
}

func TestAContextOutsideAnActivityHasNoDispatchIdentity(t *testing.T) {
	if id, ok := dispatchIdentity(context.Background()); ok || id != "" {
		t.Fatalf("got %q %v", id, ok)
	}
}
