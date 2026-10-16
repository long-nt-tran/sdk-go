package test_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nexus-rpc/sdk-go/nexus"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/temporalnexus"
	"go.temporal.io/sdk/test/nexusclient"
	"go.temporal.io/sdk/worker"
)

func TestNexusLiveDynamicService(t *testing.T) {
	skipOnCloud(t, cloudRequiresLocalServer, "creates a Nexus endpoint")
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	tc := newTestContext(t, ctx)
	service := nexus.NewService("tools")
	service.MustRegister(syncOp)
	w := worker.New(tc.client, tc.taskQueue, worker.Options{})
	w.RegisterNexusService(service)
	require.NoError(t, w.Start())
	t.Cleanup(w.Stop)
	nc := tc.newNexusClient(t, service.Name)
	op := nexus.NewSyncOperation("echo", func(context.Context, string, nexus.StartOperationOptions) (string, error) { return "old", nil })
	require.NoError(t, service.Register(op))
	result, err := nexusclient.ExecuteOperation(ctx, nc, op, "input", nexus.StartOperationOptions{})
	require.NoError(t, err)
	require.Equal(t, "old", result)
	require.NoError(t, service.Update(nexus.NewSyncOperation("echo", func(context.Context, string, nexus.StartOperationOptions) (string, error) { return "new", nil })))
	result, err = nexusclient.ExecuteOperation(ctx, nc, op, "input", nexus.StartOperationOptions{})
	require.NoError(t, err)
	require.Equal(t, "new", result)
	require.NoError(t, service.Delete("echo"))
	_, err = nexusclient.ExecuteOperation(ctx, nc, op, "input", nexus.StartOperationOptions{})
	require.Error(t, err)
	dynamic := temporalnexus.MustNewTemporalOperation(temporalnexus.TemporalOperationOptions[*nexus.LazyValue, any]{
		Name: "fallback",
		Start: func(ctx context.Context, _ temporalnexus.NexusClient, input *nexus.LazyValue, _ temporalnexus.StartTemporalOperationOptions) (temporalnexus.TemporalOperationResult[any], error) {
			var value string
			if err := input.Consume(&value); err != nil {
				return temporalnexus.TemporalOperationResult[any]{}, err
			}
			return temporalnexus.NewSyncResult[any](nexus.ExtractHandlerInfo(ctx).Operation + ":" + value), nil
		},
	})
	require.NoError(t, service.RegisterDynamicOperation(dynamic))
	result, err = nexusclient.ExecuteOperation(ctx, nc, nexus.NewOperationReference[string, string]("unknown"), "input", nexus.StartOperationOptions{})
	require.NoError(t, err)
	require.Equal(t, "unknown:input", result)
}

func TestNexusCancelRemovedOperation(t *testing.T) {
	skipOnCloud(t, cloudRequiresLocalServer, "creates a Nexus endpoint")
	for _, restart := range []bool{false, true} {
		t.Run(map[bool]string{false: "delete", true: "restart"}[restart], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			tc := newTestContext(t, ctx)
			service := nexus.NewService("tools")
			service.MustRegister(workflowOp)
			w := worker.New(tc.client, tc.taskQueue, worker.Options{})
			w.RegisterNexusService(service)
			w.RegisterWorkflow(waitForCancelWorkflow)
			require.NoError(t, w.Start())
			t.Cleanup(w.Stop)
			nc := tc.newNexusClient(t, service.Name)
			id := uuid.NewString()
			result, err := nexusclient.StartOperation(ctx, nc, workflowOp, id, nexus.StartOperationOptions{CallbackURL: "http://localhost/test"})
			require.NoError(t, err)
			require.NotNil(t, result.Pending)
			require.NoError(t, service.Delete(workflowOp.Name()))
			if restart {
				w.Stop()
				fresh := nexus.NewService("tools")

				replacement := worker.New(tc.client, tc.taskQueue, worker.Options{})
				replacement.RegisterNexusService(fresh)
				replacement.RegisterWorkflow(waitForCancelWorkflow)
				require.NoError(t, replacement.Start())
				t.Cleanup(replacement.Stop)
			}
			require.NoError(t, result.Pending.Cancel(ctx, nexus.CancelOperationOptions{}))
			require.ErrorContains(t, tc.client.GetWorkflow(ctx, id, "").Get(ctx, nil), "canceled")
		})
	}
}
