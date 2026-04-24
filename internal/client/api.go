package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
)

var slugPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,127}$`)

func validateSlug(slug string) error {
	if !slugPattern.MatchString(slug) {
		return fmt.Errorf("invalid slug %q: must match ^[a-zA-Z0-9][a-zA-Z0-9_-]{0,127}$", slug)
	}
	return nil
}

// APIClient wraps the PushWard API (api.pushward.app).
type APIClient struct{ *Base }

// NewAPIClient creates a new PushWard API client.
func NewAPIClient(baseURL, token string) *APIClient {
	return &APIClient{NewBase(baseURL, token)}
}

// ListActivities returns all activities for the authenticated user.
func (c *APIClient) ListActivities(ctx context.Context) (json.RawMessage, error) {
	raw, _, err := c.DoJSON(ctx, http.MethodGet, "/activities", nil)
	return raw, err
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

// GetActivity returns a single activity by slug.
func (c *APIClient) GetActivity(ctx context.Context, slug string) (json.RawMessage, error) {
	if err := validateSlug(slug); err != nil {
		return nil, err
	}
	raw, _, err := c.DoJSON(ctx, http.MethodGet, "/activities/"+slug, nil)
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

// UpdateActivityInput is the request body for PATCH /activity/{slug}.
// State is optional under RFC 7396 merge-patch semantics — omit to inherit
// the stored state (unless the activity is PREEMPTED).
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
	raw, _, err := c.DoJSON(ctx, http.MethodPatch, "/activity/"+slug, input)
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

// CreateNotificationInput is the request body for POST /notifications.
type CreateNotificationInput struct {
	Title             string            `json:"title"`
	Body              string            `json:"body"`
	Subtitle          string            `json:"subtitle,omitempty"`
	Source            string            `json:"source,omitempty"`
	SourceDisplayName string            `json:"source_display_name,omitempty"`
	Category          string            `json:"category,omitempty"`
	ThreadID          string            `json:"thread_id,omitempty"`
	CollapseID        string            `json:"collapse_id,omitempty"`
	Level             string            `json:"level,omitempty"`
	IconURL           string            `json:"icon_url,omitempty"`
	ImageURL          string            `json:"image_url,omitempty"`
	URL               string            `json:"url,omitempty"`
	ActivitySlug      string            `json:"activity_slug,omitempty"`
	Metadata          map[string]string `json:"metadata,omitempty"`
	Push              bool              `json:"push"`
	Volume            *float64          `json:"volume,omitempty"`
}

// CreateNotification creates an in-app notification with optional APNs push.
func (c *APIClient) CreateNotification(ctx context.Context, input CreateNotificationInput) (json.RawMessage, error) {
	raw, _, err := c.DoJSON(ctx, http.MethodPost, "/notifications", input)
	return raw, err
}
