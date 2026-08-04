package httpadapter

import (
	"encoding/json"
	"net/http"

	apiv1 "github.com/valentinkolb/filegate/api/v1"
)

// S3KeyService is the access-key surface the router exposes. Implemented in
// the CLI package, which owns the runtime store.
type S3KeyService interface {
	List() ([]apiv1.S3Key, error)
	Create(req apiv1.S3KeyCreateRequest) (apiv1.S3KeyCreated, error)
	Rotate(accessKey string) (apiv1.S3KeyCreated, error)
	Update(accessKey string, req apiv1.S3KeyUpdateRequest) (apiv1.S3Key, error)
	Delete(accessKey string) error
}

type s3KeyHandlers struct {
	svc S3KeyService
}

func (h s3KeyHandlers) handleList(w http.ResponseWriter, _ *http.Request) {
	keys, err := h.svc.List()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, apiv1.S3KeyListResponse{Items: keys, Total: len(keys)})
}

// handleCreate returns the secret, which is the only time it is ever readable.
func (h s3KeyHandlers) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req apiv1.S3KeyCreateRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	created, err := h.svc.Create(req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (h s3KeyHandlers) handleRotate(w http.ResponseWriter, r *http.Request) {
	rotated, err := h.svc.Rotate(r.PathValue("accessKey"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rotated)
}

func (h s3KeyHandlers) handleUpdate(w http.ResponseWriter, r *http.Request) {
	var req apiv1.S3KeyUpdateRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	updated, err := h.svc.Update(r.PathValue("accessKey"), req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (h s3KeyHandlers) handleDelete(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.Delete(r.PathValue("accessKey")); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func decodeStrict(w http.ResponseWriter, r *http.Request, out any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return false
	}
	return true
}
