package httpadapter

import (
	"errors"
	"io"
	"net/http"
	"strconv"

	apiv1 "github.com/k2b-dev/filegate/v3/api/v1"
	"github.com/k2b-dev/filegate/v3/domain"
)

// registerVersionRoutes wires the per-file version read and mutation endpoints
// onto the existing router.
func registerVersionRoutes(handleV1 func(string, http.HandlerFunc), svc *domain.Service) {
	handleV1("GET /v1/nodes/{id}/versions", func(w http.ResponseWriter, r *http.Request) {
		id, ok := parseID(w, r.PathValue("id"))
		if !ok {
			return
		}
		// Cursor + limit are optional. The cursor is the previous page's
		// trailing VersionID (empty = first page).
		cursor := domain.VersionID{}
		if raw := r.URL.Query().Get("cursor"); raw != "" {
			parsed, err := domain.ParseVersionID(raw)
			if err != nil {
				writeErr(w, http.StatusBadRequest, "invalid cursor")
				return
			}
			cursor = parsed
		}
		limit := 100
		if raw := r.URL.Query().Get("limit"); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 1 {
				writeErr(w, http.StatusBadRequest, "invalid limit")
				return
			}
			limit = n
		}

		listed, err := svc.ListVersions(id, cursor, limit)
		if err != nil {
			if errors.Is(err, domain.ErrUnsupportedFS) {
				writeErr(w, http.StatusNotFound, "versioning not supported on this mount")
				return
			}
			statusFromErr(w, err)
			return
		}

		out := apiv1.ListVersionsResponse{
			Items: make([]apiv1.VersionResponse, 0, len(listed.Items)),
		}
		for _, v := range listed.Items {
			out.Items = append(out.Items, versionResponse(v))
		}
		if !listed.NextCursor.IsZero() {
			out.NextCursor = listed.NextCursor.String()
		}
		writeJSON(w, http.StatusOK, out)
	})

	handleV1("POST /v1/nodes/{id}/versions/snapshot", func(w http.ResponseWriter, r *http.Request) {
		id, ok := parseID(w, r.PathValue("id"))
		if !ok {
			return
		}
		var body apiv1.VersionSnapshotRequest
		// Body is optional; an empty request still means "snapshot now".
		if r.ContentLength > 0 {
			if ok := decodeJSONBody(w, r, &body); !ok {
				return
			}
		}
		meta, err := svc.SnapshotVersion(id, body.Label)
		if err != nil {
			if errors.Is(err, domain.ErrUnsupportedFS) {
				writeErr(w, http.StatusNotFound, "versioning not supported on this mount")
				return
			}
			if errors.Is(err, domain.ErrConflict) {
				writeErr(w, http.StatusConflict, "max pinned versions reached for this file")
				return
			}
			statusFromErr(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, versionResponse(*meta))
	})

	handleV1("POST /v1/nodes/{id}/versions/{vid}/pin", func(w http.ResponseWriter, r *http.Request) {
		id, ok := parseID(w, r.PathValue("id"))
		if !ok {
			return
		}
		vid, err := domain.ParseVersionID(r.PathValue("vid"))
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid version id")
			return
		}
		var body apiv1.VersionPinRequest
		if r.ContentLength > 0 {
			if ok := decodeJSONBody(w, r, &body); !ok {
				return
			}
		}
		updated, err := svc.PinVersion(id, vid, body.Label)
		if err != nil {
			if errors.Is(err, domain.ErrUnsupportedFS) {
				writeErr(w, http.StatusNotFound, "versioning not supported on this mount")
				return
			}
			if errors.Is(err, domain.ErrConflict) {
				writeErr(w, http.StatusConflict, "max pinned versions reached for this file")
				return
			}
			statusFromErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, versionResponse(*updated))
	})

	handleV1("POST /v1/nodes/{id}/versions/{vid}/unpin", func(w http.ResponseWriter, r *http.Request) {
		id, ok := parseID(w, r.PathValue("id"))
		if !ok {
			return
		}
		vid, err := domain.ParseVersionID(r.PathValue("vid"))
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid version id")
			return
		}
		updated, err := svc.UnpinVersion(id, vid)
		if err != nil {
			if errors.Is(err, domain.ErrUnsupportedFS) {
				writeErr(w, http.StatusNotFound, "versioning not supported on this mount")
				return
			}
			statusFromErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, versionResponse(*updated))
	})

	handleV1("POST /v1/nodes/{id}/versions/{vid}/restore", func(w http.ResponseWriter, r *http.Request) {
		id, ok := parseID(w, r.PathValue("id"))
		if !ok {
			return
		}
		vid, err := domain.ParseVersionID(r.PathValue("vid"))
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid version id")
			return
		}
		var body apiv1.VersionRestoreRequest
		if r.ContentLength > 0 {
			if ok := decodeJSONBody(w, r, &body); !ok {
				return
			}
		}
		meta, asNew, err := svc.RestoreVersion(id, vid, domain.RestoreOptions{
			AsNewFile: body.AsNewFile,
			Name:      body.Name,
		})
		if err != nil {
			if errors.Is(err, domain.ErrUnsupportedFS) {
				writeErr(w, http.StatusNotFound, "versioning not supported on this mount")
				return
			}
			if errors.Is(err, domain.ErrConflict) {
				writeErr(w, http.StatusConflict, "could not place restored file (suffix space exhausted)")
				return
			}
			statusFromErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, apiv1.VersionRestoreResponse{
			Node:  nodeResponse(meta),
			AsNew: asNew,
		})
	})

	handleV1("DELETE /v1/nodes/{id}/versions/{vid}", func(w http.ResponseWriter, r *http.Request) {
		id, ok := parseID(w, r.PathValue("id"))
		if !ok {
			return
		}
		vid, err := domain.ParseVersionID(r.PathValue("vid"))
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid version id")
			return
		}
		if err := svc.DeleteVersion(id, vid); err != nil {
			if errors.Is(err, domain.ErrUnsupportedFS) {
				writeErr(w, http.StatusNotFound, "versioning not supported on this mount")
				return
			}
			statusFromErr(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	handleV1("GET /v1/nodes/{id}/versions/{vid}/content", func(w http.ResponseWriter, r *http.Request) {
		id, ok := parseID(w, r.PathValue("id"))
		if !ok {
			return
		}
		vid, err := domain.ParseVersionID(r.PathValue("vid"))
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid version id")
			return
		}

		f, meta, err := svc.OpenVersionContent(id, vid)
		if err != nil {
			if errors.Is(err, domain.ErrUnsupportedFS) {
				writeErr(w, http.StatusNotFound, "versioning not supported on this mount")
				return
			}
			statusFromErr(w, err)
			return
		}
		defer f.Close()

		// http.ServeContent gives us byte-range support and conditional
		// GET handling for free against an *os.File. Last-Modified is
		// derived from the version's recorded Timestamp so a client can
		// cache the immutable payload.
		w.Header().Set("Content-Type", "application/octet-stream")
		// Suggest a download name that's recognisable in browsers but
		// doesn't pretend to be the original filename (we don't store
		// it on the version — the source file's name might have changed
		// since this version was captured).
		setContentDisposition(w, "attachment", meta.VersionID.String()+".bin")
		w.Header().Set("Content-Length", strconv.FormatInt(meta.Size, 10))
		bufPtr := copyBufPool.Get().(*[]byte)
		buf := *bufPtr
		_, _ = io.CopyBuffer(w, f, buf)
		copyBufPool.Put(bufPtr)
	})
}

// versionResponse maps the domain VersionMeta to its HTTP representation.
// Kept in the route file so the JSON shape and the route handler evolve
// together; types live in api/v1 to stay importable by the SDK.
func versionResponse(v domain.VersionMeta) apiv1.VersionResponse {
	return apiv1.VersionResponse{
		VersionID: v.VersionID.String(),
		FileID:    v.FileID.String(),
		Timestamp: v.Timestamp,
		Size:      v.Size,
		Mode:      v.Mode,
		Pinned:    v.Pinned,
		Label:     v.Label,
		DeletedAt: v.DeletedAt,
	}
}
