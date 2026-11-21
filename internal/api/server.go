package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/interview/go-resumable-upload/internal/config"
	"github.com/interview/go-resumable-upload/internal/domain"
	"github.com/interview/go-resumable-upload/internal/service"
)

type Server struct {
	service     *service.Service
	config      config.Config
	logger      *slog.Logger
	uploadSlots chan struct{}
	mergeSlots  chan struct{}
}

func New(uploads *service.Service, config config.Config, logger *slog.Logger) *Server {
	if config.MaxConcurrentUploads <= 0 {
		config.MaxConcurrentUploads = 4
	}
	if config.MaxConcurrentMerges <= 0 {
		config.MaxConcurrentMerges = 1
	}
	return &Server{service: uploads, config: config, logger: logger,
		uploadSlots: make(chan struct{}, config.MaxConcurrentUploads), mergeSlots: make(chan struct{}, config.MaxConcurrentMerges)}
}

func (server *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", server.health)
	mux.Handle("POST /api/v1/uploads", server.auth(http.HandlerFunc(server.create)))
	mux.Handle("GET /api/v1/uploads/{id}", server.auth(http.HandlerFunc(server.status)))
	mux.Handle("PUT /api/v1/uploads/{id}/parts/{part}", server.auth(server.admit(server.uploadSlots, http.HandlerFunc(server.putPart))))
	mux.Handle("POST /api/v1/uploads/{id}/complete", server.auth(server.admit(server.mergeSlots, http.HandlerFunc(server.complete))))
	mux.Handle("DELETE /api/v1/uploads/{id}", server.auth(http.HandlerFunc(server.cancel)))
	mux.Handle("GET /api/v1/files/{id}", server.auth(http.HandlerFunc(server.download)))
	mux.Handle("HEAD /api/v1/files/{id}", server.auth(http.HandlerFunc(server.download)))
	return server.withTimeout(mux)
}

// Admission is nonblocking and happens before reading/staging bytes or starting a merge transaction.
// It bounds resources per API process, not cumulative storage quota or distributed request rate.
func (server *Server) admit(slots chan struct{}, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
			next.ServeHTTP(w, r)
		default:
			w.Header().Set("Retry-After", "1")
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "upload or merge capacity busy; retry later"})
		}
	})
}

func (server *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		provided := request.Header.Get("X-API-Key")
		if subtle.ConstantTimeCompare([]byte(provided), []byte(server.config.APIKey)) != 1 {
			writeJSON(writer, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		// Ownership comes only from server configuration, never a caller-controlled tenant header.
		next.ServeHTTP(writer, request)
	})
}

func (server *Server) withTimeout(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		ctx, cancel := context.WithTimeout(request.Context(), server.config.RequestTimeout)
		defer cancel()
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(writer, request.WithContext(ctx))
	})
}

func (server *Server) health(writer http.ResponseWriter, request *http.Request) {
	ctx, cancel := context.WithTimeout(request.Context(), 3*time.Second)
	defer cancel()
	if err := server.service.Health(ctx); err != nil {
		writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]string{"status": "ok"})
}

func (server *Server) create(writer http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(writer, request.Body, 8192)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	var input domain.CreateRequest
	if err := decoder.Decode(&input); err != nil {
		server.fail(writer, domain.ErrInvalid)
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		server.fail(writer, domain.ErrInvalid)
		return
	}
	upload, err := server.service.Create(request.Context(), server.config.TenantID, input)
	if err != nil {
		server.fail(writer, err)
		return
	}
	writer.Header().Set("Location", "/api/v1/uploads/"+upload.ID)
	writeJSON(writer, http.StatusCreated, upload)
}

func (server *Server) status(writer http.ResponseWriter, request *http.Request) {
	upload, err := server.service.Get(request.Context(), server.config.TenantID, request.PathValue("id"))
	if err != nil {
		server.fail(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, upload)
}

func (server *Server) putPart(writer http.ResponseWriter, request *http.Request) {
	number, err := strconv.Atoi(request.PathValue("part"))
	if err != nil {
		server.fail(writer, domain.ErrInvalid)
		return
	}
	deadline := time.Now().Add(server.config.UploadTimeout)
	if requestDeadline, ok := request.Context().Deadline(); ok && requestDeadline.Before(deadline) {
		deadline = requestDeadline
	}
	if err := http.NewResponseController(writer).SetReadDeadline(deadline); err != nil {
		server.logger.Warn("unable to apply upload read deadline", "error", err)
	}
	result, err := server.service.PutPart(request.Context(), server.config.TenantID,
		request.PathValue("id"), number, request.Body, request.Header.Get("X-Chunk-SHA256"))
	if err != nil {
		server.fail(writer, err)
		return
	}
	code := http.StatusCreated
	if result.AlreadyExisted {
		code = http.StatusOK
	}
	writeJSON(writer, code, result)
}

func (server *Server) complete(writer http.ResponseWriter, request *http.Request) {
	upload, err := server.service.Complete(request.Context(), server.config.TenantID, request.PathValue("id"))
	if err != nil {
		server.fail(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, upload)
}

func (server *Server) cancel(writer http.ResponseWriter, request *http.Request) {
	result, err := server.service.Cancel(request.Context(), server.config.TenantID, request.PathValue("id"))
	if err != nil {
		server.fail(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, result)
}

func (server *Server) download(writer http.ResponseWriter, request *http.Request) {
	upload, file, err := server.service.Download(request.Context(), server.config.TenantID, request.PathValue("id"))
	if err != nil {
		server.fail(writer, err)
		return
	}
	defer file.Close()
	writer.Header().Set("Content-Type", "application/octet-stream")
	writer.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": upload.Filename}))
	writer.Header().Set("ETag", `"`+upload.SHA256+`"`)
	writer.Header().Set("Cache-Control", "private, no-cache")
	modified := upload.CreatedAt
	if upload.CompletedAt != nil {
		modified = *upload.CompletedAt
	}
	http.ServeContent(writer, request, upload.Filename, modified, file)
}

func (server *Server) fail(writer http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	message := "internal error; query upload status before retrying"
	var networkError net.Error
	isNetworkTimeout := errors.As(err, &networkError) && networkError.Timeout()
	switch {
	case errors.Is(err, domain.ErrInvalid):
		status, message = http.StatusBadRequest, "invalid input, part size or checksum"
	case errors.Is(err, domain.ErrNotFound):
		status, message = http.StatusNotFound, "upload not found"
	case errors.Is(err, domain.ErrConflict):
		status, message = http.StatusConflict, "upload state, existing content or missing parts conflict"
	case errors.Is(err, domain.ErrIntegrity):
		status, message = http.StatusUnprocessableEntity, "stored or merged content failed integrity check"
	case errors.Is(err, domain.ErrStorage):
		status, message = http.StatusServiceUnavailable, "file storage unavailable"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled), isNetworkTimeout:
		status, message = http.StatusRequestTimeout, "request canceled or timed out; query status before retrying"
	}
	if status >= 500 {
		server.logger.Error("request failed", "error", err)
	}
	// SQL text, OS paths and internal errors remain outside the public response.
	writeJSON(writer, status, map[string]string{"error": message})
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}
