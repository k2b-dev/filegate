package httpadapter

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/netip"
	"strings"
	"time"

	apiv1 "github.com/valentinkolb/filegate/api/v1"
	"github.com/valentinkolb/filegate/domain"
)

const (
	defaultDirectUploadExpiry = 15 * time.Minute
	maxDirectUploadExpiry     = 24 * time.Hour
)

type directUploadManager struct {
	svc    *domain.Service
	secret []byte
	live   liveConfig
}

type directUploadToken struct {
	Version     int    `json:"v"`
	Path        string `json:"path"`
	ExpiresAt   int64  `json:"exp"`
	ContentType string `json:"contentType,omitempty"`
	OnConflict  string `json:"onConflict,omitempty"`
	MaxBytes    int64  `json:"maxBytes"`
	Nonce       string `json:"nonce"`
}

func newDirectUploadManager(svc *domain.Service, bearerToken string, live liveConfig) *directUploadManager {
	return &directUploadManager{
		svc:    svc,
		secret: []byte(strings.TrimSpace(bearerToken)),
		live:   live,
	}
}

func (m *directUploadManager) handleCreate(w http.ResponseWriter, r *http.Request) {
	if len(m.secret) == 0 {
		writeErr(w, http.StatusUnauthorized, "direct uploads require auth.bearer_token")
		return
	}

	var body apiv1.DirectUploadURLRequest
	if ok := decodeJSONBody(w, r, &body); !ok {
		return
	}

	path := strings.TrimPrefix(strings.TrimSpace(body.Path), "/")
	if path == "" {
		writeErr(w, http.StatusBadRequest, "path required")
		return
	}
	mode, err := domain.ParseConflictMode(body.OnConflict, domain.FileConflictModes)
	if err != nil {
		statusFromErr(w, err)
		return
	}

	maxUploadBytes := m.live.maxUploadBytes()
	maxBytes := body.MaxBytes
	if maxBytes <= 0 {
		maxBytes = maxUploadBytes
	}
	if maxBytes <= 0 || maxBytes > maxUploadBytes {
		writeErr(w, http.StatusBadRequest, "maxBytes exceeds upload.max_upload_bytes")
		return
	}

	expiry := defaultDirectUploadExpiry
	if body.ExpiresInSeconds > 0 {
		expiry = time.Duration(body.ExpiresInSeconds) * time.Second
	}
	if expiry <= 0 || expiry > maxDirectUploadExpiry {
		writeErr(w, http.StatusBadRequest, "expiresInSeconds must be between 1 and 86400")
		return
	}
	expiresAt := time.Now().Add(expiry).Unix()

	token, err := m.sign(directUploadToken{
		Version:     1,
		Path:        path,
		ExpiresAt:   expiresAt,
		ContentType: strings.TrimSpace(body.ContentType),
		OnConflict:  string(mode),
		MaxBytes:    maxBytes,
		Nonce:       randomNonce(),
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to create upload url")
		return
	}
	baseURL, err := directURLBaseForRequest(m.live.publicURL(), m.live.trustedProxies(), r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "public upload URL unavailable")
		return
	}
	writeJSON(w, http.StatusCreated, apiv1.DirectUploadURLResponse{
		UploadURL: baseURL + "/v1/uploads/direct/" + token,
		Method:    http.MethodPut,
		Path:      path,
		ExpiresAt: expiresAt,
		MaxBytes:  maxBytes,
	})
}

func (m *directUploadManager) handlePut(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimSpace(r.PathValue("token"))
	payload, err := m.verify(token)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "invalid upload url")
		return
	}
	if payload.ExpiresAt < time.Now().Unix() {
		writeErr(w, http.StatusUnauthorized, "upload url expired")
		return
	}
	if payload.ContentType != "" && !contentTypeMatches(r.Header.Get("Content-Type"), payload.ContentType) {
		writeErr(w, http.StatusBadRequest, "content type mismatch")
		return
	}
	if r.ContentLength > payload.MaxBytes {
		writeErr(w, http.StatusRequestEntityTooLarge, "upload exceeds maxBytes")
		return
	}

	mode, err := domain.ParseConflictMode(payload.OnConflict, domain.FileConflictModes)
	if err != nil {
		statusFromErr(w, err)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, payload.MaxBytes)
	meta, created, err := m.svc.WriteContentByVirtualPath(payload.Path, r.Body, mode)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeErr(w, http.StatusRequestEntityTooLarge, "upload exceeds maxBytes")
			return
		}
		if errors.Is(err, domain.ErrConflict) {
			existingID, existingPath := lookupExistingByPath(m.svc, payload.Path)
			writeConflict(w, "path already exists", existingID, existingPath)
			return
		}
		statusFromErr(w, err)
		return
	}
	w.Header().Set("X-Node-Id", meta.ID.String())
	if created {
		w.Header().Set("X-Created-Id", meta.ID.String())
		writeJSON(w, http.StatusCreated, nodeResponse(meta))
		return
	}
	writeJSON(w, http.StatusOK, nodeResponse(meta))
}

func (m *directUploadManager) sign(payload directUploadToken) (string, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	encodedPayload := base64.RawURLEncoding.EncodeToString(data)
	sig := hmacSHA256(m.secret, []byte(encodedPayload))
	return encodedPayload + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func (m *directUploadManager) verify(token string) (directUploadToken, error) {
	var out directUploadToken
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
	want := hmacSHA256(m.secret, []byte(payloadPart))
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
	if out.Version != 1 || strings.TrimSpace(out.Path) == "" || out.MaxBytes <= 0 {
		return out, fmt.Errorf("invalid token payload")
	}
	out.Path = strings.TrimPrefix(strings.TrimSpace(out.Path), "/")
	return out, nil
}

func hmacSHA256(secret, data []byte) []byte {
	return hmacSHA256Purpose("direct-upload", secret, data)
}

func hmacSHA256Purpose(purpose string, secret, data []byte) []byte {
	key := sha256.Sum256(append([]byte("filegate-"+purpose+"-v1:"), secret...))
	mac := hmac.New(sha256.New, key[:])
	_, _ = mac.Write(data)
	return mac.Sum(nil)
}

func randomNonce() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}

func directURLBaseForRequest(publicURL string, trusted []netip.Prefix, r *http.Request) (string, error) {
	if publicURL != "" {
		return publicURL, nil
	}
	host := r.Host
	proto := ""
	if peerTrusted(r.RemoteAddr, trusted) {
		if forwardedHost := firstHeaderValue(r.Header.Get("X-Forwarded-Host")); forwardedHost != "" {
			host = forwardedHost
		}
		proto = firstHeaderValue(r.Header.Get("X-Forwarded-Proto"))
	}
	if strings.TrimSpace(host) == "" {
		return "", fmt.Errorf("missing host")
	}
	if proto == "" {
		if r.TLS != nil {
			proto = "https"
		} else {
			proto = "http"
		}
	}
	return proto + "://" + host, nil
}

func firstHeaderValue(raw string) string {
	if raw == "" {
		return ""
	}
	first, _, _ := strings.Cut(raw, ",")
	return strings.TrimSpace(first)
}

func contentTypeMatches(got, want string) bool {
	got = strings.TrimSpace(got)
	want = strings.TrimSpace(want)
	if want == "" {
		return true
	}
	if got == "" {
		return false
	}
	gotType, _, gotErr := mime.ParseMediaType(got)
	wantType, _, wantErr := mime.ParseMediaType(want)
	if gotErr == nil && wantErr == nil {
		return strings.EqualFold(gotType, wantType)
	}
	return strings.EqualFold(got, want)
}
