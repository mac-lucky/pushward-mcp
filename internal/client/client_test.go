package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---- Base.DoJSON tests ----

func TestDoJSON_Success200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":1,"name":"test"}`))
	}))
	defer srv.Close()

	b := NewBase(srv.URL, "test-token")
	raw, code, err := b.DoJSON(context.Background(), http.MethodGet, "/test", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if code != 200 {
		t.Fatalf("expected status 200, got %d", code)
	}
	if string(raw) != `{"id":1,"name":"test"}` {
		t.Fatalf("unexpected body: %s", raw)
	}
}

func TestDoJSON_Success204EmptyBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	b := NewBase(srv.URL, "test-token")
	raw, code, err := b.DoJSON(context.Background(), http.MethodDelete, "/test", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if code != 204 {
		t.Fatalf("expected status 204, got %d", code)
	}
	if string(raw) != `{}` {
		t.Fatalf("expected empty JSON object, got: %s", raw)
	}
}

func TestDoJSON_AuthHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth != "Bearer my-secret-token" {
			t.Errorf("expected 'Bearer my-secret-token', got %q", auth)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	b := NewBase(srv.URL, "my-secret-token")
	_, _, err := b.DoJSON(context.Background(), http.MethodGet, "/test", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDoJSON_ContentTypeOnPost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ct := r.Header.Get("Content-Type")
		if ct != "application/json" {
			t.Errorf("expected Content-Type 'application/json', got %q", ct)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	b := NewBase(srv.URL, "tok")
	_, _, err := b.DoJSON(context.Background(), http.MethodPost, "/test", map[string]string{"key": "val"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDoJSON_NoContentTypeOnGetWithoutBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ct := r.Header.Get("Content-Type")
		if ct != "" {
			t.Errorf("expected no Content-Type header for GET without body, got %q", ct)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	b := NewBase(srv.URL, "tok")
	_, _, err := b.DoJSON(context.Background(), http.MethodGet, "/test", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDoJSON_ErrorResponses(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       string
		wantInErr  string
	}{
		{
			name:       "400 bad request",
			statusCode: http.StatusBadRequest,
			body:       `{"error":"bad request"}`,
			wantInErr:  "returned 400",
		},
		{
			name:       "404 not found",
			statusCode: http.StatusNotFound,
			body:       `{"error":"not found"}`,
			wantInErr:  "returned 404",
		},
		{
			name:       "500 internal server error",
			statusCode: http.StatusInternalServerError,
			body:       `{"error":"server error"}`,
			wantInErr:  "returned 500",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.statusCode)
				w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			b := NewBase(srv.URL, "tok")
			_, code, err := b.DoJSON(context.Background(), http.MethodGet, "/test", nil)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if code != tt.statusCode {
				t.Errorf("expected status %d, got %d", tt.statusCode, code)
			}
			if !strings.Contains(err.Error(), tt.wantInErr) {
				t.Errorf("error %q should contain %q", err.Error(), tt.wantInErr)
			}
			if !strings.Contains(err.Error(), tt.body) {
				t.Errorf("error %q should contain body snippet %q", err.Error(), tt.body)
			}
		})
	}
}

func TestDoJSON_ErrorBodyTruncation(t *testing.T) {
	longBody := strings.Repeat("x", 600)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(longBody))
	}))
	defer srv.Close()

	b := NewBase(srv.URL, "tok")
	_, _, err := b.DoJSON(context.Background(), http.MethodGet, "/test", nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	errMsg := err.Error()
	// The snippet is truncated to 500 chars + "..."
	if !strings.HasSuffix(errMsg, "...") {
		t.Errorf("expected error message to end with '...', got: %s", errMsg)
	}
	// Should not contain the full 600-char body
	if strings.Contains(errMsg, longBody) {
		t.Error("error message should have truncated the body")
	}
}

func TestDoJSON_ResponseBodyOverCapErrors(t *testing.T) {
	// A success body larger than the cap would be truncated mid-JSON, so DoJSON
	// must return an error rather than hand back corrupt, unparseable data.
	bigBody := strings.Repeat("a", 2<<20) // 2MB > 1MB cap
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(bigBody))
	}))
	defer srv.Close()

	b := NewBase(srv.URL, "tok")
	raw, status, err := b.DoJSON(context.Background(), http.MethodGet, "/test", nil)
	if err == nil {
		t.Fatalf("expected an error for an over-cap response, got nil (raw=%d bytes)", len(raw))
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("expected error to mention the cap, got: %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("expected status 200 to be reported, got %d", status)
	}
	if raw != nil {
		t.Errorf("expected nil body on over-cap error, got %d bytes", len(raw))
	}
}

