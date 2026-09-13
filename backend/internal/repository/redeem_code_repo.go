package repository

import (
	"context"
	"strings"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/redeemcode"
	"github.com/Wei-Shaw/sub2api/ent/redeemcodeusage"
	"github.com/Wei-Shaw/sub2api/ent/user"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/service"

	entsql "entgo.io/ent/dialect/sql"
)

type redeemCodeRepository struct {
	client *dbent.Client
}

func NewRedeemCodeRepository(client *dbent.Client) service.RedeemCodeRepository {
	return &redeemCodeRepository{client: client}
}

func (r *redeemCodeRepository) Create(ctx context.Context, code *service.RedeemCode) error {
	if code.UsedBy == nil {
		return r.create(ctx, clientFromContext(ctx, r.client), code)
	}
	if tx := dbent.TxFromContext(ctx); tx != nil {
		return r.create(ctx, tx.Client(), code)
	}
	tx, err := r.client.Tx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	txCtx := dbent.NewTxContext(ctx, tx)
	if err := r.create(txCtx, tx.Client(), code); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *redeemCodeRepository) create(ctx context.Context, client *dbent.Client, code *service.RedeemCode) error {
	usedAt := code.UsedAt
	usedCount := code.UsedCount
	if code.UsedBy != nil {
		if usedAt == nil {
			now := time.Now()
			usedAt = &now
		}
		usedCount = max(usedCount, 1)
	}

	created, err := client.RedeemCode.Create().
		SetCode(code.Code).
		SetNillableOwnerUserID(code.OwnerUserID).
		SetType(code.Type).
		SetValue(code.Value).
		SetStatus(code.Status).
		SetNotes(code.Notes).
		SetValidityDays(code.ValidityDays).
		SetMaxUses(max(code.MaxUses, 1)).
		SetUsedCount(usedCount).
		SetNillableExpiresAt(code.ExpiresAt).
		SetNillableUsedBy(code.UsedBy).
		SetNillableUsedAt(usedAt).
		SetNillableGroupID(code.GroupID).
		Save(ctx)
	if err != nil {
		return err
	}
	if code.UsedBy != nil {
		if _, err := client.RedeemCodeUsage.Create().
			SetRedeemCodeID(created.ID).
			SetUserID(*code.UsedBy).
			SetUsedAt(*usedAt).
			Save(ctx); err != nil {
			return err
		}
	}

	code.ID = created.ID
	code.CreatedAt = created.CreatedAt
	code.UsedAt = created.UsedAt
	code.MaxUses = created.MaxUses
	code.UsedCount = created.UsedCount
	return nil
}

func (r *redeemCodeRepository) CreateBatch(ctx context.Context, codes []service.RedeemCode) error {
	if len(codes) == 0 {
		return nil
	}

	client := clientFromContext(ctx, r.client)
	builders := make([]*dbent.RedeemCodeCreate, 0, len(codes))
	for i := range codes {
		c := &codes[i]
		b := client.RedeemCode.Create().
			SetCode(c.Code).
			SetNillableOwnerUserID(c.OwnerUserID).
			SetType(c.Type).
			SetValue(c.Value).
			SetStatus(c.Status).
			SetNotes(c.Notes).
			SetValidityDays(c.ValidityDays).
			SetMaxUses(max(c.MaxUses, 1)).
			SetUsedCount(c.UsedCount).
			SetNillableExpiresAt(c.ExpiresAt).
			SetNillableUsedBy(c.UsedBy).
			SetNillableUsedAt(c.UsedAt).
			SetNillableGroupID(c.GroupID)
		builders = append(builders, b)
	}

	return client.RedeemCode.CreateBulk(builders...).Exec(ctx)
}

func (r *redeemCodeRepository) GetByID(ctx context.Context, id int64) (*service.RedeemCode, error) {
	m, err := r.client.RedeemCode.Query().
		Where(redeemcode.IDEQ(id)).
		Only(ctx)
	if err != nil {
		if dbent.IsNotFound(err) {
			return nil, service.ErrRedeemCodeNotFound
		}
		return nil, err
	}
	return redeemCodeEntityToService(m), nil
}

