package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/mac-lucky/pushward-mcp/internal/client"
)

// Receipts of acknowledged notifications: create_notification with
// acknowledge repeats the push until someone taps an action without a url,
// and its response carries a receipt to follow here.
//
// Hand-written rather than generated (see skipOperations): the read takes
// ?wait=, the cancel by id is a POST without a body, and a generated POST
// would be annotated as non-destructive although it stops an alert.

const receiptScope = " An integration key reaches only the notifications it sent; the app token, the whole account."

func registerReceiptTools(s *mcpserver.MCPServer, api *client.APIClient) {
	s.AddTool(
		mcp.NewTool("get_notification_receipt",
			mcp.WithDescription("Read the receipt of a notification sent with acknowledge (create_notification returned it as receipt): status active (still repeating) | acknowledged | expired | canceled, repeats_sent, expires_at, acknowledged_at, acknowledged_by, acknowledged_by_device, action_id (the action that acknowledged it; pw_ack is the button the server adds), cancel_reason, tags, and the callback delivery. wait_seconds holds the request while it is active; for longer waits use wait_for_ack. Finished receipts are kept 7 days."+receiptScope),
			mcp.WithReadOnlyHintAnnotation(true),
			mcp.WithOpenWorldHintAnnotation(true),
			mcp.WithNumber("notification_id", mcp.Required(), mcp.Description("Notification id from create_notification, or notification_id of a sent scheduled notification")),
			mcp.WithNumber("wait_seconds", mcp.Description("Seconds to wait while the receipt is active (default 0) (min: 0, max: 25)"), mcp.Min(0), mcp.Max(25)),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return handleGetNotificationReceipt(ctx, req, api)
		},
	)

	s.AddTool(
		mcp.NewTool("cancel_notification_receipt",
			mcp.WithDescription("Stop an acknowledged notification from repeating, e.g. once the problem it reported has cleared. The receipt becomes canceled and its callback is not sent; one that already finished comes back unchanged."+receiptScope),
			mcp.WithDestructiveHintAnnotation(true),
			mcp.WithIdempotentHintAnnotation(true),
			mcp.WithOpenWorldHintAnnotation(true),
			mcp.WithNumber("notification_id", mcp.Required(), mcp.Description("Notification id from create_notification")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return handleCancelNotificationReceipt(ctx, req, api)
		},
	)

	s.AddTool(
		mcp.NewTool("cancel_notification_receipts_by_tag",
			mcp.WithDescription("Stop every acknowledged notification still repeating that was sent with this tag, and return {\"canceled\": n}. Their callbacks are not sent."+receiptScope),
			mcp.WithDestructiveHintAnnotation(true),
			mcp.WithIdempotentHintAnnotation(true),
			mcp.WithOpenWorldHintAnnotation(true),
			mcp.WithString("tag", mcp.Required(), mcp.Description("One of the tags the notifications were sent with (1-64 printable ASCII characters, no spaces)")),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return handleCancelNotificationReceiptsByTag(ctx, req, api)
		},
	)

	s.AddTool(
		mcp.NewTool("wait_for_ack",
			mcp.WithDescription("Wait until a notification sent with acknowledge stops repeating. Returns {status, acknowledged, receipt, reason}: acknowledged=true once someone tapped an action without a url that does not open the app on one of their devices (receipt.action_id says which, receipt.acknowledged_by_device where); acknowledged=false with a reason when it expired, was canceled, or timeout_seconds ran out first (it then keeps repeating). None of these is a tool error."),
			mcp.WithReadOnlyHintAnnotation(true),
			mcp.WithOpenWorldHintAnnotation(true),
			mcp.WithNumber("notification_id", mcp.Required(), mcp.Description("Notification id from create_notification")),
			mcp.WithNumber("timeout_seconds",
				mcp.Description("Seconds to wait before giving up (default 120) (min: 1, max: 600)"),
				mcp.Min(1), mcp.Max(answerWaitMax),
			),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return handleWaitForAck(ctx, req, api)
		},
	)
}

func handleGetNotificationReceipt(ctx context.Context, req mcp.CallToolRequest, api *client.APIClient) (*mcp.CallToolResult, error) {
	id, err := notificationIDArg(req)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	raw, _, err := api.GetNotificationReceipt(ctx, id, min(req.GetInt("wait_seconds", 0), answerLongPollMax))
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(string(raw)), nil
}

func handleCancelNotificationReceipt(ctx context.Context, req mcp.CallToolRequest, api *client.APIClient) (*mcp.CallToolResult, error) {
	id, err := notificationIDArg(req)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	raw, err := api.CancelNotificationReceipt(ctx, id)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(string(raw)), nil
}

func handleCancelNotificationReceiptsByTag(ctx context.Context, req mcp.CallToolRequest, api *client.APIClient) (*mcp.CallToolResult, error) {
	tag, err := req.RequireString("tag")
	if err != nil || tag == "" {
		return mcp.NewToolResultError("tag is required"), nil
	}
	raw, err := api.CancelNotificationReceiptsByTag(ctx, tag)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(string(raw)), nil
}

// ackOutcome is wait_for_ack's result. Acknowledged is false when the receipt
// finished without an acknowledgement or the wait ran out, and Reason then
// says which; Receipt is the last one read.
type ackOutcome struct {
	Status       string          `json:"status,omitempty"`
	Acknowledged bool            `json:"acknowledged"`
	Receipt      json.RawMessage `json:"receipt,omitempty"`
	Reason       string          `json:"reason,omitempty"`
}

func ackResult(o ackOutcome) *mcp.CallToolResult {
	data, err := json.Marshal(o)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("encoding wait outcome: %v", err))
	}
	return mcp.NewToolResultText(string(data))
}

// handleWaitForAck long-polls a receipt until it leaves active. Unlike
// wait_for_answer on the same notification, an expiry or a cancel ends the
// wait at once instead of running out the timeout.
func handleWaitForAck(ctx context.Context, req mcp.CallToolRequest, api *client.APIClient) (*mcp.CallToolResult, error) {
	id, err := notificationIDArg(req)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	target := fmt.Sprintf("receipt %d", id)
	var last json.RawMessage
	judge := func(raw json.RawMessage) (string, *mcp.CallToolResult) {
		var r struct {
			Status       string `json:"status"`
			CancelReason string `json:"cancel_reason"`
		}
		if err := json.Unmarshal(raw, &r); err != nil {
			return "", mcp.NewToolResultError(fmt.Sprintf("decode %s: %v", target, err))
		}
		if r.Status == "" {
			return "", mcp.NewToolResultError(target + " has no status")
		}
		last = raw
		out := ackOutcome{Status: r.Status, Receipt: raw}
		switch r.Status {
		case "active":
			return r.Status, nil
		case "acknowledged":
			out.Acknowledged = true
		case "expired":
			out.Reason = "it expired with nobody acknowledging it"
		case "canceled":
			out.Reason = "it was canceled"
			if r.CancelReason != "" {
				out.Reason += " (" + r.CancelReason + ")"
			}
		default:
			out.Reason = "the receipt finished as " + r.Status
		}
		return r.Status, ackResult(out)
	}
	fetch := func(wait int) (json.RawMessage, int, error) { return api.GetNotificationReceipt(ctx, id, wait) }
	return runWait(ctx, req, "wait_for_ack", "an acknowledgement", target, fetch, judge, func(state string, timeout float64) *mcp.CallToolResult {
		return ackResult(ackOutcome{Status: state, Receipt: last,
			Reason: fmt.Sprintf("not acknowledged within %.0fs; it keeps repeating until it expires or is canceled", timeout)})
	}), nil
}
