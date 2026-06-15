package httpserve

import (
	"net/http"

	"github.com/mac-lucky/pushward-mcp/internal/client"
)

// bearerToken extracts the token from an "Authorization: Bearer <token>"
// header. Returns "" when absent or malformed.
func bearerToken(r *http.Request) string {
	return client.ParseBearer(r.Header.Get("Authorization"))
}
