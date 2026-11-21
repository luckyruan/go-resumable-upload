package api

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/interview/go-resumable-upload/internal/config"
)

func TestAuthenticationRunsBeforeUploadWork(t *testing.T) {
	server := New(nil, config.Config{APIKey: "test-key-123456", RequestTimeout: time.Second}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	request := httptest.NewRequest(http.MethodPut, "/api/v1/uploads/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/parts/1", strings.NewReader("data"))
	request.Header.Set("X-Tenant-ID", "other-tenant")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("got %d", response.Code)
	}
	// service is nil: reaching any business logic would panic; missing Key must stop beforehand.
}

func TestBusyAdmissionDoesNotEnterServiceAndReleasesCapacity(t *testing.T) {
	server := New(nil, config.Config{APIKey: "test-key-123456", RequestTimeout: time.Second, MaxConcurrentUploads: 1}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server.uploadSlots <- struct{}{}
	r := httptest.NewRequest(http.MethodPut, "/api/v1/uploads/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/parts/1", strings.NewReader("body"))
	r.Header.Set("X-API-Key", "test-key-123456")
	w := httptest.NewRecorder()
	server.Handler().ServeHTTP(w, r)
	if w.Code != 429 || w.Header().Get("Retry-After") == "" {
		t.Fatalf("status=%d", w.Code)
	}
	<-server.uploadSlots
	called := 0
	handler := server.admit(server.uploadSlots, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called++; w.WriteHeader(204) }))
	for i := 0; i < 2; i++ {
		handler.ServeHTTP(httptest.NewRecorder(), r)
	}
	if called != 2 || len(server.uploadSlots) != 0 {
		t.Fatal("capacity leaked")
	}
}

func TestDownloadRangeHeadAndConditionalResponse(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "download-*")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteString("abcdef"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		method, rangeValue, ifRange, ifNone string
		status                              int
		body                                string
	}{
		{"GET", "bytes=1-3", "", "", 206, "bcd"},
		{"GET", "bytes=1-3", `"old"`, "", 200, "abcdef"},
		{"HEAD", "", "", "", 200, ""},
		{"GET", "bytes=999-", "", "", 416, ""},
		{"GET", "", "", `"current"`, 304, ""},
	} {
		r := httptest.NewRequest(tc.method, "/file", nil)
		r.Header.Set("Range", tc.rangeValue)
		r.Header.Set("If-Range", tc.ifRange)
		r.Header.Set("If-None-Match", tc.ifNone)
		w := httptest.NewRecorder()
		w.Header().Set("ETag", `"current"`)
		http.ServeContent(w, r, "demo", time.Unix(1234, 0), file)
		if w.Code != tc.status {
			t.Fatalf("case=%+v got=%d", tc, w.Code)
		}
		if tc.status != 416 && w.Body.String() != tc.body {
			t.Fatalf("case=%+v body=%q", tc, w.Body.String())
		}
	}
}

func TestInternalErrorDoesNotLeakSQLOrFilesystemPath(t *testing.T) {
	server := New(nil, config.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	response := httptest.NewRecorder()
	server.fail(response, errors.New("SELECT private_column FROM uploads; /secret/private-file"))
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("got %d", response.Code)
	}
	if strings.Contains(response.Body.String(), "private") || strings.Contains(response.Body.String(), "SELECT") {
		t.Fatal("internal details leaked")
	}
}
