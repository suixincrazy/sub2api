package service

import (
	"context"
	"errors"
	"net/http"
	"testing"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

type accountProxyOwnerRepoStub struct {
	ProxyRepository
	proxy *Proxy
}

func (s *accountProxyOwnerRepoStub) GetByID(context.Context, int64) (*Proxy, error) {
	return s.proxy, nil
}

func proxyRepoReturning(proxy *Proxy) ProxyRepository {
	return &accountProxyOwnerRepoStub{proxy: proxy}
}

type accountOwnerGroupRepoStub struct {
	GroupRepository
	groups map[int64]*Group
}

func (s *accountOwnerGroupRepoStub) GetByID(_ context.Context, id int64) (*Group, error) {
	group := s.groups[id]
	if group == nil {
		return nil, ErrGroupNotFound
	}
	clone := *group
	return &clone, nil
}

type ownerAtomicAccountRepo struct {
	*upstreamBillingProbeAdminRepo
	directCreates   int
	atomicCreates   int
	atomicUpdates   int
	atomicUpdateErr error
	groupsByAccount map[int64][]int64
}

func newOwnerAtomicAccountRepo(accounts map[int64]*Account) *ownerAtomicAccountRepo {
	return &ownerAtomicAccountRepo{
		upstreamBillingProbeAdminRepo: &upstreamBillingProbeAdminRepo{
			upstreamBillingProbeAccountRepo: &upstreamBillingProbeAccountRepo{accounts: accounts},
		},
		groupsByAccount: make(map[int64][]int64),
	}
}

func (r *ownerAtomicAccountRepo) Create(ctx context.Context, account *Account) error {
	r.directCreates++
	return r.upstreamBillingProbeAccountRepo.Create(ctx, account)
}

func (r *ownerAtomicAccountRepo) CreateWithAccountGroups(ctx context.Context, account *Account, groups []AccountGroup) error {
	r.atomicCreates++
	groupIDs := make([]int64, 0, len(groups))
	for _, group := range groups {
		groupIDs = append(groupIDs, group.GroupID)
	}
	account.GroupIDs = append([]int64(nil), groupIDs...)
	account.AccountGroups = append([]AccountGroup(nil), groups...)
	if err := r.upstreamBillingProbeAccountRepo.Create(ctx, account); err != nil {
		return err
	}
	r.groupsByAccount[account.ID] = groupIDs
	return nil
}

func (r *ownerAtomicAccountRepo) UpdateAccountWithGroupsAtomically(ctx context.Context, account *Account, groupIDs []int64, _ *bool) error {
	r.atomicUpdates++
	if r.atomicUpdateErr != nil {
		return r.atomicUpdateErr
	}
	committed := *account
	committed.GroupIDs = append([]int64(nil), groupIDs...)
	committed.AccountGroups = accountGroupsFromIDs(groupIDs)
	if err := r.Update(ctx, &committed); err != nil {
		return err
	}
	r.groupsByAccount[account.ID] = append([]int64(nil), groupIDs...)
	return nil
}

func TestAccountProxyOwnerCompatible(t *testing.T) {
	ownerOne := int64(1)
	ownerTwo := int64(2)

	tests := []struct {
		name         string
		accountOwner *int64
		proxy        *Proxy
		want         bool
	}{
		{name: "system account with system proxy", proxy: &Proxy{}, want: true},
		{name: "system account with user proxy", proxy: &Proxy{OwnerUserID: &ownerOne}, want: true},
		{name: "user account with owned proxy", accountOwner: &ownerOne, proxy: &Proxy{OwnerUserID: &ownerOne}, want: true},
		{name: "user account with another private proxy", accountOwner: &ownerOne, proxy: &Proxy{OwnerUserID: &ownerTwo}, want: false},
		{name: "user account with private system proxy", accountOwner: &ownerOne, proxy: &Proxy{}, want: false},
		{name: "user account with public system proxy", accountOwner: &ownerOne, proxy: &Proxy{IsPublic: true}, want: true},
		{name: "user account with public proxy owned by another user", accountOwner: &ownerOne, proxy: &Proxy{OwnerUserID: &ownerTwo, IsPublic: true}, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, accountProxyOwnerCompatible(tt.accountOwner, tt.proxy))
		})
	}
}