func (r *redeemCodeRepository) GetByCode(ctx context.Context, code string) (*service.RedeemCode, error) {
	m, err := r.client.RedeemCode.Query().
		Where(redeemcode.CodeEQ(code)).
		Only(ctx)
	if err != nil {
		if dbent.IsNotFound(err) {
			return nil, service.ErrRedeemCodeNotFound
		}
		return nil, err
	}
	return redeemCodeEntityToService(m), nil
}

func (r *redeemCodeRepository) GetByCodeForUpdate(ctx context.Context, code string) (*service.RedeemCode, error) {
	client := clientFromContext(ctx, r.client)
	m, err := client.RedeemCode.Query().Where(redeemcode.CodeEQ(code)).ForUpdate().Only(ctx)
	if err != nil {
		if dbent.IsNotFound(err) {
			return nil, service.ErrRedeemCodeNotFound
		}
		return nil, err
	}
	return redeemCodeEntityToService(m), nil
}

func (r *redeemCodeRepository) Delete(ctx context.Context, id int64) error {
	_, err := r.client.RedeemCode.Delete().Where(redeemcode.IDEQ(id)).Exec(ctx)
	return err
}

func (r *redeemCodeRepository) List(ctx context.Context, params pagination.PaginationParams) ([]service.RedeemCode, *pagination.PaginationResult, error) {
	return r.ListWithFilters(ctx, params, "", "", "")
}

func (r *redeemCodeRepository) ListWithFilters(ctx context.Context, params pagination.PaginationParams, codeType, status, search string) ([]service.RedeemCode, *pagination.PaginationResult, error) {
	q := r.client.RedeemCode.Query()

	if codeType != "" {
		q = q.Where(redeemcode.TypeEQ(codeType))
	}
	if status != "" {
		now := time.Now()
		switch status {
		case service.StatusExpired:
			q = q.Where(redeemcode.Or(
				redeemcode.StatusEQ(service.StatusExpired),
				redeemcode.And(
					redeemcode.StatusEQ(service.StatusUnused),
					redeemcode.ExpiresAtNotNil(),
					redeemcode.ExpiresAtLTE(now),
				),
			))
		case service.StatusUnused:
			q = q.Where(
				redeemcode.StatusEQ(service.StatusUnused),
				redeemcode.Or(
					redeemcode.ExpiresAtIsNil(),
					redeemcode.ExpiresAtGT(now),
				),
			)
		default:
			q = q.Where(redeemcode.StatusEQ(status))
		}
	}
	if search != "" {
		q = q.Where(
			redeemcode.Or(
				redeemcode.CodeContainsFold(search),
				redeemcode.HasUserWith(user.EmailContainsFold(search)),
			),
		)
	}

	total, err := q.Count(ctx)
	if err != nil {
		return nil, nil, err
	}

	codesQuery := q.
		WithUser().
		WithGroup().
		Offset(params.Offset()).
		Limit(params.Limit())
	for _, order := range redeemCodeListOrder(params) {
		codesQuery = codesQuery.Order(order)
	}

	codes, err := codesQuery.All(ctx)
	if err != nil {
		return nil, nil, err
	}

	outCodes := redeemCodeEntitiesToService(codes)

	return outCodes, paginationResultFromTotal(int64(total), params), nil
}

func redeemCodeListOrder(params pagination.PaginationParams) []func(*entsql.Selector) {
	sortBy := strings.ToLower(strings.TrimSpace(params.SortBy))
	sortOrder := params.NormalizedSortOrder(pagination.SortOrderDesc)

	var field string
	switch sortBy {
	case "type":
		field = redeemcode.FieldType
	case "value":
		field = redeemcode.FieldValue
	case "status":
		field = redeemcode.FieldStatus
	case "used_at":
		field = redeemcode.FieldUsedAt
	case "created_at":
		field = redeemcode.FieldCreatedAt
	case "expires_at":
		field = redeemcode.FieldExpiresAt
	case "code":
		field = redeemcode.FieldCode
	default:
		field = redeemcode.FieldID
	}

	if sortOrder == pagination.SortOrderAsc {
		return []func(*entsql.Selector){dbent.Asc(field), dbent.Asc(redeemcode.FieldID)}
	}
	return []func(*entsql.Selector){dbent.Desc(field), dbent.Desc(redeemcode.FieldID)}
}

