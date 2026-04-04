package server

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/go-git/go-git/v6/plumbing/transport"
)

const lfsContentType = "application/vnd.git-lfs+json"

var lfsAuthPattern = regexp.MustCompile(`^git-lfs-authenticate\s+'?([^'\s]+)'?\s+(upload|download)$`)

type lfsBatchRequest struct {
	Operation string                  `json:"operation"`
	Transfers []string                `json:"transfers,omitempty"`
	Objects   []lfsBatchObjectRequest `json:"objects"`
}

type lfsBatchObjectRequest struct {
	OID  string `json:"oid"`
	Size int64  `json:"size"`
}

type lfsBatchResponse struct {
	Transfer string                   `json:"transfer,omitempty"`
	Objects  []lfsBatchObjectResponse `json:"objects"`
}

type lfsBatchObjectResponse struct {
	OID     string               `json:"oid"`
	Size    int64                `json:"size"`
	Actions map[string]lfsAction `json:"actions,omitempty"`
	Error   *lfsErrorResponse    `json:"error,omitempty"`
}

type lfsAction struct {
	Href      string            `json:"href"`
	Header    map[string]string `json:"header,omitempty"`
	ExpiresIn int               `json:"expires_in,omitempty"`
}

type lfsErrorResponse struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type lfsVerifyRequest struct {
	OID  string `json:"oid"`
	Size int64  `json:"size"`
}

func (s *Server) maybeHandleLFSHTTP(ctx context.Context, w http.ResponseWriter, r *http.Request) bool {
	repo, suffix, ok := parseLFSHTTPRequest(r.URL.Path)
	if !ok {
		return false
	}

	switch {
	case r.Method == http.MethodPost && suffix == "/objects/batch":
		s.handleLFSBatch(ctx, w, r, repo)
		return true
	case r.Method == http.MethodPost && suffix == "/verify":
		s.handleLFSVerify(ctx, w, r, repo)
		return true
	case (r.Method == http.MethodGet || r.Method == http.MethodPut) && strings.HasPrefix(suffix, "/objects/"):
		oid := strings.TrimPrefix(suffix, "/objects/")
		s.handleLFSObject(ctx, w, r, repo, oid)
		return true
	default:
		http.NotFound(w, r)
		return true
	}
}

func parseLFSHTTPRequest(path string) (repo string, suffix string, ok bool) {
	const marker = "/info/lfs"
	idx := strings.Index(path, marker)
	if idx == -1 {
		return "", "", false
	}

	repo = path[:idx]
	suffix = path[idx+len(marker):]
	if repo == "" {
		return "", "", false
	}
	return repo, suffix, true
}

func (s *Server) handleLFSBatch(ctx context.Context, w http.ResponseWriter, r *http.Request, repo string) {
	var req lfsBatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeLFSJSONError(w, http.StatusBadRequest, "invalid LFS batch request")
		return
	}

	switch req.Operation {
	case "upload", "download":
	default:
		s.writeLFSJSONError(w, http.StatusBadRequest, "unsupported LFS operation")
		return
	}

	baseURL := s.HTTPBaseURL() + "/" + strings.TrimPrefix(repo, "/") + "/info/lfs"
	authHeader := map[string]string{"Authorization": s.lfsBasicAuthHeader()}
	resp := lfsBatchResponse{
		Transfer: "basic",
		Objects:  make([]lfsBatchObjectResponse, 0, len(req.Objects)),
	}

	for _, obj := range req.Objects {
		if err := validateLFSOID(obj.OID); err != nil {
			resp.Objects = append(resp.Objects, lfsBatchObjectResponse{
				OID:  obj.OID,
				Size: obj.Size,
				Error: &lfsErrorResponse{
					Code:    http.StatusBadRequest,
					Message: err.Error(),
				},
			})
			continue
		}

		path, err := s.backend.LFSObjectPath(ctx, repo, obj.OID)
		if err != nil {
			resp.Objects = append(resp.Objects, lfsBatchObjectResponse{
				OID:  obj.OID,
				Size: obj.Size,
				Error: &lfsErrorResponse{
					Code:    lfsHTTPErrorCode(err),
					Message: err.Error(),
				},
			})
			continue
		}

		exists, err := lfsObjectExists(path)
		if err != nil {
			resp.Objects = append(resp.Objects, lfsBatchObjectResponse{
				OID:  obj.OID,
				Size: obj.Size,
				Error: &lfsErrorResponse{
					Code:    http.StatusInternalServerError,
					Message: err.Error(),
				},
			})
			continue
		}

		objectResp := lfsBatchObjectResponse{
			OID:  obj.OID,
			Size: obj.Size,
		}

		switch req.Operation {
		case "upload":
			if !exists {
				objectResp.Actions = map[string]lfsAction{
					"upload": {
						Href:   baseURL + "/objects/" + obj.OID,
						Header: authHeader,
					},
					"verify": {
						Href:   baseURL + "/verify",
						Header: authHeader,
					},
				}
			}
		case "download":
			if !exists {
				objectResp.Error = &lfsErrorResponse{
					Code:    http.StatusNotFound,
					Message: "LFS object not found",
				}
			} else {
				objectResp.Actions = map[string]lfsAction{
					"download": {
						Href:   baseURL + "/objects/" + obj.OID,
						Header: authHeader,
					},
				}
			}
		}

		resp.Objects = append(resp.Objects, objectResp)
	}

	s.writeLFSJSON(w, http.StatusOK, resp)
}

