package test_test

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nexus-rpc/sdk-go/nexus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/interceptor"
	"go.temporal.io/sdk/temporalnexus"
	"go.temporal.io/sdk/test/nexusclient"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

// TestNexusDynamicTemporalCatchAll checks token-based cancellation after tool removal and worker restart.
func TestNexusDynamicTemporalCatchAll(t *testing.T) {
	skipOnCloud(t, cloudRequiresLocalServer, "creates a Nexus endpoint")
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	tc := newTestContext(t, ctx)
	var available atomic.Bool
	available.Store(true)
	newService := func() *nexus.Service {
		service := nexus.NewService("tools")
		service.MustRegister(listToolsOp)
		require.NoError(t, service.RegisterDynamicOperation(temporalnexus.MustNewDynamicTemporalOperation(
			temporalnexus.DynamicTemporalOperationOptions{
				Start: func(ctx context.Context, nc temporalnexus.NexusClient, input *nexus.LazyValue, opts temporalnexus.StartTemporalOperationOptions) (temporalnexus.TemporalOperationResult[any], error) {
					var id string
					if err := input.Consume(&id); err != nil {
						return temporalnexus.TemporalOperationResult[any]{}, err
					}
					if nexus.ExtractHandlerInfo(ctx).Operation != "wait" || !available.Load() {
						return temporalnexus.TemporalOperationResult[any]{}, nexus.NewHandlerErrorf(nexus.HandlerErrorTypeNotFound, "unknown tool")
					}
					return temporalnexus.StartUntypedWorkflow[any](ctx, nc, client.StartWorkflowOptions{ID: id}, waitForCancelWorkflow, id)
				},
			},
		)))
		return service
	}
	service := newService()
	w := worker.New(tc.client, tc.taskQueue, worker.Options{})
	w.RegisterNexusService(service)
	w.RegisterWorkflow(waitForCancelWorkflow)
	require.NoError(t, w.Start())
	t.Cleanup(w.Stop)
	nc := tc.newNexusClient(t, service.Name)
	wait := nexus.NewOperationReference[string, string]("wait")
	id := uuid.NewString()
	result, err := nexusclient.StartOperation(ctx, nc, wait, id, nexus.StartOperationOptions{CallbackURL: "http://localhost/test"})
	require.NoError(t, err)
	require.NotNil(t, result.Pending)
	available.Store(false)
	_, err = nexusclient.StartOperation(ctx, nc, wait, uuid.NewString(), nexus.StartOperationOptions{CallbackURL: "http://localhost/test"})
	require.Error(t, err)
	w.Stop()
	replacement := worker.New(tc.client, tc.taskQueue, worker.Options{})
	replacement.RegisterNexusService(newService())
	replacement.RegisterWorkflow(waitForCancelWorkflow)
	require.NoError(t, replacement.Start())
	t.Cleanup(replacement.Stop)
	require.NoError(t, result.Pending.Cancel(ctx, nexus.CancelOperationOptions{}))
	require.ErrorContains(t, tc.client.GetWorkflow(ctx, id, "").Get(ctx, nil), "canceled")
}

// dynamicRecorder records what Nexus inbound interceptors see for each call.
type dynamicRecorder struct {
	interceptor.WorkerInterceptorBase
	mu      sync.Mutex
	starts  []string
	cancels []string
}

type dynamicRecordingInbound struct {
	interceptor.NexusOperationInboundInterceptorBase
	root *dynamicRecorder
}

func (r *dynamicRecorder) InterceptNexusOperation(_ context.Context, next interceptor.NexusOperationInboundInterceptor) interceptor.NexusOperationInboundInterceptor {
	return &dynamicRecordingInbound{
		NexusOperationInboundInterceptorBase: interceptor.NexusOperationInboundInterceptorBase{Next: next},
		root:                                 r,
	}
}

func (i *dynamicRecordingInbound) StartOperation(ctx context.Context, input interceptor.NexusStartOperationInput) (nexus.HandlerStartOperationResult[any], error) {
	i.root.mu.Lock()
	i.root.starts = append(i.root.starts, fmt.Sprintf("%s:%s", nexus.ExtractHandlerInfo(ctx).Operation, reflect.TypeOf(input.Input)))
	i.root.mu.Unlock()
	return i.Next.StartOperation(ctx, input)
}

func (i *dynamicRecordingInbound) CancelOperation(ctx context.Context, input interceptor.NexusCancelOperationInput) error {
	i.root.mu.Lock()
	i.root.cancels = append(i.root.cancels, nexus.ExtractHandlerInfo(ctx).Operation)
	i.root.mu.Unlock()
	return i.Next.CancelOperation(ctx, input)
}

func echoWorkflow(_ workflow.Context, value string) (string, error) {
	return value, nil
}