func TestDoJSON_ResponseBodyAtCapSucceeds(t *testing.T) {
	// A body of *exactly* maxRespBytes (1MB) must be returned intact - this pins
	// the `len > maxRespBytes` boundary so an off-by-one regression to `>=`
	// (which would wrongly reject a legitimate 1MB body) is caught.
	const cap = 1 << 20
	body := `{"k":"` + strings.Repeat("a", cap-8) + `"}` // 6 + (cap-8) + 2 == cap
	if len(body) != cap {
		t.Fatalf("test body is %d bytes, want exactly %d; adjust padding", len(body), cap)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(body))
	}))
	defer srv.Close()

	b := NewBase(srv.URL, "tok")
	raw, _, err := b.DoJSON(context.Background(), http.MethodGet, "/test", nil)
	if err != nil {
		t.Fatalf("unexpected error for at-cap body: %v", err)
	}
	if len(raw) != cap {
		t.Errorf("expected full %d-byte body, got %d", cap, len(raw))
	}
}

func TestDoJSON_RequestBodySentCorrectly(t *testing.T) {
	type payload struct {
		Name  string `json:"name"`
		Value int    `json:"value"`
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var p payload
		if err := json.Unmarshal(body, &p); err != nil {
			t.Errorf("failed to unmarshal request body: %v", err)
		}
		if p.Name != "test" || p.Value != 42 {
			t.Errorf("unexpected body: %+v", p)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	b := NewBase(srv.URL, "tok")
	_, _, err := b.DoJSON(context.Background(), http.MethodPost, "/test", payload{Name: "test", Value: 42})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// ---- validateSlug tests ----

func TestValidateSlug(t *testing.T) {
	tests := []struct {
		slug    string
		wantErr bool
	}{
		// valid
		{"my-activity", false},
		{"a", false},
		{"Activity_1", false},
		{"a1-b2_c3", false},
		{strings.Repeat("a", 128), false},

		// invalid: empty
		{"", true},
		// invalid: starts with non-alphanumeric
		{"-starts-with-dash", true},
		{"_starts-with-underscore", true},
		// invalid: contains disallowed characters
		{"has space", true},
		{"has/slash", true},
		{"has..dots", true},
		{"slug@special", true},
		// invalid: too long (129 chars)
		{strings.Repeat("a", 129), true},
	}

	for _, tt := range tests {
		t.Run(tt.slug, func(t *testing.T) {
			err := validateSlug(tt.slug)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateSlug(%q) error = %v, wantErr %v", tt.slug, err, tt.wantErr)
			}
		})
	}
}

// ---- APIClient tests ----

func TestAPIClient_CreateActivity(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/activities" {
			t.Errorf("expected path /activities, got %s", r.URL.Path)
		}

		body, _ := io.ReadAll(r.Body)
		var input CreateActivityInput
		if err := json.Unmarshal(body, &input); err != nil {
			t.Fatalf("failed to unmarshal: %v", err)
		}
		if input.Slug != "my-app" || input.Name != "My App" {
			t.Errorf("unexpected input: %+v", input)
		}

		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"slug":"my-app","name":"My App"}`))
	}))
	defer srv.Close()

	c := NewAPIClient(srv.URL, "tok")
	raw, err := c.CreateActivity(context.Background(), CreateActivityInput{
		Slug: "my-app",
		Name: "My App",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(string(raw), "my-app") {
		t.Errorf("response should contain slug: %s", raw)
	}
}

func TestAPIClient_GetActivity(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if r.URL.Path != "/activities/test-slug" {
			t.Errorf("expected path /activities/test-slug, got %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"slug":"test-slug"}`))
	}))
	defer srv.Close()

	c := NewAPIClient(srv.URL, "tok")
	raw, err := c.GetActivity(context.Background(), "test-slug")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(string(raw), "test-slug") {
		t.Errorf("response should contain slug: %s", raw)
	}
}

func TestAPIClient_GetActivity_InvalidSlug(t *testing.T) {
	c := NewAPIClient("http://unused", "tok")
	_, err := c.GetActivity(context.Background(), "bad/slug")
	if err == nil {
		t.Fatal("expected error for invalid slug")
	}
	if !strings.Contains(err.Error(), "invalid slug") {
		t.Errorf("error should mention invalid slug: %v", err)
	}
}