func (s *Server) handleLFSVerify(ctx context.Context, w http.ResponseWriter, r *http.Request, repo string) {
	var req lfsVerifyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeLFSJSONError(w, http.StatusBadRequest, "invalid LFS verify request")
		return
	}
	if err := validateLFSOID(req.OID); err != nil {
		s.writeLFSJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	path, err := s.backend.LFSObjectPath(ctx, repo, req.OID)
	if err != nil {
		s.writeLFSJSONError(w, lfsHTTPErrorCode(err), err.Error())
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			s.writeLFSJSONError(w, http.StatusNotFound, "LFS object not found")
			return
		}
		s.writeLFSJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if info.Size() != req.Size {
		s.writeLFSJSONError(w, http.StatusUnprocessableEntity, "LFS object size mismatch")
		return
	}

	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleLFSObject(ctx context.Context, w http.ResponseWriter, r *http.Request, repo, oid string) {
	if err := validateLFSOID(oid); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	path, err := s.backend.LFSObjectPath(ctx, repo, oid)
	if err != nil {
		http.Error(w, err.Error(), lfsHTTPErrorCode(err))
		return
	}

	switch r.Method {
	case http.MethodGet:
		file, err := os.Open(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				http.Error(w, "LFS object not found", http.StatusNotFound)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer file.Close()
		w.Header().Set("Content-Type", "application/octet-stream")
		_, err = io.Copy(w, file)
		if err != nil {
			return
		}
	case http.MethodPut:
		if err := s.backend.WriteLFSObject(ctx, repo, oid, r.Body); err != nil {
			http.Error(w, err.Error(), lfsHTTPErrorCode(err))
			return
		}
		w.WriteHeader(http.StatusOK)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) tryHandleLFSSSHAuth(command string, stdout, stderr io.Writer) (handled bool, exitCode uint32) {
	matches := lfsAuthPattern.FindStringSubmatch(strings.TrimSpace(command))
	if matches == nil {
		return false, 0
	}

	repo := matches[1]
	op := matches[2]
	if _, err := normalizeRepoPath(repo); err != nil {
		_, _ = fmt.Fprintln(stderr, err.Error())
		return true, 1
	}

	// Ensure the repo exists before issuing LFS access details.
	ctx, cancel := s.operationContext(context.Background(), nil)
	defer cancel()
	if _, err := s.backend.Open(ctx, repo); err != nil {
		_, _ = fmt.Fprintln(stderr, err.Error())
		return true, 1
	}

	resp := map[string]any{
		"href":       s.HTTPBaseURL() + "/" + strings.TrimPrefix(repo, "/") + "/info/lfs",
		"header":     map[string]string{"Authorization": s.lfsBasicAuthHeader()},
		"expires_in": int(maxOperationTime / time.Second),
		"operation":  op,
	}

	if err := json.NewEncoder(stdout).Encode(resp); err != nil {
		_, _ = fmt.Fprintln(stderr, err.Error())
		return true, 1
	}
	return true, 0
}

func (s *Server) lfsBasicAuthHeader() string {
	creds := s.cfg.HTTPUsername + ":" + s.cfg.HTTPPassword
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(creds))
}

func (s *Server) writeLFSJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", lfsContentType)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func (s *Server) writeLFSJSONError(w http.ResponseWriter, status int, message string) {
	s.writeLFSJSON(w, status, map[string]any{
		"message": message,
	})
}

func lfsHTTPErrorCode(err error) int {
	switch {
	case errors.Is(err, transport.ErrRepositoryNotFound):
		return http.StatusNotFound
	case errors.Is(err, ErrInvalidRepoPath):
		return http.StatusBadRequest
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout
	case errors.Is(err, context.Canceled):
		return http.StatusRequestTimeout
	default:
		return http.StatusInternalServerError
	}
}

func validateLFSOID(oid string) error {
	if len(oid) != 64 {
		return fmt.Errorf("invalid LFS oid")
	}
	for _, ch := range oid {
		switch {
		case ch >= '0' && ch <= '9':
		case ch >= 'a' && ch <= 'f':
		default:
			return fmt.Errorf("invalid LFS oid")
		}
	}
	return nil
}

func lfsObjectExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

func (s *Server) authorizeLFSHeader(r *http.Request) bool {
	auth := r.Header.Get("Authorization")
	expected := s.lfsBasicAuthHeader()
	if auth == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(auth), []byte(expected)) == 1
}
