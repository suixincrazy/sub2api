package admin

import (
	"context"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

type groupOwnerScopeLister interface {
	ListGroupsByOwnerScope(ctx context.Context, page, pageSize int, platform, status, search string, isExclusive *bool, ownerScope, sortBy, sortOrder string) ([]service.Group, int64, error)
}

type accountOwnerScopeLister interface {
	ListAccountsByOwnerScope(ctx context.Context, page, pageSize int, platform, accountType, status, search string, groupID int64, privacyMode, ownerScope, sortBy, sortOrder string) ([]service.Account, int64, error)
	ListAccountsForSchedulerScoreFilterByOwnerScope(ctx context.Context, platform, accountType, status, search string, groupID int64, privacyMode, ownerScope string) ([]service.Account, error)
}

type proxyOwnerScopeLister interface {
	ListProxiesWithAccountCountByOwnerScope(ctx context.Context, page, pageSize int, protocol, status, search, ownerScope string, sourceID int64, sortBy, sortOrder string) ([]service.ProxyWithAccountCount, int64, error)
}
