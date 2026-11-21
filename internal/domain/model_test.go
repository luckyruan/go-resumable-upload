package domain

import (
	"errors"
	"math"
	"strings"
	"testing"
)

func TestExpectedPartSize(t *testing.T) {
	tests := []struct {
		name    string
		upload  Upload
		number  int
		want    int64
		invalid bool
	}{
		{name: "full first", upload: Upload{Size: 9, ChunkSize: 4, TotalParts: 3}, number: 1, want: 4},
		{name: "short final", upload: Upload{Size: 9, ChunkSize: 4, TotalParts: 3}, number: 3, want: 1},
		{name: "exact multiple", upload: Upload{Size: 8, ChunkSize: 4, TotalParts: 2}, number: 2, want: 4},
		{name: "zero part", upload: Upload{Size: 8, ChunkSize: 4, TotalParts: 2}, number: 0, invalid: true},
		{name: "extra part", upload: Upload{Size: 8, ChunkSize: 4, TotalParts: 2}, number: 3, invalid: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			actual, err := test.upload.ExpectedPartSize(test.number)
			if test.invalid {
				if !errors.Is(err, ErrInvalid) {
					t.Fatalf("expected invalid, got %v", err)
				}
				return
			}
			if err != nil || actual != test.want {
				t.Fatalf("got %d, %v; want %d", actual, err, test.want)
			}
		})
	}
}

func TestValidateCreateLimits(t *testing.T) {
	valid := CreateRequest{Filename: "项目.bin", Size: MinChunkSize + 1, ChunkSize: MinChunkSize, SHA256: strings.Repeat("A", 64)}
	actual, count, err := ValidateCreate(valid, 1<<32)
	if err != nil || count != 2 || actual.SHA256 != strings.Repeat("a", 64) {
		t.Fatalf("normalization/last part calculation: %+v, %d, %v", actual, count, err)
	}
	tests := []struct {
		name    string
		request CreateRequest
		max     int64
	}{
		{"zero file", CreateRequest{Filename: "x", ChunkSize: MinChunkSize, SHA256: valid.SHA256}, 1 << 32},
		{"control filename", CreateRequest{Filename: "x\r\nheader", Size: 1, ChunkSize: MinChunkSize, SHA256: valid.SHA256}, 1 << 32},
		{"oversized file", valid, MinChunkSize},
		{"too many parts", CreateRequest{Filename: "x", Size: MinChunkSize*MaxParts + 1, ChunkSize: MinChunkSize, SHA256: valid.SHA256}, 1 << 40},
		{"overflow-safe count", CreateRequest{Filename: "x", Size: math.MaxInt64, ChunkSize: MaxChunkSize, SHA256: valid.SHA256}, math.MaxInt64},
		{"bad hash", CreateRequest{Filename: "x", Size: 1, ChunkSize: MinChunkSize, SHA256: strings.Repeat("z", 64)}, 1 << 32},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, _, err := ValidateCreate(test.request, test.max); !errors.Is(err, ErrInvalid) {
				t.Fatalf("want invalid, got %v", err)
			}
		})
	}
}

func TestValidIDRejectsPaths(t *testing.T) {
	for _, id := range []string{"../secret", "", strings.Repeat("a", 31), strings.Repeat("A", 32), strings.Repeat("z", 32)} {
		if ValidID(id) {
			t.Fatalf("accepted unsafe ID %q", id)
		}
	}
	if !ValidID(strings.Repeat("a", 32)) {
		t.Fatal("rejected normal internal ID")
	}
}