// listToolsOp is the named operation that takes precedence over the dynamic handler.
var listToolsOp = nexus.NewSyncOperation("list_tools", func(context.Context, string, nexus.StartOperationOptions) (string, error) {
	return "manifest", nil
})

func dynamicToolsService(t *testing.T, opts temporalnexus.DynamicTemporalOperationOptions) *nexus.Service {
	service := nexus.NewService("tools")
	service.MustRegister(listToolsOp)
	require.NoError(t, service.RegisterDynamicOperation(temporalnexus.MustNewDynamicTemporalOperation(opts)))
	return service
}

// TestNexusDynamicWorkflowCaller checks named and dynamic dispatch, fallback errors, and the inputs seen by interceptors.
func TestNexusDynamicWorkflowCaller(t *testing.T) {
	skipOnCloud(t, cloudRequiresLocalServer, "creates a Nexus endpoint")
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	tc := newTestContext(t, ctx)
	service := dynamicToolsService(t, temporalnexus.DynamicTemporalOperationOptions{
		Start: func(ctx context.Context, nc temporalnexus.NexusClient, input *nexus.LazyValue, opts temporalnexus.StartTemporalOperationOptions) (temporalnexus.TemporalOperationResult[any], error) {
			var value string
			if err := input.Consume(&value); err != nil {
				return temporalnexus.TemporalOperationResult[any]{}, err
			}
			switch nexus.ExtractHandlerInfo(ctx).Operation {
			case "echo_sync":
				return temporalnexus.NewSyncResult[any]("sync:" + value), nil
			case "echo_workflow":
				return temporalnexus.StartUntypedWorkflow[any](ctx, nc, client.StartWorkflowOptions{ID: uuid.NewString()}, echoWorkflow, value)
			}
			return temporalnexus.TemporalOperationResult[any]{}, nexus.NewHandlerErrorf(nexus.HandlerErrorTypeNotFound, "unknown tool")
		},
	})
	callerWorkflow := func(ctx workflow.Context, operation, value string) (string, error) {
		var result string
		err := workflow.NewNexusClient(tc.endpoint, "tools").
			ExecuteOperation(ctx, operation, value, workflow.NexusOperationOptions{}).
			Get(ctx, &result)
		return result, err
	}
	recorder := &dynamicRecorder{}
	w := worker.New(tc.client, tc.taskQueue, worker.Options{Interceptors: []interceptor.WorkerInterceptor{recorder}})
	w.RegisterNexusService(service)
	w.RegisterWorkflow(echoWorkflow)
	w.RegisterWorkflow(callerWorkflow)
	require.NoError(t, w.Start())
	t.Cleanup(w.Stop)

	run := func(t *testing.T, operation, value string) (string, error) {
		t.Helper()
		wr, err := tc.client.ExecuteWorkflow(ctx, client.StartWorkflowOptions{TaskQueue: tc.taskQueue}, callerWorkflow, operation, value)
		require.NoError(t, err)
		var result string
		err = wr.Get(ctx, &result)
		return result, err
	}
	for _, tc := range []struct {
		operation    string
		want         string
		inputType    string
		wantNotFound bool
	}{
		{operation: "list_tools", want: "manifest", inputType: "string"},
		{operation: "echo_sync", want: "sync:hello", inputType: "*nexus.LazyValue"},
		{operation: "echo_workflow", want: "hello", inputType: "*nexus.LazyValue"},
		{operation: "missing", inputType: "*nexus.LazyValue", wantNotFound: true},
	} {
		t.Run(tc.operation, func(t *testing.T) {
			got, err := run(t, tc.operation, "hello")
			if tc.wantNotFound {
				var handlerErr *nexus.HandlerError
				require.ErrorAs(t, err, &handlerErr)
				require.Equal(t, nexus.HandlerErrorTypeNotFound, handlerErr.Type)
			} else {
				require.NoError(t, err)
				require.Equal(t, tc.want, got)
			}
			recorder.mu.Lock()
			defer recorder.mu.Unlock()
			require.Contains(t, recorder.starts, tc.operation+":"+tc.inputType)
		})
	}
}

func waitForCancelActivity(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
			activity.RecordHeartbeat(ctx)
		}
	}
}

