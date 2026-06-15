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
// network, parse) yields an error and the key is treated as invalid. The key is
// never logged.
func validateHLK(ctx context.Context, apiBaseURL, hlk string) (string, error) {
	hlk = strings.TrimSpace(hlk)
	if hlk == "" {
		return "", errors.New("empty API key")
	}
	if !strings.HasPrefix(hlk, hlkPrefix) {
		return "", errors.New("not a PushWard API key")
	}
	raw, _, err := client.NewBase(apiBaseURL, hlk).DoJSON(ctx, http.MethodGet, "/auth/me", nil)
	if err != nil {
		return "", err
	}
	var u struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &u); err != nil {
		return "", err
	}
	if u.ID == "" {
		return "", errors.New("auth/me returned no user id")
	}
	return u.ID, nil
}
