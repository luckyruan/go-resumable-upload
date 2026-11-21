package domain

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
)

const (
	Uploading          = "UPLOADING"
	Completed          = "COMPLETED"
	Canceled           = "CANCELED"
	MinChunkSize int64 = 64 * 1024
	MaxChunkSize int64 = 16 * 1024 * 1024
	MaxParts           = 10000
)

var (
	ErrNotFound  = errors.New("not found")
	ErrInvalid   = errors.New("invalid request")
	ErrConflict  = errors.New("upload conflict")
	ErrIntegrity = errors.New("integrity check failed")
	ErrStorage   = errors.New("storage unavailable")
)

type Upload struct {
	ID            string     `json:"id"`
	TenantID      string     `json:"-"`
	Filename      string     `json:"filename"`
	Size          int64      `json:"size"`
	ChunkSize     int64      `json:"chunkSize"`
	TotalParts    int        `json:"totalParts"`
	SHA256        string     `json:"sha256"`
	Status        string     `json:"status"`
	CreatedAt     time.Time  `json:"createdAt"`
	CompletedAt   *time.Time `json:"completedAt,omitempty"`
	UploadedParts []Part     `json:"uploadedParts"`
	DownloadPath  string     `json:"downloadPath,omitempty"`
}

type Part struct {
	PartNumber int    `json:"partNumber"`
	Size       int64  `json:"size"`
	SHA256     string `json:"sha256"`
}

type CreateRequest struct {
	Filename  string `json:"filename"`
	Size      int64  `json:"size"`
	ChunkSize int64  `json:"chunkSize"`
	SHA256    string `json:"sha256"`
}

type PartResult struct {
	Part
	AlreadyExisted bool `json:"alreadyExisted"`
}

func NewID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func ValidID(id string) bool {
	if len(id) != 32 || id != strings.ToLower(id) {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

func NormalizeSHA256(value string) (string, error) {
	if len(value) != 64 {
		return "", fmt.Errorf("%w: SHA256 must contain 64 hex digits", ErrInvalid)
	}
	if _, err := hex.DecodeString(value); err != nil {
		return "", fmt.Errorf("%w: invalid SHA256", ErrInvalid)
	}
	return strings.ToLower(value), nil
}

func ValidateCreate(req CreateRequest, maxSize int64) (CreateRequest, int, error) {
	if strings.TrimSpace(req.Filename) == "" || len(req.Filename) > 255 {
		return req, 0, fmt.Errorf("%w: filename length", ErrInvalid)
	}
	for _, char := range req.Filename {
		if unicode.IsControl(char) {
			return req, 0, fmt.Errorf("%w: control character in filename", ErrInvalid)
		}
	}
	if req.Size <= 0 || req.Size > maxSize {
		return req, 0, fmt.Errorf("%w: file size", ErrInvalid)
	}
	if req.ChunkSize < MinChunkSize || req.ChunkSize > MaxChunkSize {
		return req, 0, fmt.Errorf("%w: chunk size", ErrInvalid)
	}
	total := (req.Size-1)/req.ChunkSize + 1
	if total > MaxParts {
		return req, 0, fmt.Errorf("%w: too many parts", ErrInvalid)
	}
	hash, err := NormalizeSHA256(req.SHA256)
	if err != nil {
		return req, 0, err
	}
	req.SHA256 = hash
	return req, int(total), nil
}

func (upload Upload) ExpectedPartSize(number int) (int64, error) {
	if number < 1 || number > upload.TotalParts {
		return 0, fmt.Errorf("%w: part number", ErrInvalid)
	}
	if number < upload.TotalParts {
		return upload.ChunkSize, nil
	}
	return upload.Size - int64(upload.TotalParts-1)*upload.ChunkSize, nil
}

func (upload *Upload) SetDownloadPath() {
	if upload.Status == Completed {
		upload.DownloadPath = "/api/v1/files/" + upload.ID
	}
}
