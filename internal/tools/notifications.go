package tools

import (
	"context"
	"fmt"
	"math"
	"strconv"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/mac-lucky/pushward-mcp/internal/client"
)

// The answer stream and retract. Hand-written rather than generated (see
// skipOperations): the stream takes ?after=, ?wait= and ?limit=, which the
// generator does not emit, and a generated retract would be annotated as a
// harmless POST although it takes notifications off the user's devices.

func registerNotificationTools(s *mcpserver.MCPServer, api *client.APIClient) {
	s.AddTool(
		mcp.NewTool("list_notification_answers",
			mcp.WithDescription("Read the answer stream: the answers recorded after the cursor after, across every notification, oldest first, as {items, cursor}. Each item is a get_notification_answer result (notification_id, status answered, action_id, text, answered_at) plus collapse_id and thread_id when the notification had them, so one loop can handle the answers to many questions. "+
				"Start without after: that call returns no items, just the current cursor, so the answers kept for 30 days are not replayed. Or start from the answer_cursor create_notification returned: every answer to that send comes after it (a scheduled send returns none, so take a cursor before scheduling). "+
				"Then pass each response's cursor as after, with wait_seconds to hold the call until the next answer lands; when the time runs out it returns no items and the same cursor. A full page (limit items) means more are waiting, so call again at once. For one notification, wait_for_answer is simpler. "+
				"Two waits run at once per integration key (or app token); past that it returns 429 answer_wait.limit_exceeded, so retry shortly or call without wait_seconds. An integration key reads only the answers to notifications it sent, whichever organization member answered; the app token reads the whole account's."),
			mcp.WithReadOnlyHintAnnotation(true),
			mcp.WithOpenWorldHintAnnotation(true),
			mcp.WithString("after", mcp.Description("The cursor of the previous response (opaque, keep it as it is), or answer_cursor from create_notification. Leave it out on the first call."), mcp.MaxLength(64)),
			mcp.WithNumber("wait_seconds", mcp.Description("Seconds to hold the call while no answer has arrived after the cursor (default 0) (min: 0, max: 25). Ignored without after."), mcp.Min(0), mcp.Max(25)),
			mcp.WithNumber("limit", mcp.Description("The most answers to return (default 100) (min: 1, max: 100)"), mcp.Min(1), mcp.Max(100)),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return handleListNotificationAnswers(ctx, req, api)
		},
	)

	s.AddTool(
		mcp.NewTool("retract_notifications",
			mcp.WithDescription("Take back the notifications sent with a collapse_id once what they said is no longer true (the door was closed, the build was fixed). Every device the sender's notifications reach gets a silent push that removes the ones it shows under that collapse_id and marks them read in the app, and acknowledged notifications sent with it stop repeating (their receipts end canceled, cancel_reason retracted). Returns {devices: n}, the number of devices the retraction was sent to. "+
				"Best effort: a force-quit app or Low Power Mode can leave a notification on screen until the app runs again, and on Apple devices only PushWard 1.20 or later on iPhone, iPad and Mac removes it (Apple Watch is skipped). Nothing is deleted on the server. "+
				"It matches every notification of the account (or organization) under that collapse_id, whichever key sent it; an organization key limited to groups or tags reaches only those devices and stops only the repeats it sent itself. At most 10 a minute per account or organization."),
			mcp.WithDestructiveHintAnnotation(true),
			mcp.WithIdempotentHintAnnotation(true),
			mcp.WithOpenWorldHintAnnotation(true),
			mcp.WithString("collapse_id", mcp.Required(), mcp.Description("The collapse_id the notifications were sent with (create_notification or create_scheduled_notification), 1-64 characters"), mcp.MinLength(1), mcp.MaxLength(64)),
		),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return handleRetractNotifications(ctx, req, api)
		},
	)
}

// answerPageMax is the server's largest answer page.
const answerPageMax = 100

func handleListNotificationAnswers(ctx context.Context, req mcp.CallToolRequest, api *client.APIClient) (*mcp.CallToolResult, error) {
	after, err := cursorArg(req, "after")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	limit := 0 // the server's default page
	if _, ok := req.GetArguments()["limit"]; ok {
		limit = max(1, min(req.GetInt("limit", answerPageMax), answerPageMax))
	}
	raw, err := api.ListNotificationAnswers(ctx, after, min(req.GetInt("wait_seconds", 0), answerLongPollMax), limit)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(string(raw)), nil
}

func handleRetractNotifications(ctx context.Context, req mcp.CallToolRequest, api *client.APIClient) (*mcp.CallToolResult, error) {
	id, err := req.RequireString("collapse_id")
	if err != nil || id == "" {
		return mcp.NewToolResultError("collapse_id is required"), nil
	}
	raw, err := api.RetractNotifications(ctx, id)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(string(raw)), nil
}

// cursorArg reads an opaque cursor argument. The cursors look like numbers,
// so a model may send one unquoted; GetString would turn that into "" and the
// call would start over from now, skipping every answer since. A whole
// number is sent as its decimal string, anything else but a string is an
// error.
func cursorArg(req mcp.CallToolRequest, name string) (string, error) {
	switch v := req.GetArguments()[name].(type) {
	case nil:
		return "", nil
	case string:
		return v, nil
	case float64:
		if v == math.Trunc(v) && math.Abs(v) <= 1<<53 {
			return strconv.FormatInt(int64(v), 10), nil
		}
	}
	return "", fmt.Errorf("%s must be the cursor string of the previous response, passed as it is", name)
}