func TestCreateAccountRejectsUserGroupBeforePersistingAccount(t *testing.T) {
	groupOwner := int64(9)
	repo := newOwnerAtomicAccountRepo(nil)
	svc := &adminServiceImpl{
		accountRepo:          repo,
		accountDuplicateRepo: repo,
		groupRepo: &accountOwnerGroupRepoStub{groups: map[int64]*Group{
			11: {ID: 11, OwnerUserID: &groupOwner, Platform: PlatformOpenAI},
		}},
	}

	_, err := svc.CreateAccount(context.Background(), &CreateAccountInput{
		Name:                  "system-account",
		Platform:              PlatformOpenAI,
		Type:                  AccountTypeAPIKey,
		Credentials:           map[string]any{"api_key": "sk-test"},
		GroupIDs:              []int64{11},
		SkipDefaultGroupBind:  true,
		SkipMixedChannelCheck: true,
	})

	require.Error(t, err)
	require.Equal(t, http.StatusBadRequest, infraerrors.Code(err))
	require.Equal(t, "ACCOUNT_GROUP_OWNER_MISMATCH", string(infraerrors.Reason(err)))
	require.Zero(t, repo.directCreates)
	require.Zero(t, repo.atomicCreates)
	require.Empty(t, repo.accounts)
}

func TestCreateAccountWithSystemGroupUsesAtomicWriter(t *testing.T) {
	repo := newOwnerAtomicAccountRepo(nil)
	svc := &adminServiceImpl{
		accountRepo:          repo,
		accountDuplicateRepo: repo,
		groupRepo: &accountOwnerGroupRepoStub{groups: map[int64]*Group{
			12: {ID: 12, Platform: PlatformOpenAI},
		}},
	}

	created, err := svc.CreateAccount(context.Background(), &CreateAccountInput{
		Name:                  "system-account",
		Platform:              PlatformOpenAI,
		Type:                  AccountTypeAPIKey,
		Credentials:           map[string]any{"api_key": "sk-test"},
		GroupIDs:              []int64{12},
		SkipDefaultGroupBind:  true,
		SkipMixedChannelCheck: true,
	})

	require.NoError(t, err)
	require.NotZero(t, created.ID)
	require.Zero(t, repo.directCreates)
	require.Equal(t, 1, repo.atomicCreates)
	require.Equal(t, []int64{12}, repo.groupsByAccount[created.ID])
}

func TestUpdateAccountRejectsGroupFromAnotherOwnerBeforeWrite(t *testing.T) {
	accountOwner := int64(7)
	groupOwner := int64(8)
	accountID := int64(21)
	repo := newOwnerAtomicAccountRepo(map[int64]*Account{
		accountID: {ID: accountID, Name: "original", OwnerUserID: &accountOwner, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive},
	})
	svc := &adminServiceImpl{
		accountRepo: repo,
		groupRepo: &accountOwnerGroupRepoStub{groups: map[int64]*Group{
			31: {ID: 31, OwnerUserID: &groupOwner, Platform: PlatformOpenAI},
		}},
	}
	groupIDs := []int64{31}

	_, err := svc.UpdateAccount(context.Background(), accountID, &UpdateAccountInput{
		Name:                  "changed",
		GroupIDs:              &groupIDs,
		SkipMixedChannelCheck: true,
	})

	require.Error(t, err)
	require.Equal(t, "ACCOUNT_GROUP_OWNER_MISMATCH", string(infraerrors.Reason(err)))
	require.Zero(t, repo.atomicUpdates)
	require.Equal(t, "original", repo.accounts[accountID].Name)
}

func TestUpdateAccountAtomicFailurePreservesOriginalFields(t *testing.T) {
	accountID := int64(22)
	repo := newOwnerAtomicAccountRepo(map[int64]*Account{
		accountID: {ID: accountID, Name: "original", Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive},
	})
	repo.atomicUpdateErr = errors.New("account group write failed")
	svc := &adminServiceImpl{
		accountRepo: repo,
		groupRepo: &accountOwnerGroupRepoStub{groups: map[int64]*Group{
			32: {ID: 32, Platform: PlatformOpenAI},
		}},
	}
	groupIDs := []int64{32}

	_, err := svc.UpdateAccount(context.Background(), accountID, &UpdateAccountInput{
		Name:                  "changed",
		GroupIDs:              &groupIDs,
		SkipMixedChannelCheck: true,
	})

	require.ErrorContains(t, err, "account group write failed")
	require.Equal(t, 1, repo.atomicUpdates)
	require.Equal(t, "original", repo.accounts[accountID].Name)
	require.Empty(t, repo.groupsByAccount[accountID])
}

