package temporalnexus_test

import (
	"context"
	"testing"

	"github.com/nexus-rpc/sdk-go/nexus"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/temporalnexus"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"
)

func TestNexusDynamicWorkflowTestEnvironment(t *testing.T) {
	service := nexus.NewService("tools")
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
	caller := func(ctx workflow.Context) (string, error) {
		var value string
		err := workflow.NewNexusClient("endpoint", "tools").ExecuteOperation(ctx, "echo", "input", workflow.NexusOperationOptions{}).Get(ctx, &value)
		return value, err
	}
	suite := testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterNexusService(service)
	started := false
	env.SetOnNexusOperationStartedListener(func(service, operation string, input converter.EncodedValue) {
		require.Equal(t, "tools", service)
		require.Equal(t, "echo", operation)
		var value string
		require.NoError(t, input.Get(&value))
		require.Equal(t, "input", value)
		started = true
	})
	env.ExecuteWorkflow(caller)
	require.True(t, started)
	require.NoError(t, env.GetWorkflowError())
	var result string
	require.NoError(t, env.GetWorkflowResult(&result))
	require.Equal(t, "echo:input", result)
}
