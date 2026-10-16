package internal

import (
	"context"
	"encoding/base64"
	"testing"

	"github.com/nexus-rpc/sdk-go/nexus"
	"github.com/stretchr/testify/require"
)

type tokenCancelClient struct {
	Client
	workflow  string
	activity  ClientGetActivityHandleOptions
	cancelled bool
}

func (c *tokenCancelClient) CancelWorkflow(_ context.Context, id, _ string) error {
	c.workflow = id
	return nil
}
func (c *tokenCancelClient) GetActivityHandle(options ClientGetActivityHandleOptions) ClientActivityHandle {
	c.activity = options
	return &tokenCancelActivity{client: c}
}

type tokenCancelActivity struct {
	ClientActivityHandle
	client *tokenCancelClient
}

func (a *tokenCancelActivity) Cancel(context.Context, ClientCancelActivityOptions) error {
	a.client.cancelled = true
	return nil
}

func TestNexusTokenCancellationFallback(t *testing.T) {
	for _, test := range []struct {
		name, token string
		want        nexus.HandlerErrorType
	}{
		{"workflow", `{"t":1,"ns":"default","wid":"workflow"}`, ""},
		{"activity", `{"t":2,"ns":"default","aid":"activity","rid":"run"}`, ""},
		{"update", `{"t":3,"ns":"default","wid":"workflow","uid":"update"}`, nexus.HandlerErrorTypeNotImplemented},
		{"wrong-namespace", `{"t":1,"ns":"other","wid":"workflow"}`, nexus.HandlerErrorTypeNotFound},
		{"bad-version", `{"t":1,"v":2,"ns":"default","wid":"workflow"}`, nexus.HandlerErrorTypeNotFound},
		{"no-id", `{"t":1,"ns":"default"}`, nexus.HandlerErrorTypeNotFound},
		{"no-run", `{"t":2,"ns":"default","aid":"activity"}`, nexus.HandlerErrorTypeNotFound},
		{"unknown-type", `{"t":9,"ns":"default"}`, nexus.HandlerErrorTypeNotFound},
		{"invalid", `not-json`, nexus.HandlerErrorTypeNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := &tokenCancelClient{}
			nctx := &NexusOperationContext{client: c, Namespace: "default"}
			ctx := ContextWithNexusOperationContext(t.Context(), nctx)
			ctx = context.WithValue(ctx, nexusOperationOutboundInterceptorKey, &nexusOperationEnvironment{})
			registry := nexus.NewServiceRegistry()
			service := nexus.NewService("tools")
			service.MustRegister(nexus.NewSyncOperation("list", func(context.Context, string, nexus.StartOperationOptions) (string, error) { return "", nil }))
			registry.MustRegister(service)
			registry.SetCancelFallback(&nexusTokenCancelHandler{})
			handler, err := registry.NewHandler()
			require.NoError(t, err)
			ctx = nexus.WithHandlerContext(ctx, nexus.HandlerInfo{Service: "tools", Operation: "removed"})
			token := base64.RawURLEncoding.EncodeToString([]byte(test.token))
			err = handler.CancelOperation(ctx, "tools", "removed", token, nexus.CancelOperationOptions{})
			if test.want != "" {
				var handlerError *nexus.HandlerError
				require.ErrorAs(t, err, &handlerError)
				require.Equal(t, test.want, handlerError.Type)
				require.Empty(t, c.workflow)
				require.False(t, c.cancelled)
			} else {
				require.NoError(t, err)
				if test.name == "workflow" {
					require.Equal(t, "workflow", c.workflow)
				} else {
					require.Equal(t, "activity", c.activity.ActivityID)
					require.Equal(t, "run", c.activity.RunID)
					require.True(t, c.cancelled)
				}
			}
		})
	}
}
