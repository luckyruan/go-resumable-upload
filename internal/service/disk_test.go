package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/interview/go-resumable-upload/internal/domain"
)

func sum(data []byte) string {
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func TestStageChecksLengthHashAndCleansFailures(t *testing.T) {
	tests := []struct {
		name     string
		data     []byte
		expected int64
		hash     string
		good     bool
	}{
		{"exact", []byte("abc"), 3, sum([]byte("abc")), true},
		{"short", []byte("ab"), 3, "", false},
		{"long", []byte("abcd"), 3, "", false},
		{"bad checksum", []byte("abc"), 3, strings.Repeat("0", 64), false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			disk, err := NewDisk(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			stage, err := disk.Stage(context.Background(), bytes.NewReader(test.data), test.expected, test.hash)
			if test.good {
				if err != nil || stage.size != test.expected || stage.sha256 != sum(test.data) {
					t.Fatalf("stage=%+v err=%v", stage, err)
				}
				return
			}
			if !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("want invalid, got %v", err)
			}
			entries, err := os.ReadDir(filepath.Join(disk.root, "staging"))
			if err != nil || len(entries) != 0 {
				t.Fatalf("failed staging leaked files: %v, %v", entries, err)
			}
		})
	}
}

func TestMergeAndRecoverPublishedFile(t *testing.T) {
	disk, err := NewDisk(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	upload := domain.Upload{ID: strings.Repeat("a", 32), Size: 7, ChunkSize: 3, TotalParts: 3, SHA256: sum([]byte("abcdefg"))}
	pieces := [][]byte{[]byte("abc"), []byte("def"), []byte("g")}
	parts := make([]domain.Part, 0, len(pieces))
	for index, data := range pieces {
		stage, err := disk.Stage(ctx, bytes.NewReader(data), int64(len(data)), "")
		if err != nil {
			t.Fatal(err)
		}
		part := domain.Part{PartNumber: index + 1, Size: int64(len(data)), SHA256: stage.sha256}
		if err := disk.PublishPart(ctx, upload.ID, part, stage); err != nil {
			t.Fatal(err)
		}
		_ = os.Remove(stage.path)
		parts = append(parts, part)
	}
	if err := disk.Merge(ctx, upload, parts); err != nil {
		t.Fatal(err)
	}
	recovered, err := disk.RecoverCompleted(ctx, upload)
	if err != nil || !recovered {
		t.Fatalf("could not recover pre-DB-commit file: %v, %v", recovered, err)
	}
	file, err := disk.OpenCompleted(upload)
	if err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	wrong := upload
	wrong.SHA256 = strings.Repeat("0", 64)
	if _, err := disk.RecoverCompleted(ctx, wrong); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatalf("accepted wrong whole hash: %v", err)
	}
	if err := os.WriteFile(disk.partPath(upload.ID, parts[0]), []byte("xyz"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := disk.Merge(ctx, upload, parts); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatalf("accepted corrupt part: %v", err)
	}
	// A failed rebuild must not delete or replace an already valid published file.
	if recovered, err := disk.RecoverCompleted(ctx, upload); err != nil || !recovered {
		t.Fatalf("valid file lost: %v", err)
	}
}

func TestStageContextCancellation(t *testing.T) {
	disk, err := NewDisk(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := disk.Stage(ctx, strings.NewReader("abc"), 3, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("want canceled, got %v", err)
	}
	if err := disk.RemoveCanceled("../"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatal("unsafe cleanup target accepted")
	}
}

func TestValidatePartsRequiresContinuousCompleteSequence(t *testing.T) {
	upload := domain.Upload{Size: 7, ChunkSize: 3, TotalParts: 3}
	parts := []domain.Part{{PartNumber: 1, Size: 3}, {PartNumber: 2, Size: 3}, {PartNumber: 3, Size: 1}}
	if err := validateParts(upload, parts); err != nil {
		t.Fatal(err)
	}
	if err := validateParts(upload, parts[:2]); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("accepted missing part: %v", err)
	}
	swapped := []domain.Part{parts[1], parts[0], parts[2]}
	if err := validateParts(upload, swapped); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("accepted out-of-order metadata: %v", err)
	}
	parts[2].Size = 3
	if err := validateParts(upload, parts); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatalf("accepted wrong final length: %v", err)
	}
}
