package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"github.com/interview/go-resumable-upload/internal/domain"
)

const bufferSize = 64 * 1024

type Disk struct{ root string }

type stagedPart struct {
	path   string
	size   int64
	sha256 string
}

func NewDisk(root string) (*Disk, error) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	for _, name := range []string{"staging", "uploads", "files"} {
		if err := os.MkdirAll(filepath.Join(absolute, name), 0700); err != nil {
			return nil, err
		}
	}
	return &Disk{root: absolute}, nil
}

func (disk *Disk) Stage(ctx context.Context, body io.Reader, expected int64, claimedHash string) (*stagedPart, error) {
	file, err := os.CreateTemp(filepath.Join(disk.root, "staging"), "part-*")
	if err != nil {
		return nil, fmt.Errorf("%w: create temporary file: %v", domain.ErrStorage, err)
	}
	keep := false
	defer func() {
		_ = file.Close()
		if !keep {
			_ = os.Remove(file.Name())
		}
	}()
	hash := sha256.New()
	reader := io.LimitReader(contextReader{ctx: ctx, reader: body}, expected+1)
	size, err := io.CopyBuffer(io.MultiWriter(file, hash), reader, make([]byte, bufferSize))
	if err != nil {
		return nil, err
	}
	if size != expected {
		return nil, fmt.Errorf("%w: actual part length differs from declared length", domain.ErrInvalid)
	}
	sum := hex.EncodeToString(hash.Sum(nil))
	if claimedHash != "" && claimedHash != sum {
		return nil, fmt.Errorf("%w: part hash mismatch", domain.ErrInvalid)
	}
	if err := file.Sync(); err != nil {
		return nil, fmt.Errorf("%w: sync part: %v", domain.ErrStorage, err)
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("%w: close part: %v", domain.ErrStorage, err)
	}
	keep = true
	return &stagedPart{path: file.Name(), size: size, sha256: sum}, nil
}

func (disk *Disk) partPath(id string, part domain.Part) string {
	return filepath.Join(disk.root, "uploads", id, "chunks",
		strconv.Itoa(part.PartNumber)+"-"+part.SHA256+".blob")
}

func (disk *Disk) completedPath(id string) string {
	return filepath.Join(disk.root, "files", id+".blob")
}

func (disk *Disk) PublishPart(ctx context.Context, id string, part domain.Part, staged *stagedPart) error {
	if !domain.ValidID(id) {
		return domain.ErrInvalid
	}
	destination := disk.partPath(id, part)
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return fmt.Errorf("%w: create part directory: %v", domain.ErrStorage, err)
	}
	// A previous failed/unknown database commit may have left this exact content on disk.
	if _, err := os.Stat(destination); err == nil {
		return disk.verifyFile(ctx, destination, part.Size, part.SHA256)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: inspect part: %v", domain.ErrStorage, err)
	}
	if err := os.Rename(staged.path, destination); err != nil {
		return fmt.Errorf("%w: publish part: %v", domain.ErrStorage, err)
	}
	return nil
}

func (disk *Disk) verifyFile(ctx context.Context, path string, expectedSize int64, expectedHash string) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("%w: open stored file: %v", domain.ErrStorage, err)
	}
	defer file.Close()
	hash := sha256.New()
	count, err := io.CopyBuffer(hash,
		io.LimitReader(contextReader{ctx: ctx, reader: file}, expectedSize+1), make([]byte, bufferSize))
	if err != nil {
		return err
	}
	if count != expectedSize || hex.EncodeToString(hash.Sum(nil)) != expectedHash {
		return domain.ErrIntegrity
	}
	return nil
}

func (disk *Disk) RecoverCompleted(ctx context.Context, upload domain.Upload) (bool, error) {
	path := disk.completedPath(upload.ID)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("%w: inspect completed file: %v", domain.ErrStorage, err)
	}
	if err := disk.verifyFile(ctx, path, upload.Size, upload.SHA256); err != nil {
		return false, err
	}
	return true, nil
}

func (disk *Disk) Merge(ctx context.Context, upload domain.Upload, parts []domain.Part) error {
	if !domain.ValidID(upload.ID) {
		return domain.ErrInvalid
	}
	output, err := os.CreateTemp(filepath.Join(disk.root, "files"), "merge-*")
	if err != nil {
		return fmt.Errorf("%w: create merge output: %v", domain.ErrStorage, err)
	}
	defer func() {
		_ = output.Close()
		_ = os.Remove(output.Name())
	}()
	wholeHash := sha256.New()
	var total int64
	buffer := make([]byte, bufferSize)
	for _, part := range parts {
		expected, err := upload.ExpectedPartSize(part.PartNumber)
		if err != nil || part.Size != expected {
			return domain.ErrIntegrity
		}
		file, err := os.Open(disk.partPath(upload.ID, part))
		if err != nil {
			return fmt.Errorf("%w: open part: %v", domain.ErrStorage, err)
		}
		partHash := sha256.New()
		reader := io.LimitReader(contextReader{ctx: ctx, reader: file}, expected+1)
		count, copyErr := io.CopyBuffer(io.MultiWriter(output, wholeHash, partHash), reader, buffer)
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return fmt.Errorf("%w: close part: %v", domain.ErrStorage, closeErr)
		}
		if count != expected || hex.EncodeToString(partHash.Sum(nil)) != part.SHA256 {
			return domain.ErrIntegrity
		}
		total += count
	}
	if total != upload.Size || hex.EncodeToString(wholeHash.Sum(nil)) != upload.SHA256 {
		return domain.ErrIntegrity
	}
	if err := output.Sync(); err != nil {
		return fmt.Errorf("%w: sync merge output: %v", domain.ErrStorage, err)
	}
	if err := output.Close(); err != nil {
		return fmt.Errorf("%w: close merge output: %v", domain.ErrStorage, err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(output.Name(), disk.completedPath(upload.ID)); err != nil {
		return fmt.Errorf("%w: publish completed file: %v", domain.ErrStorage, err)
	}
	return nil
}

func (disk *Disk) OpenCompleted(upload domain.Upload) (*os.File, error) {
	if !domain.ValidID(upload.ID) {
		return nil, domain.ErrInvalid
	}
	file, err := os.Open(disk.completedPath(upload.ID))
	if err != nil {
		return nil, fmt.Errorf("%w: open completed file: %v", domain.ErrStorage, err)
	}
	stat, err := file.Stat()
	if err != nil || !stat.Mode().IsRegular() || stat.Size() != upload.Size {
		_ = file.Close()
		return nil, domain.ErrStorage
	}
	return file, nil
}

func (disk *Disk) RemoveCanceled(id string) error {
	if !domain.ValidID(id) {
		return domain.ErrInvalid
	}
	// Both targets are generated from a validated internal ID, never a user filename.
	if err := os.RemoveAll(filepath.Join(disk.root, "uploads", id)); err != nil {
		return err
	}
	if err := os.Remove(disk.completedPath(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader contextReader) Read(buffer []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(buffer)
}
