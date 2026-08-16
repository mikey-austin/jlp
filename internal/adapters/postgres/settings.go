package postgres

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mikeyaustin/jlp/internal/adapters/postgres/sqlcgen"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// SettingsRepository persists operator overrides for AI model/effort
// selection (Phase 4 Task S) in the global (not identity-scoped)
// app_settings key/value table.
type SettingsRepository struct{ q *sqlcgen.Queries }

func NewSettingsRepository(pool *pgxpool.Pool) *SettingsRepository {
	return &SettingsRepository{q: sqlcgen.New(pool)}
}

func (r *SettingsRepository) Set(ctx context.Context, key, value string) error {
	return r.q.UpsertSetting(ctx, sqlcgen.UpsertSettingParams{Key: key, Value: value})
}

func (r *SettingsRepository) Delete(ctx context.Context, key string) error {
	return r.q.DeleteSetting(ctx, key)
}

func (r *SettingsRepository) List(ctx context.Context) ([]storage.AppSetting, error) {
	rows, err := r.q.ListSettings(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]storage.AppSetting, 0, len(rows))
	for _, row := range rows {
		out = append(out, storage.AppSetting{Key: row.Key, Value: row.Value})
	}
	return out, nil
}