func TestAPIClient_UpdateActivity(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Errorf("expected PATCH, got %s", r.Method)
		}
		if r.URL.Path != "/activities/my-slug" {
			t.Errorf("expected path /activities/my-slug, got %s", r.URL.Path)
		}

		body, _ := io.ReadAll(r.Body)
		var input UpdateActivityInput
		if err := json.Unmarshal(body, &input); err != nil {
			t.Fatalf("failed to unmarshal: %v", err)
		}
		if input.State != "active" {
			t.Errorf("expected state 'active', got %q", input.State)
		}

		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"slug":"my-slug","state":"active"}`))
	}))
	defer srv.Close()

	c := NewAPIClient(srv.URL, "tok")
	raw, err := c.UpdateActivity(context.Background(), "my-slug", UpdateActivityInput{
		State:   "active",
		Content: json.RawMessage(`{"title":"Hello"}`),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(string(raw), "active") {
		t.Errorf("response should contain state: %s", raw)
	}
}

func TestAPIClient_UpdateActivity_InvalidSlug(t *testing.T) {
	c := NewAPIClient("http://unused", "tok")
	_, err := c.UpdateActivity(context.Background(), "", UpdateActivityInput{State: "active"})
	if err == nil {
		t.Fatal("expected error for empty slug")
	}
}

// Confirms the merge-patch contract: an empty State must not appear on the
// wire so the server inherits the stored state rather than seeing "state":"".
func TestAPIClient_UpdateActivity_OmitsEmptyState(t *testing.T) {
	var wire map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &wire); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := NewAPIClient(srv.URL, "tok")
	_, err := c.UpdateActivity(context.Background(), "my-slug", UpdateActivityInput{
		Content: json.RawMessage(`{"progress":0.5}`),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := wire["state"]; ok {
		t.Errorf("state should be omitted when empty, got wire=%v", wire)
	}
}

func TestAPIClient_DeleteActivity(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("expected DELETE, got %s", r.Method)
		}
		if r.URL.Path != "/activities/rm-me" {
			t.Errorf("expected path /activities/rm-me, got %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := NewAPIClient(srv.URL, "tok")
	err := c.DeleteActivity(context.Background(), "rm-me")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestAPIClient_DeleteActivity_InvalidSlug(t *testing.T) {
	c := NewAPIClient("http://unused", "tok")
	err := c.DeleteActivity(context.Background(), "bad slug")
	if err == nil {
		t.Fatal("expected error for invalid slug")
	}
}

func TestAPIClient_CreateNotification(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/notifications" {
			t.Errorf("expected path /notifications, got %s", r.URL.Path)
		}

		body, _ := io.ReadAll(r.Body)
		var input CreateNotificationInput
		if err := json.Unmarshal(body, &input); err != nil {
			t.Fatalf("failed to unmarshal: %v", err)
		}
		if input.Title != "Test" || input.Body != "Hello" {
			t.Errorf("unexpected input: %+v", input)
		}
		if input.Push == nil || !*input.Push {
			t.Error("expected push to be true")
		}

		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"id":"notif-1"}`))
	}))
	defer srv.Close()

	c := NewAPIClient(srv.URL, "tok")
	push := true
	raw, err := c.CreateNotification(context.Background(), CreateNotificationInput{
		Title: "Test",
		Body:  "Hello",
		Push:  &push,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(string(raw), "notif-1") {
		t.Errorf("response should contain notif id: %s", raw)
	}
}

// TestAPIClient_CreateNotification_PushOmitted verifies that a nil Push pointer
// omits the "push" key entirely, so the server-side default (true) applies
// instead of the request forcing push=false.
func TestAPIClient_CreateNotification_PushOmitted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(body, &raw); err != nil {
			t.Fatalf("failed to unmarshal: %v", err)
		}
		if _, present := raw["push"]; present {
			t.Errorf("expected no \"push\" key when Push is nil, got body: %s", body)
		}
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"id":"notif-2"}`))
	}))
	defer srv.Close()

	c := NewAPIClient(srv.URL, "tok")
	if _, err := c.CreateNotification(context.Background(), CreateNotificationInput{
		Title: "Test",
		Body:  "Hello",
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestAPIClient_SendEmail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/emails" {
			t.Errorf("expected path /emails, got %s", r.URL.Path)
		}

		body, _ := io.ReadAll(r.Body)
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(body, &raw); err != nil {
			t.Fatalf("failed to unmarshal: %v", err)
		}
		var input SendEmailInput
		if err := json.Unmarshal(body, &input); err != nil {
			t.Fatalf("failed to unmarshal into SendEmailInput: %v", err)
		}
		if input.To != "user@example.com" || input.Subject != "Hi" || input.TextBody != "Hello" {
			t.Errorf("unexpected input: %+v", input)
		}
		// html_body is empty here; omitempty must drop the key entirely so the
		// server treats it as text-only rather than receiving an empty HTML body.
		if _, present := raw["html_body"]; present {
			t.Errorf("expected no \"html_body\" key when HTMLBody is empty, got body: %s", body)
		}

		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"id":1,"to":"user@example.com","subject":"Hi","status":"sent","delivery":"all","created_at":"2026-06-07T00:00:00Z"}`))
	}))
	defer srv.Close()

	c := NewAPIClient(srv.URL, "tok")
	raw, err := c.SendEmail(context.Background(), SendEmailInput{
		To:       "user@example.com",
		Subject:  "Hi",
		TextBody: "Hello",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(string(raw), `"status":"sent"`) {
		t.Errorf("response should pass through the email log: %s", raw)
	}
}

// TestAPIClient_SendEmail_HTMLBody covers the omitempty path's complement: when
// HTMLBody is set it must be serialized and present on the wire (the sibling test
// only asserts its absence).
func TestAPIClient_SendEmail_HTMLBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		// Decode into the value (not raw bytes): json.Marshal HTML-escapes < and >
		// to </> on the wire, which the server's parser decodes back - so
		// assert the decoded value the server actually receives, not the escaping.
		var raw map[string]any
		if err := json.Unmarshal(body, &raw); err != nil {
			t.Fatalf("failed to unmarshal: %v", err)
		}
		got, present := raw["html_body"]
		if !present {
			t.Errorf("expected html_body key to be present when HTMLBody is set, got body: %s", body)
		} else if got != "<p>hi</p>" {
			t.Errorf("unexpected html_body value: %v", got)
		}
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"id":1,"status":"sent"}`))
	}))
	defer srv.Close()

	c := NewAPIClient(srv.URL, "tok")
	if _, err := c.SendEmail(context.Background(), SendEmailInput{
		To:       "user@example.com",
		Subject:  "Hi",
		HTMLBody: "<p>hi</p>",
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestAPIClient_ListAllActivities_SinglePage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if r.URL.Path != "/activities" {
			t.Errorf("expected path /activities, got %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"items":[{"slug":"a"},{"slug":"b"}],"has_more":false}`))
	}))
	defer srv.Close()

	c := NewAPIClient(srv.URL, "tok")
	raw, err := c.ListAllActivities(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Result should be a flat array of items.
	var activities []map[string]any
	if err := json.Unmarshal(raw, &activities); err != nil {
		t.Fatalf("result is not a JSON array: %v (%s)", err, raw)
	}
	if len(activities) != 2 {
		t.Fatalf("expected 2 items, got %d", len(activities))
	}
	if activities[0]["slug"] != "a" || activities[1]["slug"] != "b" {
		t.Errorf("unexpected items: %v", activities)
	}
}