func (r *redeemCodeRepository) Update(ctx context.Context, code *service.RedeemCode) error {
	up := r.client.RedeemCode.UpdateOneID(code.ID).
		SetCode(code.Code).
		SetNillableOwnerUserID(code.OwnerUserID).
		SetType(code.Type).
		SetValue(code.Value).
		SetStatus(code.Status).
		SetNotes(code.Notes).
		SetValidityDays(code.ValidityDays).
		SetMaxUses(max(code.MaxUses, 1)).
		SetUsedCount(code.UsedCount)

	if code.UsedBy != nil {
		up.SetUsedBy(*code.UsedBy)
	} else {
		up.ClearUsedBy()
	}
	if code.UsedAt != nil {
		up.SetUsedAt(*code.UsedAt)
	} else {
		up.ClearUsedAt()
	}
	if code.GroupID != nil {
		up.SetGroupID(*code.GroupID)
	} else {
		up.ClearGroupID()
	}
	if code.ExpiresAt != nil {
		up.SetExpiresAt(*code.ExpiresAt)
	} else {
		up.ClearExpiresAt()
	}

	updated, err := up.Save(ctx)
	if err != nil {
		if dbent.IsNotFound(err) {
			return service.ErrRedeemCodeNotFound
		}
		return err
	}
	code.CreatedAt = updated.CreatedAt
	return nil
}

func (r *redeemCodeRepository) BatchUpdate(ctx context.Context, ids []int64, fields service.RedeemCodeBatchUpdateFields) (int64, error) {
	uniqueIDs := make([]int64, 0, len(ids))
	seen := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		uniqueIDs = append(uniqueIDs, id)
	}
	if len(uniqueIDs) == 0 {
		return 0, nil
	}

	if tx := dbent.TxFromContext(ctx); tx != nil {
		return r.batchUpdate(ctx, tx.Client(), uniqueIDs, fields)
	}

	tx, err := r.client.Tx(ctx)
	if err != nil {
		return 0, err
	}
	txCtx := dbent.NewTxContext(ctx, tx)
	defer func() { _ = tx.Rollback() }()

	updated, err := r.batchUpdate(txCtx, tx.Client(), uniqueIDs, fields)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return updated, nil
}

func (r *redeemCodeRepository) batchUpdate(ctx context.Context, client *dbent.Client, ids []int64, fields service.RedeemCodeBatchUpdateFields) (int64, error) {
	existing, err := client.RedeemCode.Query().
		Where(redeemcode.IDIn(ids...)).
		All(ctx)
	if err != nil {
		return 0, err
	}
	if len(existing) != len(ids) {
		return 0, service.ErrRedeemCodeNotFound
	}
	if fields.TouchesUsedSensitiveFields() {
		for _, code := range existing {
			if code.Status == service.StatusUsed {
				return 0, service.ErrRedeemCodeUsed
			}
		}
	}

	up := client.RedeemCode.Update().Where(redeemcode.IDIn(ids...))
	if fields.Status != nil {
		up.SetStatus(*fields.Status)
	}
	if fields.Notes != nil {
		up.SetNotes(*fields.Notes)
	}
	if fields.ExpiresAt.Set {
		if fields.ExpiresAt.Value != nil {
			up.SetExpiresAt(*fields.ExpiresAt.Value)
		} else {
			up.ClearExpiresAt()
		}
	}
	if fields.GroupID.Set {
		if fields.GroupID.Value != nil {
			up.SetGroupID(*fields.GroupID.Value)
		} else {
			up.ClearGroupID()
		}
	}

	affected, err := up.Save(ctx)
	if err != nil {
		return 0, err
	}
	if affected != len(ids) {
		return 0, service.ErrRedeemCodeNotFound
	}
	return int64(affected), nil
}

