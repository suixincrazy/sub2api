package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
)

type userModelSyncAccountRepo struct {
	AccountRepository
	account *Account
	err     error
	calls   int
}

func (r *userModelSyncAccountRepo) GetByID(context.Context, int64) (*Account, error) {
	r.calls++
	return r.account, r.err
}

type userModelSyncHTTPUpstream struct {
	HTTPUpstream
	calls              int
	request            *http.Request
	publicOnly         bool
	responseStatusCode int
	responseBody       string
}

func (s *userModelSyncHTTPUpstream) DoWithTLS(
	req *http.Request,
	_ string,
	_ int64,
	_ int,
	_ *tlsfingerprint.Profile,
) (*http.Response, error) {
	s.calls++
	s.request = req
	s.publicOnly = HTTPUpstreamNetworkPolicyFromContext(req.Context()).PublicOnly
	status := s.responseStatusCode
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(s.responseBody)),
		Header:     make(http.Header),
	}, nil
}

func TestSyncAccountUpstreamModelsRejectsForeignAccountBeforeRepositoryLookup(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("create sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	ownerID := int64(41)
	accountID := int64(91)
	repo := &userModelSyncAccountRepo{}
	testService := &AccountTestService{accountRepo: repo, cfg: upstreamModelSyncTestConfig()}
	svc := NewUserResourceService(db, nil, nil, nil)
	svc.SetAccountMaintenanceServices(testService, nil)
	mock.ExpectQuery(`SELECT EXISTS \(SELECT 1 FROM accounts WHERE id = \$1 AND owner_user_id = \$2 AND deleted_at IS NULL\)`).
		WithArgs(accountID, ownerID).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))

	if _, err := svc.SyncAccountUpstreamModels(context.Background(), ownerID, accountID); err == nil {
		t.Fatal("expected a foreign account to be rejected")
	}
	if repo.calls != 0 {
		t.Fatalf("foreign account reached the shared account repository: calls=%d", repo.calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func TestSyncAccountUpstreamModelsRechecksRepositoryOwner(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("create sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	ownerID := int64(41)
	foreignOwnerID := int64(42)
	accountID := int64(91)
	repo := &userModelSyncAccountRepo{account: &Account{ID: accountID, OwnerUserID: &foreignOwnerID}}
	upstream := &userModelSyncHTTPUpstream{responseBody: `{"data":[{"id":"gpt-5.4"}]}`}
	testService := &AccountTestService{accountRepo: repo, httpUpstream: upstream, cfg: upstreamModelSyncTestConfig()}
	svc := NewUserResourceService(db, nil, nil, nil)
	svc.SetAccountMaintenanceServices(testService, nil)
	mock.ExpectQuery(`SELECT EXISTS \(SELECT 1 FROM accounts WHERE id = \$1 AND owner_user_id = \$2 AND deleted_at IS NULL\)`).
		WithArgs(accountID, ownerID).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))

	if _, err := svc.SyncAccountUpstreamModels(context.Background(), ownerID, accountID); err == nil {
		t.Fatal("expected the repository owner mismatch to be rejected")
	}
	if upstream.calls != 0 {
		t.Fatalf("owner mismatch reached the upstream: calls=%d", upstream.calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

func TestSyncAccountUpstreamModelsPreviewIsEphemeralAndPublicOnly(t *testing.T) {
	upstream := &userModelSyncHTTPUpstream{responseBody: `{"data":[{"id":"gpt-5.4"},{"id":"gpt-5.4-mini"}]}`}
	testService := &AccountTestService{httpUpstream: upstream, cfg: upstreamModelSyncTestConfig()}
	svc := NewUserResourceService(nil, nil, nil, nil)
	svc.SetAccountMaintenanceServices(testService, nil)

	models, err := svc.SyncAccountUpstreamModelsPreview(context.Background(), 41, UserUpstreamModelsPreviewInput{
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		APIKey:   "temporary-secret",
	})
	if err != nil {
		t.Fatalf("preview returned error: %v", err)
	}
	if strings.Join(models, ",") != "gpt-5.4,gpt-5.4-mini" {
		t.Fatalf("unexpected models: %#v", models)
	}
	if upstream.calls != 1 || upstream.request == nil {
		t.Fatalf("expected one upstream request, calls=%d", upstream.calls)
	}
	if !upstream.publicOnly {
		t.Fatal("temporary user credentials were not protected by public-only network policy")
	}
	if got := upstream.request.Header.Get("Authorization"); got != "Bearer temporary-secret" {
		t.Fatalf("temporary credential was not applied to the in-memory request: %q", got)
	}
}

func TestSyncAccountUpstreamModelsPreviewCarriesCNModeAndProtocol(t *testing.T) {
	upstream := &userModelSyncHTTPUpstream{responseBody: `{"data":[{"id":"kimi-for-coding"}]}`}
	testService := &AccountTestService{httpUpstream: upstream, cfg: upstreamModelSyncTestConfig()}
	svc := NewUserResourceService(nil, nil, nil, nil)
	svc.SetAccountMaintenanceServices(testService, nil)

	models, err := svc.SyncAccountUpstreamModelsPreview(context.Background(), 41, UserUpstreamModelsPreviewInput{
		Platform:    PlatformKimi,
		Type:        AccountTypeAPIKey,
		APIKey:      "temporary-secret",
		AccountMode: AccountModeCoding,
		APIProtocol: APIProtocolAnthropic,
	})
	if err != nil {
		t.Fatalf("CN preview returned error: %v", err)
	}
	if strings.Join(models, ",") != "kimi-for-coding" {
		t.Fatalf("unexpected CN models: %#v", models)
	}
	if upstream.request == nil {
		t.Fatal("expected a CN upstream model request")
	}
	if got := upstream.request.URL.String(); got != "https://api.kimi.com/coding/v1/models" {
		t.Fatalf("CN account mode/protocol were not applied: %s", got)
	}
	if got := upstream.request.Header.Get("Authorization"); got != "Bearer temporary-secret" {
		t.Fatalf("temporary CN credential was not applied: %q", got)
	}
}

func TestSyncAccountUpstreamModelsPreviewRejectsInvalidCNProtocolCombination(t *testing.T) {
	upstream := &userModelSyncHTTPUpstream{responseBody: `{"data":[{"id":"should-not-run"}]}`}
	testService := &AccountTestService{httpUpstream: upstream, cfg: upstreamModelSyncTestConfig()}
	svc := NewUserResourceService(nil, nil, nil, nil)
	svc.SetAccountMaintenanceServices(testService, nil)

	_, err := svc.SyncAccountUpstreamModelsPreview(context.Background(), 41, UserUpstreamModelsPreviewInput{
		Platform:    PlatformKimi,
		Type:        AccountTypeAPIKey,
		APIKey:      "temporary-secret",
		AccountMode: AccountModePayG,
		APIProtocol: APIProtocolResponses,
	})
	if err == nil {
		t.Fatal("expected Kimi responses preview to be rejected")
	}
	if upstream.calls != 0 {
		t.Fatalf("invalid CN preview reached upstream: calls=%d", upstream.calls)
	}
}

func TestSyncAccountUpstreamModelsPreviewRejectsPrivateBaseURLBeforeHTTP(t *testing.T) {
	upstream := &userModelSyncHTTPUpstream{responseBody: `{"data":[{"id":"should-not-run"}]}`}
	testService := &AccountTestService{httpUpstream: upstream, cfg: upstreamModelSyncTestConfig()}
	svc := NewUserResourceService(nil, nil, nil, nil)
	svc.SetAccountMaintenanceServices(testService, nil)

	_, err := svc.SyncAccountUpstreamModelsPreview(context.Background(), 41, UserUpstreamModelsPreviewInput{
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		BaseURL:  "http://127.0.0.1:2375",
		APIKey:   "temporary-secret",
	})
	if err == nil {
		t.Fatal("expected private base_url to be rejected")
	}
	if upstream.calls != 0 {
		t.Fatalf("private base_url reached the HTTP client: calls=%d", upstream.calls)
	}
}
