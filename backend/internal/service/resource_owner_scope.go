package service

import (
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const (
	ResourceOwnerScopeSystem = "system"
	ResourceOwnerScopeUser   = "user"
)

// NormalizeResourceOwnerScope validates the optional admin resource ownership filter.
// An empty result means all resources.
func NormalizeResourceOwnerScope(raw string) (string, error) {
	scope := strings.ToLower(strings.TrimSpace(raw))
	switch scope {
	case "", "all":
		return "", nil
	case ResourceOwnerScopeSystem, ResourceOwnerScopeUser:
		return scope, nil
	default:
		return "", infraerrors.BadRequest("INVALID_RESOURCE_OWNER_SCOPE", "invalid resource owner scope")
	}
}
