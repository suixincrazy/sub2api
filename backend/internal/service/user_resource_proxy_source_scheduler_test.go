package service

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestClaimDueProxySourceIsOwnerScopedAndAtomic(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("create sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()
	mock.ExpectExec(`(?s)UPDATE proxy_sources.*owner_user_id IS NOT DISTINCT FROM \$2`).
		WithArgs(int64(44), int64(9)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	svc := NewUserResourceService(db, nil, nil, nil)
	claimed, err := svc.claimDueProxySource(context.Background(), dueUserProxySource{ID: 44, OwnerID: sql.NullInt64{Int64: 9, Valid: true}})
	if err != nil {
		t.Fatalf("claim due source: %v", err)
	}
	if !claimed {
		t.Fatal("expected due source to be claimed")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet SQL expectations: %v", err)
	}
}

func TestClaimDueSystemProxySourceUsesNullSafeOwnerScope(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("create sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()
	mock.ExpectExec(`(?s)UPDATE proxy_sources.*owner_user_id IS NOT DISTINCT FROM \$2`).
		WithArgs(int64(44), nil).
		WillReturnResult(sqlmock.NewResult(0, 1))

	svc := NewUserResourceService(db, nil, nil, nil)
	claimed, err := svc.claimDueProxySource(context.Background(), dueUserProxySource{ID: 44})
	if err != nil {
		t.Fatalf("claim due system source: %v", err)
	}
	if !claimed {
		t.Fatal("expected due system source to be claimed")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet SQL expectations: %v", err)
	}
}

func TestDueProxySourceSyncDispatchesByNullableOwner(t *testing.T) {
	t.Run("user source", func(t *testing.T) {
		var systemCalls, userCalls int
		err := syncDueProxySourceWith(
			context.Background(),
			dueUserProxySource{ID: 44, OwnerID: sql.NullInt64{Int64: 9, Valid: true}},
			func(context.Context, int64) error {
				systemCalls++
				return nil
			},
			func(_ context.Context, ownerID, sourceID int64) error {
				userCalls++
				if ownerID != 9 || sourceID != 44 {
					t.Fatalf("unexpected user sync arguments: owner=%d source=%d", ownerID, sourceID)
				}
				return nil
			},
		)
		if err != nil {
			t.Fatalf("dispatch user source: %v", err)
		}
		if systemCalls != 0 || userCalls != 1 {
			t.Fatalf("unexpected dispatch counts: system=%d user=%d", systemCalls, userCalls)
		}
	})

	t.Run("system source", func(t *testing.T) {
		var systemCalls, userCalls int
		err := syncDueProxySourceWith(
			context.Background(),
			dueUserProxySource{ID: 55},
			func(_ context.Context, sourceID int64) error {
				systemCalls++
				if sourceID != 55 {
					t.Fatalf("unexpected system source id: %d", sourceID)
				}
				return nil
			},
			func(context.Context, int64, int64) error {
				userCalls++
				return nil
			},
		)
		if err != nil {
			t.Fatalf("dispatch system source: %v", err)
		}
		if systemCalls != 1 || userCalls != 0 {
			t.Fatalf("unexpected dispatch counts: system=%d user=%d", systemCalls, userCalls)
		}
	})
}

func TestMarkScheduledSystemProxySourceFailureUsesNullSafeOwnerScope(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("create sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()
	mock.ExpectExec(`(?s)UPDATE proxy_sources.*owner_user_id IS NOT DISTINCT FROM \$3`).
		WithArgs(sqlmock.AnyArg(), int64(44), nil).
		WillReturnResult(sqlmock.NewResult(0, 1))

	svc := NewUserResourceService(db, nil, nil, nil)
	svc.markScheduledProxySourceFailure(dueUserProxySource{ID: 44}, context.DeadlineExceeded)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet SQL expectations: %v", err)
	}
}
func TestProxySourceNodeIdentityIsStableAndNamesFitSchema(t *testing.T) {
	node := parsedProxyNode{Name: "US West", Kind: "xray", Protocol: "vless", Host: "node.example.com", Port: 443, Network: "grpc"}
	base := proxySourceNodeBaseKey(node)
	first := proxySourceNodeKey(base, 1)
	if first != proxySourceNodeKey(base, 1) {
		t.Fatal("source node key is not stable")
	}
	if first == proxySourceNodeKey(base, 2) {
		t.Fatal("duplicate source nodes received the same key")
	}
	name := proxySourceNodeName(44, strings.Repeat("source", 30), strings.Repeat("node", 30), 7)
	if len([]rune(name)) > 100 || !strings.HasSuffix(name, " [44:7]") {
		t.Fatalf("source proxy name is not schema-safe: %q", name)
	}
}

func TestProxySourceQualityChecksIncludeCreatedAndUpdatedNodes(t *testing.T) {
	result := &ProxyImportResult{
		Created: []map[string]any{{"id": int64(101)}, {"id": int64(102)}},
		Updated: []map[string]any{{"id": int64(102)}, {"id": int64(103)}},
	}
	ids := proxyIDsForProxySourceQualityChecks(result)
	if len(ids) != 3 || ids[0] != 101 || ids[1] != 102 || ids[2] != 103 {
		t.Fatalf("unexpected proxy quality ids: %v", ids)
	}
}

func TestStripProxySourceMetadataPreventsUserSpoofing(t *testing.T) {
	payload := map[string]any{"extra": map[string]any{
		"source_id": int64(9), "source_node_key": "spoofed", "raw": "vless://example",
	}}
	stripProxySourceMetadata(payload)
	extra, ok := payload["extra"].(map[string]any)
	if !ok {
		t.Fatalf("unexpected proxy metadata payload: %#v", payload["extra"])
	}
	if _, ok := extra["source_id"]; ok {
		t.Fatal("user-provided source_id was retained")
	}
	if _, ok := extra["source_node_key"]; ok {
		t.Fatal("user-provided source_node_key was retained")
	}
	if extra["raw"] != "vless://example" {
		t.Fatal("non-reserved proxy metadata was removed")
	}
}

func TestUserResourceServiceCloseWithoutSchedulerIsSafe(t *testing.T) {
	svc := NewUserResourceService(nil, nil, nil, nil)
	if err := svc.Close(); err != nil {
		t.Fatalf("close service: %v", err)
	}
}

func TestDeleteProxySourceRollsBackWhenNodeDisableFails(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("create sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE proxy_sources SET deleted_at`).
		WithArgs(int64(44), int64(9)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`(?s)UPDATE proxies.*RETURNING id`).
		WithArgs(int64(9), "44", sqlmock.AnyArg()).
		WillReturnError(context.DeadlineExceeded)
	mock.ExpectRollback()

	svc := NewUserResourceService(db, nil, nil, nil)
	if err := svc.DeleteProxySource(context.Background(), 9, 44); err == nil {
		t.Fatal("expected proxy node disable failure")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet SQL expectations: %v", err)
	}
}

func TestSyncProxySourceNodesRollsBackWhenStatusWriteFails(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("create sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	proxyColumns := []string{
		"id", "owner_user_id", "is_public", "kind", "name", "protocol", "host", "port",
		"username", "password", "status", "expires_at", "fallback_mode", "backup_proxy_id",
		"expiry_warn_days", "extra",
	}
	mock.ExpectQuery(`(?s)SELECT id, owner_user_id.*FROM proxies`).
		WithArgs(int64(9), "44").
		WillReturnRows(sqlmock.NewRows(proxyColumns))
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT id FROM proxy_sources`).
		WithArgs(int64(44), int64(9)).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(44)))
	mock.ExpectQuery(`(?s)SELECT id, owner_user_id.*FROM proxies`).
		WithArgs(int64(9), "44").
		WillReturnRows(sqlmock.NewRows(proxyColumns))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM proxies`).
		WithArgs(int64(9)).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(`INSERT INTO proxies .* RETURNING id`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(101)))
	mock.ExpectQuery(`(?s)UPDATE proxies.*RETURNING id`).
		WithArgs(int64(9), "44", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectExec(`UPDATE proxy_sources.*last_sync_status`).
		WillReturnError(sql.ErrConnDone)
	mock.ExpectRollback()

	svc := NewUserResourceService(db, nil, nil, nil)
	_, err = svc.syncProxySourceNodes(context.Background(), userResourceOwner(9), 44, "source", "http://1.1.1.1:8080#node", false)
	if err == nil {
		t.Fatal("expected source status failure to roll back node changes")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet SQL expectations: %v", err)
	}
}

func TestClearProxyDependentProbeIncludesFallbackUsers(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("create sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	mock.ExpectQuery(`(?s)UPDATE accounts.*backup_proxy_id = ANY.*RETURNING id`).
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(12)).AddRow(int64(13)))

	ids, err := clearProxyDependentProbeWith(context.Background(), db, []int64{101})
	if err != nil {
		t.Fatalf("clear dependent probes: %v", err)
	}
	if len(ids) != 2 || ids[0] != 12 || ids[1] != 13 {
		t.Fatalf("unexpected dependent account ids: %v", ids)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet SQL expectations: %v", err)
	}
}
