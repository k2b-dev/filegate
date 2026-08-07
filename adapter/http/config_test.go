package httpadapter

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	apiv1 "github.com/k2b-dev/filegate/v3/api/v1"
	"github.com/k2b-dev/filegate/v3/domain"
)

type configServiceStub struct {
	planned  map[string]any
	applied  map[string]any
	expected string
	actor    string
	applyErr error
}

func (s *configServiceStub) Schema() []apiv1.ConfigKeySchema { return nil }
func (s *configServiceStub) Values() apiv1.ConfigValuesResponse {
	return apiv1.ConfigValuesResponse{}
}
func (s *configServiceStub) PlanManifest(values map[string]any) (apiv1.ConfigManifestPlanResponse, error) {
	s.planned = values
	return apiv1.ConfigManifestPlanResponse{CurrentRevision: "old", ProposedRevision: "new"}, nil
}
func (s *configServiceStub) ApplyManifest(values map[string]any, expectedRevision, actor string) (apiv1.ConfigManifestApplyResponse, error) {
	s.applied = values
	s.expected = expectedRevision
	s.actor = actor
	return apiv1.ConfigManifestApplyResponse{}, s.applyErr
}

func TestConfigPlanAcceptsCompleteEmptyManifest(t *testing.T) {
	stub := &configServiceStub{}
	handler := configHandlers{svc: stub}
	req := httptest.NewRequest(http.MethodPost, "/v1/config/plan", bytes.NewBufferString(`{"values":{}}`))
	out := httptest.NewRecorder()

	handler.handlePlan(out, req)

	if out.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", out.Code, out.Body.String())
	}
	if stub.planned == nil || len(stub.planned) != 0 {
		t.Errorf("planned = %#v, want empty non-nil map", stub.planned)
	}
}

func TestConfigApplyPassesRevisionAndSanitizedActor(t *testing.T) {
	stub := &configServiceStub{}
	handler := configHandlers{svc: stub}
	req := httptest.NewRequest(http.MethodPost, "/v1/config/apply", bytes.NewBufferString(
		`{"values":{"upload.expiry":"2h"},"expectedRevision":"old"}`,
	))
	req.Header.Set("X-Filegate-Actor", "  Alice\nAdmin  ")
	out := httptest.NewRecorder()

	handler.handleApply(out, req)

	if out.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", out.Code, out.Body.String())
	}
	if stub.expected != "old" || stub.actor != "Alice Admin" {
		t.Errorf("expected=%q actor=%q", stub.expected, stub.actor)
	}
}

func TestConfigApplyMapsStaleRevisionToConflict(t *testing.T) {
	stub := &configServiceStub{applyErr: fmt.Errorf("%w: stale manifest", domain.ErrConflict)}
	handler := configHandlers{svc: stub}
	req := httptest.NewRequest(http.MethodPost, "/v1/config/apply", bytes.NewBufferString(
		`{"values":{},"expectedRevision":"old"}`,
	))
	out := httptest.NewRecorder()

	handler.handleApply(out, req)

	if out.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", out.Code, out.Body.String())
	}
}

func TestConfigManifestEndpointsRejectUnknownEnvelopeFields(t *testing.T) {
	handler := configHandlers{svc: &configServiceStub{}}
	req := httptest.NewRequest(http.MethodPost, "/v1/config/plan", bytes.NewBufferString(
		`{"values":{},"changes":{}}`,
	))
	out := httptest.NewRecorder()

	handler.handlePlan(out, req)

	if out.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", out.Code)
	}
}