func TestAPIClient_ListAllActivities_MultiPage(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		after := r.URL.Query().Get("after")
		w.WriteHeader(http.StatusOK)
		switch after {
		case "":
			w.Write([]byte(`{"items":[{"slug":"a"},{"slug":"b"}],"next_cursor":"c1","has_more":true}`))
		case "c1":
			w.Write([]byte(`{"items":[{"slug":"c"}],"has_more":false}`))
		default:
			t.Errorf("unexpected cursor: %q", after)
		}
	}))
	defer srv.Close()

	c := NewAPIClient(srv.URL, "tok")
	raw, err := c.ListAllActivities(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 2 {
		t.Errorf("expected 2 page fetches, got %d", calls)
	}
	var activities []map[string]any
	if err := json.Unmarshal(raw, &activities); err != nil {
		t.Fatalf("result is not a JSON array: %v", err)
	}
	if len(activities) != 3 {
		t.Fatalf("expected 3 items across pages, got %d", len(activities))
	}
}

func TestAPIClient_ListAllActivities_TruncatedAtCap(t *testing.T) {
	// Server always reports has_more=true with a fresh cursor - ListAllActivities
	// must stop at the page cap and return ErrListActivitiesTruncated.
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"items":[{"slug":"a%d"}],"next_cursor":"c%d","has_more":true}`, calls, calls)
	}))
	defer srv.Close()

	c := NewAPIClient(srv.URL, "tok")
	raw, err := c.ListAllActivities(context.Background())
	if !errors.Is(err, ErrListActivitiesTruncated) {
		t.Fatalf("expected ErrListActivitiesTruncated, got %v", err)
	}
	if calls != maxListActivitiesPages {
		t.Errorf("expected %d fetches, got %d", maxListActivitiesPages, calls)
	}
	var activities []map[string]any
	if jerr := json.Unmarshal(raw, &activities); jerr != nil {
		t.Fatalf("partial result is not valid JSON: %v", jerr)
	}
	if len(activities) != maxListActivitiesPages {
		t.Errorf("expected %d items in truncated result, got %d", maxListActivitiesPages, len(activities))
	}
}

func TestAPIClient_ListAllActivities_MoreButNoCursor(t *testing.T) {
	// Server reports has_more=true but gives no next_cursor - we cannot fetch the
	// rest, so the result must be flagged as truncated rather than silently
	// treated as complete.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"items":[{"slug":"a"}],"next_cursor":"","has_more":true}`))
	}))
	defer srv.Close()

	c := NewAPIClient(srv.URL, "tok")
	raw, err := c.ListAllActivities(context.Background())
	if !errors.Is(err, ErrListActivitiesTruncated) {
		t.Fatalf("expected ErrListActivitiesTruncated, got %v", err)
	}
	var activities []map[string]any
	if jerr := json.Unmarshal(raw, &activities); jerr != nil {
		t.Fatalf("partial result is not valid JSON: %v", jerr)
	}
	if len(activities) != 1 {
		t.Errorf("expected the single fetched item, got %d", len(activities))
	}
}

