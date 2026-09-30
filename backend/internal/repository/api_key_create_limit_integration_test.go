//go:build integration

package repository

import (
	"context"
	"errors"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/apikey"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

type concurrentAPIKeyCreateRepo struct {
	*apiKeyRepository
	counted chan<- struct{}
	resume  <-chan struct{}
}

func (r *concurrentAPIKeyCreateRepo) CountByUserID(ctx context.Context, userID int64) (int64, error) {
	count, err := r.apiKeyRepository.CountByUserID(ctx, userID)
	if err != nil {
		return 0, err
	}
	r.counted <- struct{}{}
	select {
	case <-r.resume:
		return count, nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func newAPIKeyCreateLimitFixture(t *testing.T) (*apiKeyRepository, *service.User) {
	t.Helper()
	client := testEntClient(t)
	user := mustCreateUser(t, client, &service.User{})
	repo := newAPIKeyRepositoryWithSQL(client, integrationDB)
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		// DELETE triggers cache invalidation, so remove this fixture's outbox entries afterward.
		keys, err := repo.ListKeysByUserID(cleanupCtx, user.ID)
		require.NoError(t, err)
		_, err = integrationDB.ExecContext(cleanupCtx, "DELETE FROM api_keys WHERE user_id = $1", user.ID)
		require.NoError(t, err)
		for _, key := range keys {
			_, err = integrationDB.ExecContext(cleanupCtx, "DELETE FROM auth_cache_invalidation_outbox WHERE cache_key = encode(sha256(convert_to($1, 'UTF8')), 'hex')", key)
			require.NoError(t, err)
		}
		_, err = integrationDB.ExecContext(cleanupCtx, "DELETE FROM users WHERE id = $1", user.ID)
		require.NoError(t, err)
	})
	return repo, user
}

func TestAPIKeyServiceCreate_ConcurrentActiveLimit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client := testEntClient(t)
	repo, user := newAPIKeyCreateLimitFixture(t)
	seed := &service.APIKey{UserID: user.ID, Key: "sk-create-limit-" + user.Email, Name: "existing", Status: service.StatusActive}
	require.NoError(t, repo.Create(ctx, seed))
	cfg := &config.Config{APIKeyCreate: config.APIKeyCreateConfig{MaxActivePerUser: 2}}
	counted := make(chan struct{}, 2)
	resume := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		// Separate repository/service instances share only the database and test barrier.
		guardedRepo := &concurrentAPIKeyCreateRepo{
			apiKeyRepository: newAPIKeyRepositoryWithSQL(client, integrationDB),
			counted:          counted, resume: resume,
		}
		svc := service.NewAPIKeyService(guardedRepo, NewUserRepository(client, integrationDB), nil, nil, nil, nil, cfg)
		go func() {
			_, err := svc.Create(ctx, user.ID, service.CreateAPIKeyRequest{Name: "concurrent"})
			results <- err
		}()
	}
	// Both preflight reads see one free slot before either request may insert.
	for range 2 {
		select {
		case <-counted:
		case <-ctx.Done():
			t.Fatal("concurrent creates did not reach the count barrier")
		}
	}
	close(resume)
	succeeded, limited := 0, 0
	for range 2 {
		select {
		case err := <-results:
			switch {
			case err == nil:
				succeeded++
			case errors.Is(err, service.ErrAPIKeyCountExceeded):
				limited++
			default:
				t.Errorf("unexpected create error: %v", err)
			}
		case <-ctx.Done():
			t.Fatal("concurrent creates did not finish")
		}
	}
	count, err := repo.CountByUserID(ctx, user.ID)
	require.NoError(t, err)
	require.Equal(t, 1, succeeded, "only one request may consume the remaining slot")
	require.Equal(t, 1, limited)
	require.EqualValues(t, 2, count)
}

