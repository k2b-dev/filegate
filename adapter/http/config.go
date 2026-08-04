package httpadapter

import (
	"errors"
	"net/http"

	apiv1 "github.com/valentinkolb/filegate/api/v1"
	"github.com/valentinkolb/filegate/domain"
	"github.com/valentinkolb/filegate/infra/activity"
)

// ConfigService is the declarative configuration surface the router exposes.
type ConfigService interface {
	Schema() []apiv1.ConfigKeySchema
	Values() apiv1.ConfigValuesResponse
	PlanManifest(values map[string]any) (apiv1.ConfigManifestPlanResponse, error)
	ApplyManifest(values map[string]any, expectedRevision, actor string) (apiv1.ConfigManifestApplyResponse, error)
}

type configHandlers struct {
	svc ConfigService
}

func (h configHandlers) handleSchema(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, apiv1.ConfigSchemaResponse{Keys: h.svc.Schema()})
}

func (h configHandlers) handleValues(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, h.svc.Values())
}

func (h configHandlers) handlePlan(w http.ResponseWriter, r *http.Request) {
	var req apiv1.ConfigManifestPlanRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	if req.Values == nil {
		req.Values = map[string]any{}
	}
	plan, err := h.svc.PlanManifest(req.Values)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

func (h configHandlers) handleApply(w http.ResponseWriter, r *http.Request) {
	var req apiv1.ConfigManifestApplyRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	if req.Values == nil {
		req.Values = map[string]any{}
	}
	applied, err := h.svc.ApplyManifest(req.Values, req.ExpectedRevision, configActor(r))
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, domain.ErrConflict) {
			status = http.StatusConflict
		}
		writeErr(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, applied)
}

func configActor(r *http.Request) string {
	if label := activity.CleanActorLabel(r.Header.Get("X-Filegate-Actor")); label != "" {
		return label
	}
	return "bearer-token"
}
