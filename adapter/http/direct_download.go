package httpadapter

import (
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	apiv1 "github.com/k2b-dev/filegate/v3/api/v1"
	"github.com/k2b-dev/filegate/v3/domain"
)

const (
	defaultDirectDownloadExpiry = 15 * time.Minute
	maxDirectDownloadExpiry     = 24 * time.Hour
)

type directDownloadManager struct {
	svc    *domain.Service
	secret []byte
	live   liveConfig
}

type directDownloadToken struct {
	Version   int    `json:"v"`
	NodeID    string `json:"nodeId"`
	ExpiresAt int64  `json:"exp"`
	Inline    bool   `json:"inline,omitempty"`
	ETag      string `json:"etag,omitempty"`
	SHA256    string `json:"sha256,omitempty"`
	Size      int64  `json:"size,omitempty"`
	Mtime     int64  `json:"mtime,omitempty"`
	Nonce     string `json:"nonce"`
}

func newDirectDownloadManager(svc *domain.Service, bearerToken string, live liveConfig) *directDownloadManager {
	return &directDownloadManager{
		svc:    svc,
		secret: []byte(strings.TrimSpace(bearerToken)),
		live:   live,
	}
}

func (m *directDownloadManager) handleCreate(w http.ResponseWriter, r *http.Request) {
	if len(m.secret) == 0 {
		writeErr(w, http.StatusUnauthorized, "direct downloads require auth.bearer_token")
		return
	}

	var body apiv1.DirectDownloadURLRequest
	if ok := decodeJSONBody(w, r, &body); !ok {
		return
	}

	meta, err := m.resolve(body)
	if err != nil {
		statusFromErr(w, err)
		return
	}

	expiry := defaultDirectDownloadExpiry
	if body.ExpiresInSeconds > 0 {
		expiry = time.Duration(body.ExpiresInSeconds) * time.Second
	}
	if expiry <= 0 || expiry > maxDirectDownloadExpiry {
		writeErr(w, http.StatusBadRequest, "expiresInSeconds must be between 1 and 86400")
		return
	}
	expiresAt := time.Now().Add(expiry).Unix()
	tokenPayload := directDownloadToken{
		Version:   1,
		NodeID:    meta.ID.String(),
		ExpiresAt: expiresAt,
		Inline:    body.Inline,
		Nonce:     randomNonce(),
	}
	if meta.Type == "file" {
		tokenPayload.ETag = meta.ETag
		tokenPayload.SHA256 = meta.SHA256
		tokenPayload.Size = meta.Size
		tokenPayload.Mtime = meta.Mtime
	}

	token, err := m.sign(tokenPayload)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to create download url")
		return
	}
	baseURL, err := directURLBaseForRequest(m.live.publicURL(), m.live.trustedProxies(), r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "public download URL unavailable")
		return
	}
	writeJSON(w, http.StatusCreated, apiv1.DirectDownloadURLResponse{
		DownloadURL: baseURL + "/v1/downloads/direct/" + token,
		Method:      http.MethodGet,
		ExpiresAt:   expiresAt,
		Node:        nodeResponse(meta),
	})
}

func (m *directDownloadManager) handleGet(w http.ResponseWriter, r *http.Request) {
	payload, err := m.verify(strings.TrimSpace(r.PathValue("token")))
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "invalid download url")
		return
	}
	if payload.ExpiresAt < time.Now().Unix() {
		writeErr(w, http.StatusUnauthorized, "download url expired")
		return
	}

	id, err := domain.ParseFileID(payload.NodeID)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "invalid download url")
		return
	}
	meta, err := m.svc.GetFile(id)
	if err != nil {
		statusFromErr(w, err)
		return
	}
	if meta.Type == "file" && (meta.ETag != payload.ETag || meta.SHA256 != payload.SHA256 || meta.Size != payload.Size || meta.Mtime != payload.Mtime) {
		writeErr(w, http.StatusConflict, "download url no longer matches file")
		return
	}

	streamNodeContent(w, r, m.svc, id, payload.Inline)
}

func (m *directDownloadManager) resolve(body apiv1.DirectDownloadURLRequest) (*domain.FileMeta, error) {
	nodeID := strings.TrimSpace(body.NodeID)
	path := strings.TrimSpace(body.Path)
	if nodeID == "" && path == "" {
		return nil, domain.ErrInvalidArgument
	}
	if nodeID != "" && path != "" {
		return nil, domain.ErrInvalidArgument
	}
	if nodeID != "" {
		id, err := domain.ParseFileID(nodeID)
		if err != nil {
			return nil, domain.ErrInvalidArgument
		}
		return m.svc.GetFile(id)
	}
	return m.svc.GetFileByVirtualPath(path)
}

func (m *directDownloadManager) sign(payload directDownloadToken) (string, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	encodedPayload := base64.RawURLEncoding.EncodeToString(data)
	sig := hmacSHA256Purpose("direct-download", m.secret, []byte(encodedPayload))
	return encodedPayload + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func (m *directDownloadManager) verify(token string) (directDownloadToken, error) {
	var out directDownloadToken
	if len(m.secret) == 0 {
		return out, fmt.Errorf("missing signing secret")
	}
	payloadPart, sigPart, ok := strings.Cut(token, ".")
	if !ok || payloadPart == "" || sigPart == "" {
		return out, fmt.Errorf("malformed token")
	}
	sig, err := base64.RawURLEncoding.DecodeString(sigPart)
	if err != nil {
		return out, err
	}
	want := hmacSHA256Purpose("direct-download", m.secret, []byte(payloadPart))
	if subtle.ConstantTimeCompare(sig, want) != 1 {
		return out, fmt.Errorf("bad signature")
	}
	data, err := base64.RawURLEncoding.DecodeString(payloadPart)
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return out, err
	}
	if out.Version != 1 || strings.TrimSpace(out.NodeID) == "" {
		return out, fmt.Errorf("invalid token payload")
	}
	out.NodeID = strings.TrimSpace(out.NodeID)
	return out, nil
}