func TestUpdateAccountAllowsMatchingOwnerGroupAndProxyAtomically(t *testing.T) {
	ownerID := int64(7)
	accountID := int64(23)
	proxyID := int64(43)
	repo := newOwnerAtomicAccountRepo(map[int64]*Account{
		accountID: {ID: accountID, Name: "original", OwnerUserID: &ownerID, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive},
	})
	svc := &adminServiceImpl{
		accountRepo: repo,
		groupRepo: &accountOwnerGroupRepoStub{groups: map[int64]*Group{
			33: {ID: 33, OwnerUserID: &ownerID, Platform: PlatformOpenAI},
		}},
		proxyRepo: proxyRepoReturning(&Proxy{ID: proxyID, OwnerUserID: &ownerID}),
	}
	groupIDs := []int64{33}

	updated, err := svc.UpdateAccount(context.Background(), accountID, &UpdateAccountInput{
		Name:                  "changed",
		ProxyID:               &proxyID,
		GroupIDs:              &groupIDs,
		SkipMixedChannelCheck: true,
	})

	require.NoError(t, err)
	require.Equal(t, 1, repo.atomicUpdates)
	require.Equal(t, "changed", updated.Name)
	require.Equal(t, &proxyID, updated.ProxyID)
	require.Equal(t, []int64{33}, updated.GroupIDs)
	require.Equal(t, []int64{33}, repo.groupsByAccount[accountID])
}

func TestCreateAccountAllowsUserOwnedProxyForSystemAccount(t *testing.T) {
	proxyOwner := int64(9)
	proxyID := int64(41)
	repo := &upstreamBillingProbeAccountRepo{}
	svc := &adminServiceImpl{
		accountRepo: &upstreamBillingProbeAdminRepo{repo},
		proxyRepo:   proxyRepoReturning(&Proxy{ID: proxyID, OwnerUserID: &proxyOwner}),
	}

	_, err := svc.CreateAccount(context.Background(), &CreateAccountInput{
		Name:                 "system-account",
		Platform:             PlatformOpenAI,
		Type:                 AccountTypeAPIKey,
		Credentials:          map[string]any{"api_key": "sk-test"},
		ProxyID:              &proxyID,
		SkipDefaultGroupBind: true,
	})

	require.NoError(t, err)
	require.NotNil(t, repo.accounts[1])
	require.Equal(t, &proxyID, repo.accounts[1].ProxyID)
}

func TestUpdateAccountRejectsProxyFromAnotherOwnerBeforeWrite(t *testing.T) {
	accountOwner := int64(7)
	proxyOwner := int64(8)
	accountID := int64(22)
	proxyID := int64(42)
	repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{
		accountID: {ID: accountID, OwnerUserID: &accountOwner, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive},
	}}
	svc := &adminServiceImpl{
		accountRepo: &upstreamBillingProbeAdminRepo{repo},
		proxyRepo:   proxyRepoReturning(&Proxy{ID: proxyID, OwnerUserID: &proxyOwner}),
	}

	_, err := svc.UpdateAccount(context.Background(), accountID, &UpdateAccountInput{ProxyID: &proxyID})

	require.Error(t, err)
	require.Equal(t, "ACCOUNT_PROXY_OWNER_MISMATCH", string(infraerrors.Reason(err)))
	require.Nil(t, repo.accounts[accountID].ProxyID)
}

func TestBulkUpdateAccountsAcceptsPublicProxyForUserAccounts(t *testing.T) {
	accountOwner := int64(7)
	accountID := int64(23)
	proxyID := int64(43)
	repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{
		accountID: {ID: accountID, OwnerUserID: &accountOwner, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive},
	}}
	svc := &adminServiceImpl{
		accountRepo: &upstreamBillingProbeAdminRepo{repo},
		proxyRepo:   proxyRepoReturning(&Proxy{ID: proxyID, IsPublic: true}),
	}

	result, err := svc.BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{
		AccountIDs: []int64{accountID},
		ProxyID:    &proxyID,
	})

	require.NoError(t, err)
	require.Equal(t, 1, result.Success)
	require.Len(t, repo.bulkUpdates, 1)
	require.Equal(t, &proxyID, repo.bulkUpdates[0].ProxyID)
}