func TestAPIKeyCreateWithActiveLimit_DuplicateKeyReleasesLock(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	repo, user := newAPIKeyCreateLimitFixture(t)
	value := "sk-create-limit-duplicate-" + user.Email
	key := &service.APIKey{UserID: user.ID, Key: value, Name: "existing", Status: service.StatusActive}
	require.NoError(t, repo.CreateWithActiveLimit(ctx, key, 2))
	duplicate := &service.APIKey{UserID: user.ID, Key: value, Name: "duplicate", Status: service.StatusActive}
	require.ErrorIs(t, repo.CreateWithActiveLimit(ctx, duplicate, 2), service.ErrAPIKeyExists)

	next := &service.APIKey{UserID: user.ID, Key: value + "-next", Name: "next", Status: service.StatusActive}
	require.NoError(t, repo.CreateWithActiveLimit(ctx, next, 2), "failed insertion must release the user lock and leave the slot available")
	count, err := repo.CountByUserID(ctx, user.ID)
	require.NoError(t, err)
	require.EqualValues(t, 2, count)
}

func (s *APIKeyRepoSuite) TestCreateWithActiveLimit_CountsDisabledAndExpiredKeys() {
	user := s.mustCreateUser("create-limit-state@test.com")
	expired := time.Now().Add(-time.Hour)
	key := &service.APIKey{
		UserID: user.ID, Key: "sk-create-limit-disabled", Name: "disabled", Status: service.StatusDisabled,
		ExpiresAt: &expired,
	}
	s.Require().NoError(s.repo.CreateWithActiveLimit(s.ctx, key, 1))
	next := &service.APIKey{UserID: user.ID, Key: "sk-create-limit-next", Name: "next", Status: service.StatusActive}
	s.Require().ErrorIs(s.repo.CreateWithActiveLimit(s.ctx, next, 1), service.ErrAPIKeyCountExceeded)
	s.Require().Zero(next.ID)

	s.Require().NoError(s.repo.Delete(s.ctx, key.ID))
	s.Require().NoError(s.repo.CreateWithActiveLimit(s.ctx, next, 1), "soft deletion releases the slot")
	s.Require().NotZero(next.ID)
}

func (s *APIKeyRepoSuite) TestCreateWithActiveLimit_ZeroDisablesLimit() {
	user := s.mustCreateUser("create-limit-zero@test.com")
	for _, value := range []string{"sk-create-limit-zero-1", "sk-create-limit-zero-2"} {
		key := &service.APIKey{UserID: user.ID, Key: value, Name: value, Status: service.StatusActive}
		s.Require().NoError(s.repo.CreateWithActiveLimit(s.ctx, key, 0))
	}
	count, err := s.repo.CountByUserID(s.ctx, user.ID)
	s.Require().NoError(err)
	s.Require().EqualValues(2, count)
}

func TestAPIKeyCreateWithActiveLimit_UsesCallerTransaction(t *testing.T) {
	for _, viaContext := range []bool{true, false} {
		name := "bound_client"
		if viaContext {
			name = "context"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			tx := testEntTx(t)
			user := mustCreateUser(t, tx.Client(), &service.User{})
			repo := newAPIKeyRepositoryWithSQL(tx.Client(), tx)
			if viaContext {
				repo = newAPIKeyRepositoryWithSQL(testEntClient(t), integrationDB)
				ctx = dbent.NewTxContext(ctx, tx)
			}
			key := &service.APIKey{UserID: user.ID, Key: "sk-create-limit-rollback", Name: "rollback", Status: service.StatusActive}
			require.NoError(t, repo.CreateWithActiveLimit(ctx, key, 1))
			require.NotZero(t, key.ID)
			require.NoError(t, tx.Rollback())
			exists, err := testEntClient(t).APIKey.Query().Where(apikey.IDEQ(key.ID)).Exist(context.Background())
			require.NoError(t, err)
			require.False(t, exists, "the caller must retain transaction ownership")
		})
	}
}