func (r *redeemCodeRepository) Use(ctx context.Context, id, userID int64) error {
	if tx := dbent.TxFromContext(ctx); tx != nil {
		return r.use(ctx, tx.Client(), id, userID)
	}
	tx, err := r.client.Tx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	txCtx := dbent.NewTxContext(ctx, tx)
	if err := r.use(txCtx, tx.Client(), id, userID); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *redeemCodeRepository) use(ctx context.Context, client *dbent.Client, id, userID int64) error {
	now := time.Now()
	code, err := client.RedeemCode.Query().Where(redeemcode.IDEQ(id)).ForUpdate().Only(ctx)
	if err != nil {
		if dbent.IsNotFound(err) {
			return service.ErrRedeemCodeNotFound
		}
		return err
	}
	maxUses := max(code.MaxUses, 1)
	if code.Status != service.StatusUnused || code.UsedCount >= maxUses {
		return service.ErrRedeemCodeUsed
	}
	if _, err := client.RedeemCodeUsage.Create().
		SetRedeemCodeID(id).
		SetUserID(userID).
		SetUsedAt(now).
		Save(ctx); err != nil {
		if dbent.IsConstraintError(err) {
			return service.ErrRedeemCodeUserUsed
		}
		return err
	}
	update := client.RedeemCode.Update().
		Where(redeemcode.IDEQ(id), redeemcode.StatusEQ(service.StatusUnused), redeemcode.UsedCountEQ(code.UsedCount)).
		AddUsedCount(1).
		SetUsedBy(userID).
		SetUsedAt(now)
	if code.UsedCount+1 >= maxUses {
		update.SetStatus(service.StatusUsed)
	}
	affected, err := update.Save(ctx)
	if err != nil {
		return err
	}
	if affected == 0 {
		return service.ErrRedeemCodeUsed
	}
	return nil
}

