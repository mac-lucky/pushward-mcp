package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/mac-lucky/pushward-mcp/internal/client"
)

// hlkPrefix is the expected prefix of a PushWard integration key, used as a
// cheap pre-validation to fail obvious garbage before the GET /auth/me call
// (which is authoritative).
const hlkPrefix = "hlk_"

// validateHLK confirms a PushWard API key by calling GET /auth/me with it and
// returns the stable user id (User.id) on success. Any upstream error (401,
// network, parse) yields an error and the key is treated as invalid; status is
// the upstream HTTP status, 0 when none arrived, so a caller can tell a refused
// key (401) from an outage. The key is never logged.
func validateHLK(ctx context.Context, apiBaseURL, hlk string) (userID string, status int, err error) {
	hlk = strings.TrimSpace(hlk)
	if hlk == "" {
		return "", 0, errors.New("empty API key")
	}
	if !strings.HasPrefix(hlk, hlkPrefix) {
		return "", 0, errors.New("not a PushWard API key")
	}
	raw, status, err := client.NewBase(apiBaseURL, hlk).DoJSON(ctx, http.MethodGet, "/auth/me", nil)
	if err != nil {
		return "", status, err
	}
	var u struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &u); err != nil {
		return "", status, err
	}
	if u.ID == "" {
		return "", status, errors.New("auth/me returned no user id")
	}
	return u.ID, status, nil
}