func TestExtractErrorMessage_TitleOnly(t *testing.T) {
	// A Problem body with a title but no detail and no code should surface the
	// title (and no "[]" code tag).
	body := []byte(`{"title":"Not Found","status":404}`)
	got := extractErrorMessage(body)
	if got != "Not Found" {
		t.Errorf("expected title-only message %q, got %q", "Not Found", got)
	}
}

func TestExtractErrorMessage_ProblemBody(t *testing.T) {
	body := []byte(`{"type":"about:blank","title":"Bad Request","status":400,"detail":"Activity slug must be unique.","code":"activity.slug.invalid"}`)
	got := extractErrorMessage(body)
	if !strings.Contains(got, "[activity.slug.invalid]") {
		t.Errorf("expected code tag in message, got %q", got)
	}
	if !strings.Contains(got, "Activity slug must be unique.") {
		t.Errorf("expected detail in message, got %q", got)
	}
}

func TestExtractErrorMessage_RetryAfter(t *testing.T) {
	body := []byte(`{"title":"Too Many Requests","status":429,"detail":"slow down","code":"rate.limited","retry_after_ms":3000}`)
	got := extractErrorMessage(body)
	if !strings.Contains(got, "retry_after_ms=3000") {
		t.Errorf("expected retry_after_ms hint, got %q", got)
	}
}

func TestExtractErrorMessage_Fallback(t *testing.T) {
	// Unparseable body falls back to the raw snippet.
	body := []byte(`not json at all`)
	got := extractErrorMessage(body)
	if got != "not json at all" {
		t.Errorf("expected raw body, got %q", got)
	}
}

// ---- RelayClient tests ----

func TestRelayClient_PostWebhook(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/github-actions" {
			t.Errorf("expected path /github-actions, got %s", r.URL.Path)
		}
		auth := r.Header.Get("Authorization")
		if auth != "Bearer relay-tok" {
			t.Errorf("expected 'Bearer relay-tok', got %q", auth)
		}

		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("failed to unmarshal: %v", err)
		}
		if payload["event"] != "push" {
			t.Errorf("unexpected event: %v", payload["event"])
		}

		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c := NewRelayClient(srv.URL, "relay-tok")
	raw, err := c.PostWebhook(context.Background(), "github-actions", map[string]string{"event": "push"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(string(raw), "true") {
		t.Errorf("unexpected response: %s", raw)
	}
}

func TestRelayClient_PostWebhook_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte(`{"error":"upstream down"}`))
	}))
	defer srv.Close()

	c := NewRelayClient(srv.URL, "tok")
	_, err := c.PostWebhook(context.Background(), "sabnzbd", map[string]string{})
	if err == nil {
		t.Fatal("expected error for 502 response")
	}
	if !strings.Contains(err.Error(), "502") {
		t.Errorf("error should contain status code: %v", err)
	}
}