// TestNexusDynamicActivityCancel checks that cancellation reaches the backing Activity and Nexus interceptors.
func TestNexusDynamicActivityCancel(t *testing.T) {
	skipOnCloud(t, cloudRequiresLocalServer, "creates a Nexus endpoint")
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	tc := newTestContext(t, ctx)
	service := dynamicToolsService(t, temporalnexus.DynamicTemporalOperationOptions{
		Start: func(ctx context.Context, nc temporalnexus.NexusClient, input *nexus.LazyValue, opts temporalnexus.StartTemporalOperationOptions) (temporalnexus.TemporalOperationResult[any], error) {
			var id string
			if err := input.Consume(&id); err != nil {
				return temporalnexus.TemporalOperationResult[any]{}, err
			}
			return temporalnexus.StartUntypedActivity[any](ctx, nc, client.StartActivityOptions{
				ID:                  id,
				TaskQueue:           tc.taskQueue,
				StartToCloseTimeout: 30 * time.Second,
				HeartbeatTimeout:    time.Second,
			}, waitForCancelActivity)
		},
	})
	recorder := &dynamicRecorder{}
	w := worker.New(tc.client, tc.taskQueue, worker.Options{Interceptors: []interceptor.WorkerInterceptor{recorder}})
	w.RegisterNexusService(service)
	w.RegisterActivity(waitForCancelActivity)
	require.NoError(t, w.Start())
	t.Cleanup(w.Stop)

	nc := tc.newNexusClient(t, service.Name)
	id := uuid.NewString()
	result, err := nexusclient.StartOperation(ctx, nc, nexus.NewOperationReference[string, any]("wait_activity"), id, nexus.StartOperationOptions{CallbackURL: "http://localhost/test"})
	require.NoError(t, err)
	require.NotNil(t, result.Pending)
	require.NoError(t, result.Pending.Cancel(ctx, nexus.CancelOperationOptions{}))

	handle := tc.client.GetActivityHandle(client.GetActivityHandleOptions{ActivityID: id})
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		desc, err := handle.Describe(ctx, client.DescribeActivityOptions{})
		require.NoError(c, err)
		require.Equal(c, enumspb.ACTIVITY_EXECUTION_STATUS_CANCELED, desc.Status)
	}, 20*time.Second, 200*time.Millisecond)

	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	require.Equal(t, []string{"wait_activity:*nexus.LazyValue"}, recorder.starts)
	require.Equal(t, []string{"wait_activity"}, recorder.cancels)
}

const dynamicBlockingUpdate = "block"

func dynamicUpdateTargetWorkflow(ctx workflow.Context) error {
	if err := workflow.SetUpdateHandler(ctx, dynamicBlockingUpdate, func(ctx workflow.Context) error {
		return workflow.Await(ctx, func() bool { return false })
	}); err != nil {
		return err
	}
	return workflow.Await(ctx, func() bool { return false })
}

// TestNexusDynamicUpdateCustomCancel checks that Update token identifiers reach the application cancellation handler.
func TestNexusDynamicUpdateCustomCancel(t *testing.T) {
	skipOnCloud(t, cloudRequiresLocalServer, "creates a Nexus endpoint")
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	tc := newTestContext(t, ctx)
	canceled := make(chan temporalnexus.CancelTemporalUpdateWorkflowOptions, 1)
	service := dynamicToolsService(t, temporalnexus.DynamicTemporalOperationOptions{
		Start: func(ctx context.Context, nc temporalnexus.NexusClient, input *nexus.LazyValue, opts temporalnexus.StartTemporalOperationOptions) (temporalnexus.TemporalOperationResult[any], error) {
			var workflowID string
			if err := input.Consume(&workflowID); err != nil {
				return temporalnexus.TemporalOperationResult[any]{}, err
			}
			return temporalnexus.StartUpdateWorkflow[any](ctx, nc, client.UpdateWorkflowOptions{
				WorkflowID:   workflowID,
				UpdateName:   dynamicBlockingUpdate,
				WaitForStage: client.WorkflowUpdateStageAccepted,
			})
		},
		CancelWorkflowUpdate: func(_ context.Context, _ client.Client, opts temporalnexus.CancelTemporalUpdateWorkflowOptions, _ nexus.CancelOperationOptions) error {
			canceled <- opts
			return nil
		},
	})
	w := worker.New(tc.client, tc.taskQueue, worker.Options{})
	w.RegisterNexusService(service)
	w.RegisterWorkflow(dynamicUpdateTargetWorkflow)
	require.NoError(t, w.Start())
	t.Cleanup(w.Stop)

	workflowID := "dynamic-update-" + uuid.NewString()
	run, err := tc.client.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: workflowID, TaskQueue: tc.taskQueue}, dynamicUpdateTargetWorkflow)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tc.client.TerminateWorkflow(context.Background(), workflowID, run.GetRunID(), "test done") })

	nc := tc.newNexusClient(t, service.Name)
	result, err := nexusclient.StartOperation(ctx, nc, nexus.NewOperationReference[string, any]("block"), workflowID, nexus.StartOperationOptions{CallbackURL: "http://localhost/test"})
	require.NoError(t, err)
	require.NotNil(t, result.Pending)
	require.NoError(t, result.Pending.Cancel(ctx, nexus.CancelOperationOptions{}))
	select {
	case opts := <-canceled:
		require.Equal(t, workflowID, opts.WorkflowID)
		require.NotEmpty(t, opts.UpdateID)
	case <-ctx.Done():
		t.Fatal("custom Workflow Update cancel handler was not called")
	}
}
