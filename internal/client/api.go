package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

var slugPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,127}$`)

func validateSlug(slug string) error {
	if !slugPattern.MatchString(slug) {
		return fmt.Errorf("invalid slug %q: must match ^[a-zA-Z0-9][a-zA-Z0-9_-]{0,127}$", slug)
	}
	return nil
}

// withQuery appends url-encoded query parameters to path, returning path
// unchanged when q is empty. Shared by the GET builders so query assembly
// (encoding, empty-omission) lives in one place.
func withQuery(path string, q url.Values) string {
	if len(q) == 0 {
		return path
	}
	return path + "?" + q.Encode()
}

// APIClient wraps the PushWard API (api.pushward.app).
type APIClient struct{ *Base }

// NewAPIClient creates a new PushWard API client. The API client prefers a
// per-request token carried in the context (HTTP/remote mode) over the token
// argument, which serves as the stdio-mode fallback.
func NewAPIClient(baseURL, token string) *APIClient {
	b := NewBase(baseURL, token)
	b.useContextToken = true
	return &APIClient{b}
}

// ActivitiesPage is the paginated envelope returned by GET /activities (AIP-158).
type ActivitiesPage struct {
	Items      json.RawMessage `json:"items"`
	NextCursor string          `json:"next_cursor"`
	HasMore    bool            `json:"has_more"`
}

// ListActivitiesOptions controls pagination of GET /activities.
// Limit is 1-100 (server default 50 when zero). After is an opaque cursor
// from a prior page's NextCursor.
type ListActivitiesOptions struct {
	Limit int
	After string
}

// ListActivitiesPage fetches a single page of activities.
func (c *APIClient) ListActivitiesPage(ctx context.Context, opts ListActivitiesOptions) (*ActivitiesPage, error) {
	q := url.Values{}
	if opts.Limit > 0 {
		q.Set("limit", strconv.Itoa(opts.Limit))
	}
	if opts.After != "" {
		q.Set("after", opts.After)
	}
	raw, _, err := c.DoJSON(ctx, http.MethodGet, withQuery("/activities", q), nil)
	if err != nil {
		return nil, err
	}
	var page ActivitiesPage
	if err := json.Unmarshal(raw, &page); err != nil {
		return nil, fmt.Errorf("parsing activities page: %w", err)
	}
	return &page, nil
}

// maxListActivitiesPages bounds ListAllActivities to 2000 items at limit=100.
const maxListActivitiesPages = 20

// ErrListActivitiesTruncated is returned by ListAllActivities alongside a
// partial result when the server still has more pages after hitting the page
// cap. Callers should treat the returned JSON as "first N activities" and
// decide whether to warn the user.
var ErrListActivitiesTruncated = errors.New("list activities truncated at page cap")

// ListAllActivities walks the cursor until HasMore=false or the page cap is
// reached, returning the concatenated items as a JSON array. If the cap is
// hit while more pages remain, it returns the partial result along with
// ErrListActivitiesTruncated so the caller can surface a warning.
func (c *APIClient) ListAllActivities(ctx context.Context) (json.RawMessage, error) {
	all := make([]json.RawMessage, 0, maxListActivitiesPages*100)
	opts := ListActivitiesOptions{Limit: 100}
	truncated := false
	for i := 0; i < maxListActivitiesPages; i++ {
		page, err := c.ListActivitiesPage(ctx, opts)
		if err != nil {
			return nil, err
		}
		if len(page.Items) > 0 {
			var items []json.RawMessage
			// Go's json.Unmarshal treats a literal "null" as a no-op on slices,
			// so no explicit check is needed.
			if err := json.Unmarshal(page.Items, &items); err != nil {
				return nil, fmt.Errorf("parsing activities items: %w", err)
			}
			all = append(all, items...)
		}
		if !page.HasMore {
			break
		}
		// Server reports more pages but gave no cursor to fetch them — we cannot
		// continue, so flag the result as partial rather than silently complete.
		if page.NextCursor == "" {
			truncated = true
			break
		}
		opts.After = page.NextCursor
		if i == maxListActivitiesPages-1 {
			truncated = true
		}
	}
	raw, err := json.Marshal(all)
	if err != nil {
		return nil, err
	}
	if truncated {
		return raw, ErrListActivitiesTruncated
	}
	return raw, nil
}

// CreateActivityInput is the request body for POST /activities.
type CreateActivityInput struct {
	Slug     string   `json:"slug"`
	Name     string   `json:"name"`
	Priority *float64 `json:"priority,omitempty"`
	EndedTTL *float64 `json:"ended_ttl,omitempty"`
	StaleTTL *float64 `json:"stale_ttl,omitempty"`
}

// CreateActivity creates a new activity.
func (c *APIClient) CreateActivity(ctx context.Context, input CreateActivityInput) (json.RawMessage, error) {
	raw, _, err := c.DoJSON(ctx, http.MethodPost, "/activities", input)
	return raw, err
}

// GetActivity returns a single activity by slug. Optional includes (e.g.
// "log_backlog") are sent as a comma-separated ?include= query — the server
// returns server-owned extras like the log template's rolling backlog only
// when asked, omitting them from the default lean response.
func (c *APIClient) GetActivity(ctx context.Context, slug string, includes ...string) (json.RawMessage, error) {
	if err := validateSlug(slug); err != nil {
		return nil, err
	}
	q := url.Values{}
	if len(includes) > 0 {
		q.Set("include", strings.Join(includes, ","))
	}
	raw, _, err := c.DoJSON(ctx, http.MethodGet, withQuery("/activities/"+slug, q), nil)
	return raw, err
}

// DeleteActivity deletes an activity by slug.
func (c *APIClient) DeleteActivity(ctx context.Context, slug string) error {
	if err := validateSlug(slug); err != nil {
		return err
	}
	_, _, err := c.DoJSON(ctx, http.MethodDelete, "/activities/"+slug, nil)
	return err
}

// UpdateActivityInput is the request body for PATCH /activities/{slug}.
// State is optional under RFC 7396 merge-patch semantics — omit to inherit
// the stored state (unless the activity is preempted).
type UpdateActivityInput struct {
	State    string          `json:"state,omitempty"`
	Content  json.RawMessage `json:"content"`
	Priority *float64        `json:"priority,omitempty"`
	Sound    string          `json:"sound,omitempty"`
}

// UpdateActivity updates an activity's state and content.
func (c *APIClient) UpdateActivity(ctx context.Context, slug string, input UpdateActivityInput) (json.RawMessage, error) {
	if err := validateSlug(slug); err != nil {
		return nil, err
	}
	raw, _, err := c.DoJSON(ctx, http.MethodPatch, "/activities/"+slug, input)
	return raw, err
}

// GetMe returns the current user profile.
func (c *APIClient) GetMe(ctx context.Context) (json.RawMessage, error) {
	raw, _, err := c.DoJSON(ctx, http.MethodGet, "/auth/me", nil)
	return raw, err
}

// GetHealth returns the API health status.
func (c *APIClient) GetHealth(ctx context.Context) (json.RawMessage, error) {
	raw, _, err := c.DoJSON(ctx, http.MethodGet, "/health", nil)
	return raw, err
}

// GetReady returns the API readiness status.
func (c *APIClient) GetReady(ctx context.Context) (json.RawMessage, error) {
	raw, _, err := c.DoJSON(ctx, http.MethodGet, "/ready", nil)
	return raw, err
}

// MediaAttachment is a rich media attachment (image, video, or audio)
// rendered inline in the iOS notification expanded view. HTTPS only.
type MediaAttachment struct {
	URL  string `json:"url"`
	Type string `json:"type"`
}

// CreateNotificationInput is the request body for POST /notifications.
//
// `Actions` is forwarded as opaque `json.RawMessage` so new server-side action
// fields (most recently `method`/`headers`/`body`) flow through without an MCP
// rebuild. A typed struct silently dropped unknown fields on JSON unmarshal,
// re-marshalling them away before the request reached the server.
type CreateNotificationInput struct {
	Title             string            `json:"title"`
	Body              string            `json:"body"`
	Subtitle          string            `json:"subtitle,omitempty"`
	Source            string            `json:"source,omitempty"`
	SourceDisplayName string            `json:"source_display_name,omitempty"`
	ThreadID          string            `json:"thread_id,omitempty"`
	CollapseID        string            `json:"collapse_id,omitempty"`
	Level             string            `json:"level,omitempty"`
	IconURL           string            `json:"icon_url,omitempty"`
	Media             *MediaAttachment  `json:"media,omitempty"`
	URL               string            `json:"url,omitempty"`
	ActivitySlug      string            `json:"activity_slug,omitempty"`
	Metadata          map[string]string `json:"metadata,omitempty"`
	Actions           json.RawMessage   `json:"actions,omitempty"`
	Push              *bool             `json:"push,omitempty"`
	Volume            *float64          `json:"volume,omitempty"`
}

// CreateNotification creates an in-app notification with optional APNs push.
func (c *APIClient) CreateNotification(ctx context.Context, input CreateNotificationInput) (json.RawMessage, error) {
	raw, _, err := c.DoJSON(ctx, http.MethodPost, "/notifications", input)
	return raw, err
}

// SendEmailInput is the request body for POST /emails. `To` must already be a
// verified, non-unsubscribed recipient of the calling account — registering and
// verifying recipients is an hla_/dashboard operation, not reachable with the
// hlk_ integration key this MCP uses. Provide HTMLBody, TextBody, or both.
type SendEmailInput struct {
	To       string `json:"to"`
	Subject  string `json:"subject"`
	HTMLBody string `json:"html_body,omitempty"`
	TextBody string `json:"text_body,omitempty"`
}

// SendEmail sends a transactional email to a verified recipient.
func (c *APIClient) SendEmail(ctx context.Context, input SendEmailInput) (json.RawMessage, error) {
	raw, _, err := c.DoJSON(ctx, http.MethodPost, "/emails", input)
	return raw, err
}

// CreateWidgetInput is the request body for POST /widgets. Requires an hlk_
// integration key with the `widgets` permission flag.
//
// `Content` is forwarded as opaque `json.RawMessage` so new server-side widget
// content fields flow through without an MCP rebuild — the server owns the
// schema.
type CreateWidgetInput struct {
	Slug         string          `json:"slug"`
	Name         string          `json:"name"`
	Content      json.RawMessage `json:"content"`
	PushThrottle *float64        `json:"push_throttle,omitempty"`
}

// CreateWidget creates a new widget.
func (c *APIClient) CreateWidget(ctx context.Context, input CreateWidgetInput) (json.RawMessage, error) {
	raw, _, err := c.DoJSON(ctx, http.MethodPost, "/widgets", input)
	return raw, err
}

// ListWidgets returns all widgets owned by the caller.
func (c *APIClient) ListWidgets(ctx context.Context) (json.RawMessage, error) {
	raw, _, err := c.DoJSON(ctx, http.MethodGet, "/widgets", nil)
	return raw, err
}

// GetWidget returns a single widget by slug.
func (c *APIClient) GetWidget(ctx context.Context, slug string) (json.RawMessage, error) {
	if err := validateSlug(slug); err != nil {
		return nil, err
	}
	raw, _, err := c.DoJSON(ctx, http.MethodGet, "/widgets/"+slug, nil)
	return raw, err
}

// UpdateWidgetInput is the request body for PATCH /widgets/{slug}. Applied
// with RFC 7396 JSON merge-patch semantics — omitted fields are preserved,
// explicit `null` clears.
type UpdateWidgetInput struct {
	Name         string          `json:"name,omitempty"`
	Content      json.RawMessage `json:"content"`
	PushThrottle *float64        `json:"push_throttle,omitempty"`
}

// UpdateWidget partially updates a widget's content.
func (c *APIClient) UpdateWidget(ctx context.Context, slug string, input UpdateWidgetInput) (json.RawMessage, error) {
	if err := validateSlug(slug); err != nil {
		return nil, err
	}
	raw, _, err := c.DoJSON(ctx, http.MethodPatch, "/widgets/"+slug, input)
	return raw, err
}

// DeleteWidget removes a widget by slug.
func (c *APIClient) DeleteWidget(ctx context.Context, slug string) error {
	if err := validateSlug(slug); err != nil {
		return err
	}
	_, _, err := c.DoJSON(ctx, http.MethodDelete, "/widgets/"+slug, nil)
	return err
}
