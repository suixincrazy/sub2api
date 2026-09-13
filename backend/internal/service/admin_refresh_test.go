package service

import (
	"context"
	"testing"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

type adminRefreshAccountRepo struct {
	AccountRepository
	account *Account
}

func (r *adminRefreshAccountRepo) GetByID(context.Context, int64) (*Account, error) {
	return r.account, nil
}

func TestAdminServiceRefreshAccountCredentialsRequiresRefreshService(t *testing.T) {
	svc := &adminServiceImpl{
		accountRepo: &adminRefreshAccountRepo{account: &Account{
			ID:          91,
			Platform:    PlatformOpenAI,
			Type:        AccountTypeOAuth,
			Credentials: map[string]any{"refresh_token": "refresh-token"},
		}},
	}

	_, err := svc.RefreshAccountCredentials(context.Background(), 91)

	if err == nil {
		t.Fatalf("expected missing token refresh service to fail")
	}
	if infraerrors.Reason(err) != "TOKEN_REFRESH_UNAVAILABLE" {
		t.Fatalf("unexpected error reason: %s", infraerrors.Reason(err))
	}
}
