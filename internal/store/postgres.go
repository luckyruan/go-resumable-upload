package store

import (
	"context"
	"errors"
	"time"

	"github.com/interview/go-resumable-upload/internal/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const uploadColumns = "id,tenant_id,filename,size,chunk_size,total_parts,sha256,status,created_at,completed_at"

type Store struct {
	pool *pgxpool.Pool
}

func Open(ctx context.Context, connection string) (*Store, error) {
	config, err := pgxpool.ParseConfig(connection)
	if err != nil {
		return nil, err
	}
	config.MaxConns = 10
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &Store{pool: pool}, nil
}

func (store *Store) Close()                         { store.pool.Close() }
func (store *Store) Ping(ctx context.Context) error { return store.pool.Ping(ctx) }

func scanUpload(row pgx.Row) (domain.Upload, error) {
	upload := domain.Upload{UploadedParts: []domain.Part{}}
	err := row.Scan(&upload.ID, &upload.TenantID, &upload.Filename, &upload.Size,
		&upload.ChunkSize, &upload.TotalParts, &upload.SHA256, &upload.Status,
		&upload.CreatedAt, &upload.CompletedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return upload, domain.ErrNotFound
	}
	upload.SetDownloadPath()
	return upload, err
}

func (store *Store) Create(ctx context.Context, upload domain.Upload) (domain.Upload, error) {
	return scanUpload(store.pool.QueryRow(ctx, `
		INSERT INTO uploads(id,tenant_id,filename,size,chunk_size,total_parts,sha256)
		VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING `+uploadColumns,
		upload.ID, upload.TenantID, upload.Filename, upload.Size, upload.ChunkSize,
		upload.TotalParts, upload.SHA256))
}

func (store *Store) Get(ctx context.Context, tenant, id string) (domain.Upload, error) {
	return scanUpload(store.pool.QueryRow(ctx, "SELECT "+uploadColumns+
		" FROM uploads WHERE id=$1 AND tenant_id=$2", id, tenant))
}

func (store *Store) GetWithParts(ctx context.Context, tenant, id string) (domain.Upload, error) {
	// A repeatable snapshot prevents mixing a pre-cancel state with a post-cancel part list.
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return domain.Upload{}, err
	}
	defer rollback(tx)
	upload, err := scanUpload(tx.QueryRow(ctx, "SELECT "+uploadColumns+
		" FROM uploads WHERE id=$1 AND tenant_id=$2", id, tenant))
	if err != nil {
		return upload, err
	}
	upload.UploadedParts, err = listParts(ctx, tx, id)
	if err != nil {
		return upload, err
	}
	return upload, tx.Commit(ctx)
}

type LockedUpload struct {
	tx     pgx.Tx
	Upload domain.Upload
}

func (store *Store) Lock(ctx context.Context, tenant, id string) (*LockedUpload, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	upload, err := scanUpload(tx.QueryRow(ctx, "SELECT "+uploadColumns+
		" FROM uploads WHERE id=$1 AND tenant_id=$2 FOR UPDATE", id, tenant))
	if err != nil {
		rollback(tx)
		return nil, err
	}
	return &LockedUpload{tx: tx, Upload: upload}, nil
}

func (locked *LockedUpload) Parts(ctx context.Context) ([]domain.Part, error) {
	return listParts(ctx, locked.tx, locked.Upload.ID)
}

func listParts(ctx context.Context, tx pgx.Tx, id string) ([]domain.Part, error) {
	rows, err := tx.Query(ctx,
		"SELECT part_number,size,sha256 FROM upload_chunks WHERE upload_id=$1 ORDER BY part_number", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	parts := []domain.Part{}
	for rows.Next() {
		var part domain.Part
		if err := rows.Scan(&part.PartNumber, &part.Size, &part.SHA256); err != nil {
			return nil, err
		}
		parts = append(parts, part)
	}
	return parts, rows.Err()
}

func (locked *LockedUpload) InsertPart(ctx context.Context, part domain.Part) error {
	_, err := locked.tx.Exec(ctx,
		"INSERT INTO upload_chunks(upload_id,part_number,size,sha256) VALUES($1,$2,$3,$4)",
		locked.Upload.ID, part.PartNumber, part.Size, part.SHA256)
	return err
}

func (locked *LockedUpload) MarkCompleted(ctx context.Context) error {
	var completed time.Time
	err := locked.tx.QueryRow(ctx,
		"UPDATE uploads SET status='COMPLETED',completed_at=now() WHERE id=$1 RETURNING completed_at",
		locked.Upload.ID).Scan(&completed)
	if err == nil {
		locked.Upload.Status = domain.Completed
		locked.Upload.CompletedAt = &completed
		locked.Upload.SetDownloadPath()
	}
	return err
}

func (locked *LockedUpload) MarkCanceled(ctx context.Context) error {
	if _, err := locked.tx.Exec(ctx, "UPDATE uploads SET status='CANCELED' WHERE id=$1", locked.Upload.ID); err != nil {
		return err
	}
	if _, err := locked.tx.Exec(ctx, "DELETE FROM upload_chunks WHERE upload_id=$1", locked.Upload.ID); err != nil {
		return err
	}
	locked.Upload.Status = domain.Canceled
	locked.Upload.UploadedParts = []domain.Part{}
	return nil
}

func (locked *LockedUpload) Commit(ctx context.Context) error { return locked.tx.Commit(ctx) }
func (locked *LockedUpload) Rollback()                        { rollback(locked.tx) }

func rollback(tx pgx.Tx) {
	cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = tx.Rollback(cleanup)
}