func (r *redeemCodeRepository) GetUsageByRedeemCodeAndUser(ctx context.Context, redeemCodeID, userID int64) (*service.RedeemCodeUsage, error) {
	client := clientFromContext(ctx, r.client)
	usage, err := client.RedeemCodeUsage.Query().
		Where(redeemcodeusage.RedeemCodeIDEQ(redeemCodeID), redeemcodeusage.UserIDEQ(userID)).
		Only(ctx)
	if err != nil {
		if dbent.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return redeemCodeUsageEntityToService(usage), nil
}

func (r *redeemCodeRepository) ListUsagesByRedeemCode(ctx context.Context, redeemCodeID int64, params pagination.PaginationParams) ([]service.RedeemCodeUsage, *pagination.PaginationResult, error) {
	q := r.client.RedeemCodeUsage.Query().Where(redeemcodeusage.RedeemCodeIDEQ(redeemCodeID))
	total, err := q.Clone().Count(ctx)
	if err != nil {
		return nil, nil, err
	}
	rows, err := q.WithUser().Offset(params.Offset()).Limit(params.Limit()).Order(dbent.Desc(redeemcodeusage.FieldUsedAt)).All(ctx)
	if err != nil {
		return nil, nil, err
	}
	out := make([]service.RedeemCodeUsage, 0, len(rows))
	for _, row := range rows {
		if item := redeemCodeUsageEntityToService(row); item != nil {
			out = append(out, *item)
		}
	}
	return out, paginationResultFromTotal(int64(total), params), nil
}

func (r *redeemCodeRepository) ListByUser(ctx context.Context, userID int64, limit int) ([]service.RedeemCode, error) {
	if limit <= 0 {
		limit = 10
	}

	usages, err := r.client.RedeemCodeUsage.Query().
		Where(redeemcodeusage.UserIDEQ(userID)).
		WithRedeemCode(func(q *dbent.RedeemCodeQuery) { q.WithGroup() }).
		Order(dbent.Desc(redeemcodeusage.FieldUsedAt)).
		Limit(limit).
		All(ctx)
	if err != nil {
		return nil, err
	}
	return redeemCodeUsagesToRedeemCodes(usages), nil
}

// ListByUserPaginated returns paginated balance/concurrency history for a user.
// Supports optional type filter (e.g. "balance", "admin_balance", "concurrency", "admin_concurrency", "subscription").
func (r *redeemCodeRepository) ListByUserPaginated(ctx context.Context, userID int64, params pagination.PaginationParams, codeType string) ([]service.RedeemCode, *pagination.PaginationResult, error) {
	q := r.client.RedeemCodeUsage.Query().
		Where(redeemcodeusage.UserIDEQ(userID))

	// Optional type filter
	if codeType != "" {
		q = q.Where(redeemcodeusage.HasRedeemCodeWith(redeemcode.TypeEQ(codeType)))
	}

	total, err := q.Count(ctx)
	if err != nil {
		return nil, nil, err
	}

	usages, err := q.
		WithRedeemCode(func(codeQuery *dbent.RedeemCodeQuery) { codeQuery.WithGroup() }).
		Offset(params.Offset()).
		Limit(params.Limit()).
		Order(dbent.Desc(redeemcodeusage.FieldUsedAt)).
		All(ctx)
	if err != nil {
		return nil, nil, err
	}

	return redeemCodeUsagesToRedeemCodes(usages), paginationResultFromTotal(int64(total), params), nil
}

// SumPositiveBalanceByUser returns total recharged amount (sum of value > 0 where type is balance/admin_balance).
func (r *redeemCodeRepository) SumPositiveBalanceByUser(ctx context.Context, userID int64) (float64, error) {
	var result []struct {
		Sum float64 `json:"sum"`
	}
	err := r.client.RedeemCode.Query().
		Where(
			redeemcode.UsedByEQ(userID),
			redeemcode.ValueGT(0),
			redeemcode.TypeIn("balance", "admin_balance"),
		).
		Aggregate(dbent.As(dbent.Sum(redeemcode.FieldValue), "sum")).
		Scan(ctx, &result)
	if err != nil {
		return 0, err
	}
	if len(result) == 0 {
		return 0, nil
	}
	return result[0].Sum, nil
}

func redeemCodeEntityToService(m *dbent.RedeemCode) *service.RedeemCode {
	if m == nil {
		return nil
	}
	out := &service.RedeemCode{
		ID:           m.ID,
		Code:         m.Code,
		OwnerUserID:  m.OwnerUserID,
		Type:         m.Type,
		Value:        m.Value,
		Status:       m.Status,
		UsedBy:       m.UsedBy,
		UsedAt:       m.UsedAt,
		Notes:        derefString(m.Notes),
		CreatedAt:    m.CreatedAt,
		ExpiresAt:    m.ExpiresAt,
		GroupID:      m.GroupID,
		ValidityDays: m.ValidityDays,
		MaxUses:      m.MaxUses,
		UsedCount:    m.UsedCount,
	}
	if m.Edges.User != nil {
		out.User = userEntityToService(m.Edges.User)
	}
	if m.Edges.Group != nil {
		out.Group = groupEntityToService(m.Edges.Group)
	}
	return out
}

func redeemCodeUsageEntityToService(m *dbent.RedeemCodeUsage) *service.RedeemCodeUsage {
	if m == nil {
		return nil
	}
	out := &service.RedeemCodeUsage{
		ID:           m.ID,
		RedeemCodeID: m.RedeemCodeID,
		UserID:       m.UserID,
		UsedAt:       m.UsedAt,
	}
	if m.Edges.User != nil {
		out.User = userEntityToService(m.Edges.User)
	}
	return out
}

func redeemCodeUsagesToRedeemCodes(usages []*dbent.RedeemCodeUsage) []service.RedeemCode {
	out := make([]service.RedeemCode, 0, len(usages))
	for _, usage := range usages {
		if usage == nil || usage.Edges.RedeemCode == nil {
			continue
		}
		code := redeemCodeEntityToService(usage.Edges.RedeemCode)
		if code == nil {
			continue
		}
		usedAt := usage.UsedAt
		code.UsedAt = &usedAt
		code.UsedBy = &usage.UserID
		out = append(out, *code)
	}
	return out
}

func redeemCodeEntitiesToService(models []*dbent.RedeemCode) []service.RedeemCode {
	out := make([]service.RedeemCode, 0, len(models))
	for i := range models {
		if s := redeemCodeEntityToService(models[i]); s != nil {
			out = append(out, *s)
		}
	}
	return out
}
