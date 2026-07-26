package httpadapter

import (
	"encoding/json"
	"net/http"

	apiv1 "github.com/valentinkolb/filegate/api/v1"
)

// ConfigService is the configuration surface the router exposes.
//
// It is an interface because the specs, defaults and validation live in the
// CLI package, which already imports this one. Passing the implementation in
// keeps the dependency pointing one way instead of reaching back up.
type ConfigService interface {
	Schema() []apiv1.ConfigKeySchema
	Values() apiv1.ConfigValuesResponse
	ApplyChanges(changes map[string]any) ([]apiv1.ConfigRestartRequired, error)
	ValidateChanges(changes map[string]any) error
	ReloadConfig() ([]apiv1.ConfigRestartRequired, error)
}

type configHandlers struct {
	svc ConfigService
}

// handleSchema serves GET /v1/config/schema.
//
// Clients render their controls from this rather than hardcoding the key list,
// so a key added in a later release needs no client change.
func (h configHandlers) handleSchema(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, apiv1.ConfigSchemaResponse{Keys: h.svc.Schema()})
}

// handleValues serves GET /v1/config: effective values plus where each came
// from, and which static settings are waiting on a restart.
func (h configHandlers) handleValues(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, h.svc.Values())
}

// handlePatch serves PATCH /v1/config.
//
// Three outcomes, not two: applied and live, applied but needing a restart, or
// rejected. The middle one is what a config API most easily gets wrong, by
// answering 200 and changing nothing.
func (h configHandlers) handlePatch(w http.ResponseWriter, r *http.Request) {
	changes, ok := decodeChanges(w, r)
	if !ok {
		return
	}

	restarts, err := h.svc.ApplyChanges(changes)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, apiv1.ConfigChangeResponse{Applied: true, RestartRequired: restarts})
}

// handleValidate serves POST /v1/config/validate: the same resolution and
// validation as a patch, without persisting anything, so a UI can check input
// as it is typed.
func (h configHandlers) handleValidate(w http.ResponseWriter, r *http.Request) {
	changes, ok := decodeChanges(w, r)
	if !ok {
		return
	}

	if err := h.svc.ValidateChanges(changes); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, apiv1.ConfigChangeResponse{Applied: false})
}

// handleReload serves POST /v1/config/reload, re-reading every source so an
// operator who edited the file by hand can apply it without a restart.
func (h configHandlers) handleReload(w http.ResponseWriter, _ *http.Request) {
	restarts, err := h.svc.ReloadConfig()
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, apiv1.ConfigChangeResponse{Applied: true, RestartRequired: restarts})
}

func decodeChanges(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	var req apiv1.ConfigChangeRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	// Unknown fields are rejected so a typo in the envelope fails loudly
	// instead of being read as an empty change set.
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return nil, false
	}
	if len(req.Changes) == 0 {
		writeErr(w, http.StatusBadRequest, "changes must contain at least one key")
		return nil, false
	}
	return req.Changes, true
}
