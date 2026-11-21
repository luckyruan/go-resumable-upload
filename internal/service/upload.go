package service

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/interview/go-resumable-upload/internal/domain"
	"github.com/interview/go-resumable-upload/internal/store"
)

type Service struct {
	store   *store.Store
	disk    *Disk
	maxSize int64
}

type CancelResult struct {
	Upload         domain.Upload `json:"upload"`
	CleanupPending bool          `json:"cleanupPending"`
}

func New(metadata *store.Store, disk *Disk, maxSize int64) *Service {
	return &Service{store: metadata, disk: disk, maxSize: maxSize}
}

func (service *Service) Health(ctx context.Context) error { return service.store.Ping(ctx) }

func (service *Service) Create(ctx context.Context, tenant string, request domain.CreateRequest) (domain.Upload, error) {
	request, total, err := domain.ValidateCreate(request, service.maxSize)
	if err != nil {
		return domain.Upload{}, err
	}
	id, err := domain.NewID()
	if err != nil {
		return domain.Upload{}, err
	}
	return service.store.Create(ctx, domain.Upload{
		ID: id, TenantID: tenant, Filename: request.Filename, Size: request.Size,
		ChunkSize: request.ChunkSize, TotalParts: total, SHA256: request.SHA256,
	})
}

func (service *Service) Get(ctx context.Context, tenant, id string) (domain.Upload, error) {
	if !domain.ValidID(id) {
		return domain.Upload{}, domain.ErrInvalid
	}
	return service.store.GetWithParts(ctx, tenant, id)
}

func (service *Service) PutPart(ctx context.Context, tenant, id string, number int, body io.Reader, claimedHash string) (domain.PartResult, error) {
	if !domain.ValidID(id) {
		return domain.PartResult{}, domain.ErrInvalid
	}
	if claimedHash != "" {
		var err error
		claimedHash, err = domain.NormalizeSHA256(claimedHash)
		if err != nil {
			return domain.PartResult{}, err
		}
	}
	upload, err := service.store.Get(ctx, tenant, id)
	if err != nil {
		return domain.PartResult{}, err
	}
	if upload.Status != domain.Uploading {
		return domain.PartResult{}, domain.ErrConflict
	}
	expected, err := upload.ExpectedPartSize(number)
	if err != nil {
		return domain.PartResult{}, err
	}
	staged, err := service.disk.Stage(ctx, body, expected, claimedHash)
	if err != nil {
		return domain.PartResult{}, err
	}
	defer os.Remove(staged.path)
	part := domain.Part{PartNumber: number, Size: staged.size, SHA256: staged.sha256}
	locked, err := service.store.Lock(ctx, tenant, id)
	if err != nil {
		return domain.PartResult{}, err
	}
	defer locked.Rollback()
	if locked.Upload.Status != domain.Uploading {
		return domain.PartResult{}, domain.ErrConflict
	}
	parts, err := locked.Parts(ctx)
	if err != nil {
		return domain.PartResult{}, err
	}
	for _, existing := range parts {
		if existing.PartNumber != number {
			continue
		}
		if existing.SHA256 != part.SHA256 || existing.Size != part.Size {
			return domain.PartResult{}, fmt.Errorf("%w: part already contains different bytes", domain.ErrConflict)
		}
		if err := service.disk.verifyFile(ctx, service.disk.partPath(id, existing), existing.Size, existing.SHA256); err != nil {
			return domain.PartResult{}, err
		}
		return domain.PartResult{Part: existing, AlreadyExisted: true}, locked.Commit(ctx)
	}
	if err := service.disk.PublishPart(ctx, id, part, staged); err != nil {
		return domain.PartResult{}, err
	}
	if err := locked.InsertPart(ctx, part); err != nil {
		return domain.PartResult{}, err
	}
	// A commit error can mean an unknown outcome. Never remove the published part here.
	return domain.PartResult{Part: part}, locked.Commit(ctx)
}

func (service *Service) Complete(ctx context.Context, tenant, id string) (domain.Upload, error) {
	if !domain.ValidID(id) {
		return domain.Upload{}, domain.ErrInvalid
	}
	locked, err := service.store.Lock(ctx, tenant, id)
	if err != nil {
		return domain.Upload{}, err
	}
	defer locked.Rollback()
	if locked.Upload.Status == domain.Canceled {
		return domain.Upload{}, domain.ErrConflict
	}
	parts, err := locked.Parts(ctx)
	if err != nil {
		return domain.Upload{}, err
	}
	locked.Upload.UploadedParts = parts
	if locked.Upload.Status == domain.Completed {
		file, err := service.disk.OpenCompleted(locked.Upload)
		if err != nil {
			return domain.Upload{}, err
		}
		_ = file.Close()
		return locked.Upload, locked.Commit(ctx)
	}
	if err := validateParts(locked.Upload, parts); err != nil {
		return domain.Upload{}, err
	}
	// A prior attempt may have published a valid file before losing the DB commit response.
	recovered, err := service.disk.RecoverCompleted(ctx, locked.Upload)
	if err != nil {
		return domain.Upload{}, err
	}
	if !recovered {
		if err := service.disk.Merge(ctx, locked.Upload, parts); err != nil {
			return domain.Upload{}, err
		}
	}
	if err := locked.MarkCompleted(ctx); err != nil {
		return domain.Upload{}, err
	}
	// Keep chunks and canonical output when commit fails: the result might already be durable.
	// This learning version also retains successful chunks until a future cleanup policy exists.
	return locked.Upload, locked.Commit(ctx)
}

func validateParts(upload domain.Upload, parts []domain.Part) error {
	if len(parts) != upload.TotalParts {
		return fmt.Errorf("%w: some parts are missing", domain.ErrConflict)
	}
	for index, part := range parts {
		if part.PartNumber != index+1 {
			return fmt.Errorf("%w: part numbers are incomplete", domain.ErrConflict)
		}
		expected, err := upload.ExpectedPartSize(part.PartNumber)
		if err != nil || part.Size != expected {
			return domain.ErrIntegrity
		}
	}
	return nil
}

func (service *Service) Cancel(ctx context.Context, tenant, id string) (CancelResult, error) {
	if !domain.ValidID(id) {
		return CancelResult{}, domain.ErrInvalid
	}
	locked, err := service.store.Lock(ctx, tenant, id)
	if err != nil {
		return CancelResult{}, err
	}
	defer locked.Rollback()
	if locked.Upload.Status == domain.Completed {
		return CancelResult{}, domain.ErrConflict
	}
	if err := locked.MarkCanceled(ctx); err != nil {
		return CancelResult{}, err
	}
	if err := locked.Commit(ctx); err != nil {
		// Unknown commit: check DB with a later GET/retry, do not delete files speculatively.
		return CancelResult{}, err
	}
	result := CancelResult{Upload: locked.Upload}
	if err := service.disk.RemoveCanceled(id); err != nil {
		result.CleanupPending = true
	}
	return result, nil
}

func (service *Service) Download(ctx context.Context, tenant, id string) (domain.Upload, *os.File, error) {
	if !domain.ValidID(id) {
		return domain.Upload{}, nil, domain.ErrInvalid
	}
	upload, err := service.store.Get(ctx, tenant, id)
	if err != nil {
		return upload, nil, err
	}
	if upload.Status != domain.Completed {
		return upload, nil, domain.ErrConflict
	}
	file, err := service.disk.OpenCompleted(upload)
	return upload, file, err
}
