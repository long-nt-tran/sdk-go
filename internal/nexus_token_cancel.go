package internal

import (
	"context"
	"encoding/base64"
	"encoding/json"

	"github.com/nexus-rpc/sdk-go/nexus"
)

// nexusTokenCancelHandler only handles missing operation names. Explicit handlers
// retain their cancellation behavior, including application-specific Update handling.
type nexusTokenCancelHandler struct {
	nexus.UnimplementedOperation[any, any]
}

func (*nexusTokenCancelHandler) Cancel(ctx context.Context, encoded string, _ nexus.CancelOperationOptions) error {
	var token struct {
		Version    int    `json:"v"`
		Type       int    `json:"t"`
		Namespace  string `json:"ns"`
		WorkflowID string `json:"wid"`
		ActivityID string `json:"aid"`
		RunID      string `json:"rid"`
		UpdateID   string `json:"uid"`
	}
	notFound := func() error {
		return nexus.NewHandlerErrorf(nexus.HandlerErrorTypeNotFound, "unknown operation or invalid Temporal operation token")
	}
	data, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return notFound()
	}
	if err := json.Unmarshal(data, &token); err != nil {
		return notFound()
	}
	if token.Version != 0 || token.Namespace == "" || token.Namespace != GetNexusOperationInfo(ctx).Namespace {
		return notFound()
	}
	ctx = context.WithValue(ctx, IsWorkflowRunOpContextKey, true)
	c := GetNexusOperationClient(ctx)
	switch token.Type {
	case 1:
		if token.WorkflowID == "" {
			return notFound()
		}
		return c.CancelWorkflow(ctx, token.WorkflowID, "")
	case 2:
		if token.ActivityID == "" || token.RunID == "" {
			return notFound()
		}
		return c.GetActivityHandle(ClientGetActivityHandleOptions{ActivityID: token.ActivityID, RunID: token.RunID}).Cancel(ctx, ClientCancelActivityOptions{})
	case 3:
		if token.WorkflowID == "" || token.UpdateID == "" {
			return notFound()
		}
		return nexus.NewHandlerErrorf(nexus.HandlerErrorTypeNotImplemented, "Workflow Update cancellation requires an application-specific operation handler")
	default:
		return notFound()
	}
}
