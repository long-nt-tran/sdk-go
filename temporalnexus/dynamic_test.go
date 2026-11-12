package temporalnexus_test

import (
	"context"
	"testing"

	"github.com/nexus-rpc/sdk-go/nexus"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/temporalnexus"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"
)

// TestNexusDynamicWorkflowTestEnvironment checks named precedence and fallback dispatch with the original name and input.
func TestNexusDynamicWorkflowTestEnvironment(t *testing.T) {
	for _, tc := range []struct{ operation, want string }{
		{"named", "static"},
		{"echo", "echo:input"},
		{"another", "another:input"},
	} {
		t.Run(tc.operation, func(t *testing.T) {
			service := newDynamicService(t, func(ctx context.Context, _ temporalnexus.NexusClient, input *nexus.LazyValue, opts temporalnexus.StartTemporalOperationOptions) (temporalnexus.TemporalOperationResult[any], error) {
				require.NotEmpty(t, opts.RequestID)
				var value string
				if err := input.Consume(&value); err != nil {
					return temporalnexus.TemporalOperationResult[any]{}, err
				}
				return temporalnexus.NewSyncResult[any](nexus.ExtractHandlerInfo(ctx).Operation + ":" + value), nil
			})
			service.MustRegister(nexus.NewSyncOperation("named", func(context.Context, string, nexus.StartOperationOptions) (string, error) {
				return "static", nil
			}))
			caller := func(ctx workflow.Context) (string, error) {
				var value string
				err := workflow.NewNexusClient("endpoint", "tools").ExecuteOperation(ctx, tc.operation, "input", workflow.NexusOperationOptions{}).Get(ctx, &value)
				return value, err
			}
			suite := testsuite.WorkflowTestSuite{}
			env := suite.NewTestWorkflowEnvironment()
			env.RegisterNexusService(service)
			started := false
			env.SetOnNexusOperationStartedListener(func(service, operation string, input converter.EncodedValue) {
				require.Equal(t, "tools", service)
				require.Equal(t, tc.operation, operation)
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
			require.Equal(t, tc.want, result)
			if tc.operation != "named" {
				require.Nil(t, service.Operation(tc.operation))
			}
		})
	}
}

func newDynamicService(t *testing.T, start func(context.Context, temporalnexus.NexusClient, *nexus.LazyValue, temporalnexus.StartTemporalOperationOptions) (temporalnexus.TemporalOperationResult[any], error)) *nexus.Service {
	service := nexus.NewService("tools")
	require.NoError(t, service.RegisterDynamicOperation(temporalnexus.MustNewDynamicTemporalOperation(
		temporalnexus.DynamicTemporalOperationOptions{Start: start},
	)))
	return service
}

// TestNexusDynamicWorkflowTestEnvironmentMock checks that a typed mock overrides the fallback and an untyped name gives a useful error.
func TestNexusDynamicWorkflowTestEnvironmentMock(t *testing.T) {
	service := newDynamicService(t, func(context.Context, temporalnexus.NexusClient, *nexus.LazyValue, temporalnexus.StartTemporalOperationOptions) (temporalnexus.TemporalOperationResult[any], error) {
		return temporalnexus.TemporalOperationResult[any]{}, nexus.NewHandlerErrorf(nexus.HandlerErrorTypeInternal, "dynamic handler must not run for a mocked name")
	})
	caller := func(ctx workflow.Context) (string, error) {
		var value string
		err := workflow.NewNexusClient("endpoint", "tools").ExecuteOperation(ctx, "echo", "input", workflow.NexusOperationOptions{}).Get(ctx, &value)
		return value, err
	}
	suite := testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterNexusService(service)
	require.PanicsWithValue(t,
		`operation "echo" in service "tools" is served by the dynamic operation handler; mock it with nexus.NewOperationReference[I, O]("echo") to set its input and output types`,
		func() { env.OnNexusOperation(service, "echo", "input", mock.Anything) },
	)
	env.OnNexusOperation(service, nexus.NewOperationReference[string, string]("echo"), "input", mock.Anything).
		Return(&nexus.HandlerStartOperationResultSync[string]{Value: "mocked"}, nil)
	env.ExecuteWorkflow(caller)
	require.NoError(t, env.GetWorkflowError())
	var result string
	require.NoError(t, env.GetWorkflowResult(&result))
	require.Equal(t, "mocked", result)
}

func waitForCancel(ctx workflow.Context, _ string) (string, error) {
	return "", workflow.Await(ctx, func() bool { return false })
}

// TestNexusDynamicWorkflowTestEnvironmentCancel checks that the test environment cancels the Workflow started by the fallback.
func TestNexusDynamicWorkflowTestEnvironmentCancel(t *testing.T) {
	service := newDynamicService(t, func(ctx context.Context, nc temporalnexus.NexusClient, input *nexus.LazyValue, _ temporalnexus.StartTemporalOperationOptions) (temporalnexus.TemporalOperationResult[any], error) {
		var id string
		if err := input.Consume(&id); err != nil {
			return temporalnexus.TemporalOperationResult[any]{}, err
		}
		return temporalnexus.StartUntypedWorkflow[any](ctx, nc, client.StartWorkflowOptions{ID: id}, waitForCancel, id)
	})
	caller := func(ctx workflow.Context) error {
		ctx, cancel := workflow.WithCancel(ctx)
		fut := workflow.NewNexusClient("endpoint", "tools").ExecuteOperation(ctx, "wait", "backing-id", workflow.NexusOperationOptions{})
		var exec workflow.NexusOperationExecution
		if err := fut.GetNexusOperationExecution().Get(ctx, &exec); err != nil {
			return err
		}
		cancel()
		return fut.Get(ctx, nil)
	}
	suite := testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(waitForCancel)
	env.RegisterNexusService(service)
	env.ExecuteWorkflow(caller)
	require.True(t, env.IsWorkflowCompleted())
	var canceledErr *temporal.CanceledError
	require.ErrorAs(t, env.GetWorkflowError(), &canceledErr)
}

// TestNexusDynamicWorkflowTestEnvironmentError checks that fallback errors reach the caller without becoming missing-operation panics.
func TestNexusDynamicWorkflowTestEnvironmentError(t *testing.T) {
	for _, errorType := range []nexus.HandlerErrorType{nexus.HandlerErrorTypeNotFound, nexus.HandlerErrorTypeBadRequest} {
		t.Run(string(errorType), func(t *testing.T) {
			service := newDynamicService(t, func(context.Context, temporalnexus.NexusClient, *nexus.LazyValue, temporalnexus.StartTemporalOperationOptions) (temporalnexus.TemporalOperationResult[any], error) {
				return temporalnexus.TemporalOperationResult[any]{}, nexus.NewHandlerErrorf(errorType, "rejected by fallback")
			})
			caller := func(ctx workflow.Context) error {
				return workflow.NewNexusClient("endpoint", "tools").ExecuteOperation(ctx, "reject", "input", workflow.NexusOperationOptions{}).Get(ctx, nil)
			}
			suite := testsuite.WorkflowTestSuite{}
			env := suite.NewTestWorkflowEnvironment()
			env.RegisterNexusService(service)
			env.ExecuteWorkflow(caller)
			require.True(t, env.IsWorkflowCompleted())
			var handlerErr *nexus.HandlerError
			require.ErrorAs(t, env.GetWorkflowError(), &handlerErr)
			require.Equal(t, errorType, handlerErr.Type)
			require.Equal(t, "rejected by fallback", handlerErr.Message)
		})
	}
}
