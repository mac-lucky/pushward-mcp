package observability

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	mcp "github.com/mark3labs/mcp-go/mcp"

	"github.com/mac-lucky/pushward-mcp/internal/client"
)

func hooksLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewJSONHandler(&buf, nil)), &buf
}

func TestLoggingHooksAuditLine(t *testing.T) {
	log, buf := hooksLogger()
	h := LoggingHooks(log)

	ctx := client.ContextWithUserID(context.Background(), "user-42")
	req := &mcp.CallToolRequest{}
	req.Params.Name = "create_activity"
	for _, f := range h.OnBeforeCallTool {
		f(ctx, nil, req)
	}

	line := buf.String()
	if !strings.Contains(line, "tool.call") || !strings.Contains(line, "create_activity") {
		t.Errorf("audit line missing tool call info: %s", line)
	}
	if !strings.Contains(line, "user-42") {
		t.Errorf("audit line missing user id: %s", line)
	}
}

func TestLoggingHooksNeverLogCredentials(t *testing.T) {
	// The package guarantee: the per-user upstream token rides in the request
	// context right next to the user id, and must never reach a log line.
	log, buf := hooksLogger()
	h := LoggingHooks(log)

	ctx := client.ContextWithToken(context.Background(), "hlk_secret_credential")
	ctx = client.ContextWithUserID(ctx, "user-42")
	req := &mcp.CallToolRequest{}
	req.Params.Name = "create_notification"
	for _, f := range h.OnBeforeCallTool {
		f(ctx, nil, req)
	}
	for _, f := range h.OnError {
		f(ctx, nil, mcp.MethodToolsCall, nil, errors.New("upstream refused"))
	}

	if out := buf.String(); strings.Contains(out, "hlk_secret_credential") {
		t.Fatalf("credential leaked into log output: %s", out)
	}
}

func TestLoggingHooksErrorLine(t *testing.T) {
	log, buf := hooksLogger()
	h := LoggingHooks(log)

	for _, f := range h.OnError {
		f(context.Background(), nil, mcp.MethodToolsCall, nil, errors.New("boom"))
	}

	line := buf.String()
	if !strings.Contains(line, "mcp.error") || !strings.Contains(line, "boom") {
		t.Errorf("error line incomplete: %s", line)
	}
}
