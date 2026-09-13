package service

import (
	"context"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestParseProxySubscriptionUserInfoReadsTrafficAndExpiry(t *testing.T) {
	info := parseProxySubscriptionUserInfo("upload=100; download=250; total=1000; expire=1758297600")
	if !info.HasUsed || info.TrafficUsed != 350 {
		t.Fatalf("expected used=350 (upload+download), got used=%d has=%v", info.TrafficUsed, info.HasUsed)
	}
	if !info.HasTotal || info.TrafficTotal != 1000 {
		t.Fatalf("expected total=1000, got total=%d has=%v", info.TrafficTotal, info.HasTotal)
	}
	if !info.HasExpiry || info.ExpiresAt == nil {
		t.Fatal("expected an expiry date")
	}
	if got := info.ExpiresAt.Unix(); got != 1758297600 {
		t.Fatalf("expected expiry unix 1758297600, got %d", got)
	}
}

func TestParseProxySubscriptionUserInfoTreatsZeroExpiryAsNoExpiry(t *testing.T) {
	info := parseProxySubscriptionUserInfo("upload=1; download=2; total=3; expire=0")
	if !info.HasExpiry {
		t.Fatal("expire=0 must count as a reported expiry so a stale date gets cleared")
	}
	if info.ExpiresAt != nil {
		t.Fatalf("expire=0 must store NULL, got %v", info.ExpiresAt)
	}
}

func TestParseProxySubscriptionUserInfoIgnoresMissingAndJunkFields(t *testing.T) {
	cases := []string{"", "   ", "no-pairs-here", "upload=abc; download=-5; total=; expire=oops"}
	for _, header := range cases {
		info := parseProxySubscriptionUserInfo(header)
		if info.hasAny() {
			t.Fatalf("header %q must not produce a snapshot, got %+v", header, info)
		}
	}
}

func TestParseProxySubscriptionUserInfoKeepsPartialReports(t *testing.T) {
	// A panel that reports only the quota must not zero out a previously known
	// "used" value, so HasUsed stays false.
	info := parseProxySubscriptionUserInfo("TOTAL = 2048 ")
	if !info.HasTotal || info.TrafficTotal != 2048 {
		t.Fatalf("expected total=2048 from a padded uppercase key, got %+v", info)
	}
	if info.HasUsed || info.HasExpiry {
		t.Fatalf("expected used/expiry to stay unreported, got %+v", info)
	}
}

func TestParseProxySubscriptionUserInfoAcceptsDecimalNumbers(t *testing.T) {
	info := parseProxySubscriptionUserInfo("upload=1.5; download=2.5; total=1073741824.0")
	if info.TrafficUsed != 3 {
		t.Fatalf("expected truncated used=3, got %d", info.TrafficUsed)
	}
	if info.TrafficTotal != 1073741824 {
		t.Fatalf("expected total=1073741824, got %d", info.TrafficTotal)
	}
}

func TestRecordProxySourceSubscriptionInfoSkipsUnreportedColumns(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("create sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()
	// Only `total` was reported: the statement must touch sub_traffic_total and
	// leave sub_traffic_used / sub_expires_at alone.
	mock.ExpectExec(`(?s)UPDATE proxy_sources SET sub_traffic_total = \$1, sub_info_updated_at = NOW\(\), updated_at = NOW\(\)`).
		WithArgs(int64(2048), int64(44), int64(9)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	svc := NewUserResourceService(db, nil, nil, nil)
	if err := svc.recordProxySourceSubscriptionInfo(context.Background(), userResourceOwner(9), 44, parseProxySubscriptionUserInfo("total=2048")); err != nil {
		t.Fatalf("record subscription info: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet SQL expectations: %v", err)
	}
}

func TestRecordProxySourceSubscriptionInfoWritesNothingWithoutHeader(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("create sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	svc := NewUserResourceService(db, nil, nil, nil)
	if err := svc.recordProxySourceSubscriptionInfo(context.Background(), nil, 44, parseProxySubscriptionUserInfo("")); err != nil {
		t.Fatalf("record subscription info: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expected no SQL for a source without a subscription-userinfo header: %v", err)
	}
}

func TestRecordProxySourceSubscriptionInfoWritesNullExpiry(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("create sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()
	mock.ExpectExec(`(?s)UPDATE proxy_sources SET sub_traffic_used = \$1, sub_traffic_total = \$2, sub_expires_at = \$3`).
		WithArgs(int64(3), int64(3), nil, int64(44), nil).
		WillReturnResult(sqlmock.NewResult(0, 1))

	svc := NewUserResourceService(db, nil, nil, nil)
	if err := svc.recordProxySourceSubscriptionInfo(context.Background(), nil, 44, parseProxySubscriptionUserInfo("upload=1; download=2; total=3; expire=0")); err != nil {
		t.Fatalf("record subscription info: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet SQL expectations: %v", err)
	}
}

func TestProxySourceSyncAllSkipsPausedSourcesWithoutFetching(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("create sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()
	mock.ExpectQuery(`(?s)SELECT id, name, sync_enabled FROM proxy_sources`).
		WithArgs(int64(9)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "sync_enabled"}).
			AddRow(int64(41), "paused one", false).
			AddRow(int64(42), "paused two", false))

	svc := NewUserResourceService(db, nil, nil, nil)
	result, err := svc.SyncAllProxySources(context.Background(), 9)
	if err != nil {
		t.Fatalf("sync all: %v", err)
	}
	if result.Total != 2 || result.SkippedCount != 2 {
		t.Fatalf("expected 2 sources all skipped, got %+v", result)
	}
	for _, item := range result.Items {
		if item.Status != proxySourceSyncStatusSkipped {
			t.Fatalf("expected skipped status, got %q", item.Status)
		}
	}
	// No further statements: a paused source must never be fetched or written.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet SQL expectations: %v", err)
	}
}

func TestProxySourceNextSyncMatchesSchedulerDuePredicate(t *testing.T) {
	// The scheduler treats "never synced" as due now and ignores paused sources;
	// next_sync_at in the API must be derived from exactly those two rules.
	for _, needle := range []string{
		"WHEN NOT sync_enabled THEN NULL::timestamptz",
		"WHEN last_synced_at IS NULL THEN NOW()",
		"ELSE last_synced_at + (refresh_interval_minutes * INTERVAL '1 minute')",
	} {
		if !strings.Contains(proxySourceSelectColumns, needle) {
			t.Fatalf("proxySourceSelectColumns must contain %q", needle)
		}
	}
}
