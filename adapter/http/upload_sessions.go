package httpadapter

import (
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/puzpuzpuz/xsync/v4"

	apiv1 "github.com/k2b-dev/filegate/v3/api/v1"
	"github.com/k2b-dev/filegate/v3/domain"
	"github.com/k2b-dev/filegate/v3/infra/filesystem"
)

const (
	uploadSessionIDPrefix       = "upl_"
	uploadSessionTokenTTL       = 15 * time.Minute
	maxUploadSessionTokenTTL    = 24 * time.Hour
	maxUploadSessionSegments    = 10000
	maxUploadSessionBatch       = 1000
	uploadSessionSegmentsDir    = ".fg-uploads"
	uploadSessionSegmentSubdir  = "segments"
	uploadSessionCompleteSubdir = "complete"
)

var (
	uploadSessionIDRE = regexpMust(`^upl_[a-f0-9]{32}$`)
	checksumRE        = regexpMust(`^sha256:[a-f0-9]{64}$`)
)

type uploadSessionManager struct {
	svc *domain.Service

	secret []byte
	live   liveConfig

	maxWrites       int
	expiry          time.Duration
	cleanupInterval time.Duration

	locks        *xsync.Map[string, *sync.Mutex]
	segmentLocks *xsync.Map[string, *sync.Mutex]
	writeSlots   chan struct{}

	cleanupStop chan struct{}
	cleanupDone chan struct{}
	cleanupOnce sync.Once
}

type uploadSessionToken struct {
	Version   int      `json:"v"`
	SessionID string   `json:"sid"`
	ExpiresAt int64    `json:"exp"`
	Allow     []string `json:"allow"`
	Nonce     string   `json:"nonce"`
}

func newUploadSessionManager(
	svc *domain.Service,
	bearerToken string,
	live liveConfig,
	maxConcurrentWrites int,
	expiry, cleanupInterval time.Duration,
) *uploadSessionManager {
	if maxConcurrentWrites <= 0 {
		maxConcurrentWrites = runtime.NumCPU() * 8
		if maxConcurrentWrites < 32 {
			maxConcurrentWrites = 32
		}
		if maxConcurrentWrites > 512 {
			maxConcurrentWrites = 512
		}
	}
	m := &uploadSessionManager{
		svc:             svc,
		secret:          []byte(strings.TrimSpace(bearerToken)),
		live:            live,
		maxWrites:       maxConcurrentWrites,
		expiry:          expiry,
		cleanupInterval: cleanupInterval,
		locks:           xsync.NewMap[string, *sync.Mutex](),
		segmentLocks:    xsync.NewMap[string, *sync.Mutex](),
		writeSlots:      make(chan struct{}, maxConcurrentWrites),
		cleanupStop:     make(chan struct{}),
		cleanupDone:     make(chan struct{}),
	}
	if cleanupInterval > 0 {
		go m.cleanupLoop()
	} else {
		close(m.cleanupDone)
	}
	return m
}

func regexpMust(pattern string) *regexp.Regexp {
	return regexp.MustCompile(pattern)
}

func (m *uploadSessionManager) lock(sessionID string) *sync.Mutex {
	if l, ok := m.locks.Load(sessionID); ok {
		return l
	}
	l := &sync.Mutex{}
	actual, _ := m.locks.LoadOrStore(sessionID, l)
	return actual
}

func (m *uploadSessionManager) segmentLock(sessionID string, index int) *sync.Mutex {
	key := sessionID + ":" + strconv.Itoa(index)
	if l, ok := m.segmentLocks.Load(key); ok {
		return l
	}
	l := &sync.Mutex{}
	actual, _ := m.segmentLocks.LoadOrStore(key, l)
	return actual
}

func (m *uploadSessionManager) acquireWriteSlot(r *http.Request) error {
	select {
	case m.writeSlots <- struct{}{}:
		return nil
	case <-r.Context().Done():
		return r.Context().Err()
	}
}

func (m *uploadSessionManager) releaseWriteSlot() {
	select {
	case <-m.writeSlots:
	default:
	}
}

func (m *uploadSessionManager) cleanupLoop() {
	defer close(m.cleanupDone)
	ticker := time.NewTicker(m.cleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if err := m.cleanupExpired(); err != nil {
				log.Printf("[filegate] upload session cleanup: %v", err)
			}
		case <-m.cleanupStop:
			return
		}
	}
}

func (m *uploadSessionManager) Close() {
	if m == nil {
		return
	}
	m.cleanupOnce.Do(func() {
		close(m.cleanupStop)
		<-m.cleanupDone
	})
}

func (m *uploadSessionManager) cleanupExpired() error {
	if m.expiry <= 0 {
		return nil
	}
	now := time.Now()
	for _, phase := range []domain.UploadSessionPhase{
		domain.UploadSessionInProgress,
		domain.UploadSessionCommitting,
		domain.UploadSessionAborted,
		domain.UploadSessionCommitted,
	} {
		sessions, err := m.svc.ListUploadSessions(phase)
		if err != nil {
			return err
		}
		for _, session := range sessions {
			ts := session.UpdatedAt
			if session.CompletedAt > 0 {
				ts = session.CompletedAt
			}
			if ts == 0 {
				ts = session.CreatedAt
			}
			if ts == 0 || now.Sub(time.UnixMilli(ts)) <= m.expiry {
				continue
			}
			lock := m.lock(session.ID)
			lock.Lock()
			if err := m.removeSessionArtifacts(session); err != nil {
				log.Printf("[filegate] warning: failed to remove upload session artifacts for %s: %v", session.ID, err)
			}
			if err := m.svc.DeleteUploadSession(session.ID); err != nil {
				lock.Unlock()
				return err
			}
			if err := m.svc.DeleteUploadCommitRecord(session.ID); err != nil && !errors.Is(err, domain.ErrNotFound) {
				lock.Unlock()
				return err
			}
			lock.Unlock()
			m.locks.Delete(session.ID)
		}
	}
	return nil
}

// removeSessionArtifacts deletes a session's staged segments and its assembled
// file.
//
// The paths are derived rather than looked up or globbed. A segment file only
// ever lives at segmentPath(session, i) for an index the PUT handler validated
// into [0, TotalSegments), and the partial writes it makes carry a
// .upload-segment-* name that never matched the old glob anyway. Deriving them
// drops an index read and, more importantly, a directory scan: the stage
// directory is shared by every session on the mount, so globbing it once per
// commit turned a five-thousand-file upload into five thousand scans of a
// five-thousand-entry directory.
//
// The directories are deliberately not fsynced. The only thing that would
// guarantee is that the deletion of temporary staging files survives a crash,
// and a resurrected staging file is harmless: a committed session answers from
// its commit record without consulting segments, an aborted one is closed to
// writes, and the cleanup loop sweeps whatever is left. Two directory fsyncs per
// commit is a real cost paid for keeping garbage deleted.
func (m *uploadSessionManager) removeSessionArtifacts(session domain.UploadSession) error {
	if session.StageDir == "" || session.ID == "" {
		return nil
	}
	for i := 0; i < session.TotalSegments; i++ {
		_ = os.Remove(segmentPath(session, i))
	}
	_ = os.Remove(filepath.Join(filepath.Dir(session.StageDir), uploadSessionCompleteSubdir, session.ID+".complete"))
	return nil
}

func generateUploadSessionID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return uploadSessionIDPrefix + hex.EncodeToString(raw[:]), nil
}

func hashWholeFile(path string) (domain.ContentHashes, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return domain.ContentHashes{}, 0, err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return domain.ContentHashes{}, 0, err
	}
	md5Hash := md5.New()
	shaHash := sha256.New()
	bufPtr := copyBufPool.Get().(*[]byte)
	buf := *bufPtr
	_, copyErr := io.CopyBuffer(io.MultiWriter(md5Hash, shaHash), f, buf)
	copyBufPool.Put(bufPtr)
	if copyErr != nil {
		return domain.ContentHashes{}, 0, copyErr
	}
	return domain.ContentHashes{
		MD5Hex: hex.EncodeToString(md5Hash.Sum(nil)),
		SHA256: "sha256:" + hex.EncodeToString(shaHash.Sum(nil)),
	}, st.Size(), nil
}

func uploadAllowed(allow []string, op string) bool {
	for _, v := range allow {
		if v == op {
			return true
		}
	}
	return false
}

func defaultUploadSessionAllow() []string {
	return []string{"putSegment", "status", "commit", "abort"}
}

func validateUploadSessionAllow(allow []string) error {
	if len(allow) == 0 {
		return nil
	}
	valid := defaultUploadSessionAllow()
	for _, op := range allow {
		if !uploadAllowed(valid, op) {
			return domain.ErrInvalidArgument
		}
	}
	return nil
}

func (m *uploadSessionManager) isBearer(r *http.Request) bool {
	if len(m.secret) == 0 {
		return false
	}
	got := strings.TrimSpace(r.Header.Get("Authorization"))
	if !strings.HasPrefix(got, "Bearer ") {
		return false
	}
	got = strings.TrimSpace(strings.TrimPrefix(got, "Bearer "))
	return subtle.ConstantTimeCompare([]byte(got), m.secret) == 1
}

func (m *uploadSessionManager) authorizeSession(r *http.Request, sessionID, op string) bool {
	if m.isBearer(r) {
		return true
	}
	token := strings.TrimSpace(r.Header.Get("Filegate-Upload-Session"))
	if token == "" {
		token = strings.TrimSpace(r.URL.Query().Get("upload_token"))
	}
	payload, err := m.verifyToken(token)
	if err != nil {
		return false
	}
	if payload.ExpiresAt < time.Now().Unix() || payload.SessionID != sessionID {
		return false
	}
	return uploadAllowed(payload.Allow, op)
}

func (m *uploadSessionManager) signToken(payload uploadSessionToken) (string, error) {
	if len(m.secret) == 0 {
		return "", fmt.Errorf("missing signing secret")
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(data)
	sig := hmacSHA256(m.secret, []byte(encoded))
	return encoded + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func (m *uploadSessionManager) verifyToken(token string) (uploadSessionToken, error) {
	var out uploadSessionToken
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
	if out.Version != 1 || !uploadSessionIDRE.MatchString(out.SessionID) || len(out.Allow) == 0 {
		return out, fmt.Errorf("invalid token payload")
	}
	return out, nil
}

func (m *uploadSessionManager) directForRequest(r *http.Request, sessionID string, req *apiv1.UploadSessionDirectRequest) (*apiv1.UploadSessionDirect, error) {
	if req == nil {
		return nil, nil
	}
	expiry := uploadSessionTokenTTL
	if req.ExpiresInSeconds > 0 {
		expiry = time.Duration(req.ExpiresInSeconds) * time.Second
	}
	if expiry <= 0 || expiry > maxUploadSessionTokenTTL {
		return nil, fmt.Errorf("expiresInSeconds must be between 1 and 86400")
	}
	allow := append([]string(nil), req.Allow...)
	if len(allow) == 0 {
		allow = defaultUploadSessionAllow()
	}
	if err := validateUploadSessionAllow(allow); err != nil {
		return nil, err
	}
	expiresAt := time.Now().Add(expiry).Unix()
	token, err := m.signToken(uploadSessionToken{
		Version:   1,
		SessionID: sessionID,
		ExpiresAt: expiresAt,
		Allow:     allow,
		Nonce:     randomNonce(),
	})
	if err != nil {
		return nil, err
	}
	baseURL, err := m.baseURLForRequest(r)
	if err != nil {
		return nil, err
	}
	return &apiv1.UploadSessionDirect{
		BaseURL:   baseURL + "/v1/uploads/sessions/" + sessionID,
		Token:     token,
		ExpiresAt: expiresAt,
		Allow:     allow,
	}, nil
}

func (m *uploadSessionManager) baseURLForRequest(r *http.Request) (string, error) {
	if publicURL := m.live.publicURL(); publicURL != "" {
		return publicURL, nil
	}
	host := r.Host
	proto := ""
	if m.peerTrusted(r.RemoteAddr) {
		if forwardedHost := firstHeaderValue(r.Header.Get("X-Forwarded-Host")); forwardedHost != "" {
			host = forwardedHost
		}
		proto = firstHeaderValue(r.Header.Get("X-Forwarded-Proto"))
	}
	if host == "" {
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

func (m *uploadSessionManager) peerTrusted(remoteAddr string) bool {
	return peerTrusted(remoteAddr, m.live.trustedProxies())
}

func cleanSessionUploadPath(raw string) (string, error) {
	p := strings.Trim(strings.TrimSpace(raw), "/")
	if p == "" || strings.Contains(p, "\x00") {
		return "", domain.ErrInvalidArgument
	}
	parts := strings.Split(p, "/")
	clean := make([]string, 0, len(parts))
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return "", domain.ErrInvalidArgument
		}
		if part == uploadSessionSegmentsDir {
			return "", domain.ErrForbidden
		}
		clean = append(clean, part)
	}
	if len(clean) < 2 {
		return "", domain.ErrInvalidArgument
	}
	return strings.Join(clean, "/"), nil
}

func (m *uploadSessionManager) mountRootByName(name string) (domain.MountEntry, string, error) {
	for _, root := range m.svc.ListRoot() {
		if root.Name != name {
			continue
		}
		abs, err := m.svc.ResolveAbsPath(root.ID)
		if err != nil {
			return domain.MountEntry{}, "", err
		}
		return root, abs, nil
	}
	return domain.MountEntry{}, "", domain.ErrNotFound
}

func segmentPlan(size, segmentSize int64) ([]apiv1.UploadSessionSegment, int) {
	if size <= 0 || segmentSize <= 0 {
		return nil, 0
	}
	total := int((size + segmentSize - 1) / segmentSize)
	out := make([]apiv1.UploadSessionSegment, 0, total)
	for i := 0; i < total; i++ {
		offset := int64(i) * segmentSize
		partSize := segmentSize
		if remaining := size - offset; remaining < partSize {
			partSize = remaining
		}
		out = append(out, apiv1.UploadSessionSegment{Index: i, Offset: offset, Size: partSize})
	}
	return out, total
}

func expectedSegmentSize(session domain.UploadSession, index int) (int64, int64, error) {
	if index < 0 || index >= session.TotalSegments {
		return 0, 0, domain.ErrInvalidArgument
	}
	offset := int64(index) * session.SegmentSize
	size := session.SegmentSize
	if remaining := session.Size - offset; remaining < size {
		size = remaining
	}
	if size <= 0 {
		return 0, 0, domain.ErrInvalidArgument
	}
	return offset, size, nil
}

func uploadedSegmentIndexes(segments []domain.UploadSegment) []int {
	out := make([]int, 0, len(segments))
	for _, segment := range segments {
		out = append(out, segment.Index)
	}
	sort.Ints(out)
	return out
}

func (m *uploadSessionManager) responseFor(r *http.Request, session domain.UploadSession, directReq *apiv1.UploadSessionDirectRequest) (apiv1.UploadSessionResponse, error) {
	segments, err := m.svc.ListUploadSegments(session.ID)
	if err != nil {
		return apiv1.UploadSessionResponse{}, err
	}
	plan, _ := segmentPlan(session.Size, session.SegmentSize)
	var direct *apiv1.UploadSessionDirect
	if directReq != nil {
		direct, err = m.directForRequest(r, session.ID, directReq)
		if err != nil {
			return apiv1.UploadSessionResponse{}, err
		}
	}
	return apiv1.UploadSessionResponse{
		ID:               session.ID,
		Path:             session.Path,
		Size:             session.Size,
		Checksum:         session.Checksum,
		SegmentSize:      session.SegmentSize,
		TotalSegments:    session.TotalSegments,
		Segments:         plan,
		UploadedSegments: uploadedSegmentIndexes(segments),
		Phase:            string(session.Phase),
		Direct:           direct,
	}, nil
}

func (m *uploadSessionManager) createSession(r *http.Request, body apiv1.UploadSessionCreateRequest, fallbackSegmentSize int64, fallbackDirect *apiv1.UploadSessionDirectRequest) (apiv1.UploadSessionResponse, error) {
	path, err := cleanSessionUploadPath(body.Path)
	if err != nil {
		return apiv1.UploadSessionResponse{}, err
	}
	maxUploadBytes := m.live.maxSessionUploadBytes()
	if body.Size <= 0 || body.Size > maxUploadBytes {
		return apiv1.UploadSessionResponse{}, domain.ErrInvalidArgument
	}
	if !checksumRE.MatchString(strings.TrimSpace(body.Checksum)) {
		return apiv1.UploadSessionResponse{}, domain.ErrInvalidArgument
	}
	segmentSize := body.SegmentSize
	if segmentSize <= 0 {
		segmentSize = fallbackSegmentSize
	}
	maxSegmentBytes := m.live.maxChunkBytes()
	if segmentSize <= 0 {
		segmentSize = maxSegmentBytes
	}
	if segmentSize <= 0 || segmentSize > maxSegmentBytes {
		return apiv1.UploadSessionResponse{}, domain.ErrInvalidArgument
	}
	mode, err := domain.ParseConflictMode(body.OnConflict, domain.FileConflictModes)
	if err != nil {
		return apiv1.UploadSessionResponse{}, err
	}
	if mode == domain.ConflictRename {
		return apiv1.UploadSessionResponse{}, domain.ErrInvalidArgument
	}

	parts := strings.Split(path, "/")
	root, mountAbs, err := m.mountRootByName(parts[0])
	if err != nil {
		return apiv1.UploadSessionResponse{}, err
	}
	if mode == domain.ConflictError {
		if _, err := m.svc.ResolvePath(path); err == nil {
			return apiv1.UploadSessionResponse{}, domain.ErrConflict
		} else if !errors.Is(err, domain.ErrNotFound) {
			return apiv1.UploadSessionResponse{}, err
		}
	}

	stageDir := filepath.Join(mountAbs, uploadSessionSegmentsDir, uploadSessionSegmentSubdir)
	if err := os.MkdirAll(stageDir, 0o700); err != nil {
		return apiv1.UploadSessionResponse{}, err
	}
	if err := m.ensureSpace(stageDir, body.Size); err != nil {
		return apiv1.UploadSessionResponse{}, err
	}

	sessionID, err := generateUploadSessionID()
	if err != nil {
		return apiv1.UploadSessionResponse{}, err
	}
	_, totalSegments := segmentPlan(body.Size, segmentSize)
	if totalSegments <= 0 || totalSegments > maxUploadSessionSegments {
		return apiv1.UploadSessionResponse{}, domain.ErrInvalidArgument
	}
	now := time.Now().UnixMilli()
	session := domain.UploadSession{
		ID:            sessionID,
		Path:          path,
		ParentID:      root.ID,
		Filename:      parts[len(parts)-1],
		Size:          body.Size,
		Checksum:      strings.TrimSpace(body.Checksum),
		SegmentSize:   segmentSize,
		TotalSegments: totalSegments,
		ContentType:   strings.TrimSpace(body.ContentType),
		Ownership:     ownershipToDomain(body.Ownership),
		OnConflict:    mode,
		StageDir:      stageDir,
		Phase:         domain.UploadSessionInProgress,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := m.svc.CreateUploadSession(session); err != nil {
		return apiv1.UploadSessionResponse{}, err
	}
	direct := body.Direct
	if direct == nil {
		direct = fallbackDirect
	}
	return m.responseFor(r, session, direct)
}

func (m *uploadSessionManager) ensureSpace(stageRoot string, bytesNeeded int64) error {
	if bytesNeeded <= 0 {
		return nil
	}
	free, err := filesystem.FreeBytes(stageRoot)
	if err != nil {
		return err
	}
	needed := uint64(bytesNeeded)
	if minFreeBytes := m.live.uploadMinFreeBytes(); minFreeBytes > 0 {
		needed += uint64(minFreeBytes)
	}
	if free < needed {
		return domain.ErrInsufficientStorage
	}
	return nil
}

func (m *uploadSessionManager) handleCreate(w http.ResponseWriter, r *http.Request) {
	var body apiv1.UploadSessionCreateRequest
	if ok := decodeJSONBody(w, r, &body); !ok {
		return
	}
	res, err := m.createSession(r, body, 0, nil)
	if err != nil {
		statusFromErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, res)
}

func (m *uploadSessionManager) handleCreateBatch(w http.ResponseWriter, r *http.Request) {
	var body apiv1.UploadSessionBatchCreateRequest
	if ok := decodeJSONBody(w, r, &body); !ok {
		return
	}
	if len(body.Uploads) == 0 {
		writeErr(w, http.StatusBadRequest, "uploads required")
		return
	}
	if len(body.Uploads) > maxUploadSessionBatch {
		writeErr(w, http.StatusBadRequest, "too many uploads")
		return
	}
	sessions := make([]apiv1.UploadSessionResponse, 0, len(body.Uploads))
	for _, upload := range body.Uploads {
		res, err := m.createSession(r, upload, body.SegmentSize, body.Direct)
		if err != nil {
			for _, created := range sessions {
				session, lookupErr := m.svc.LookupUploadSession(created.ID)
				if lookupErr == nil && session != nil {
					_ = m.removeSessionArtifacts(*session)
				}
				_ = m.svc.DeleteUploadSession(created.ID)
				_ = m.svc.DeleteUploadCommitRecord(created.ID)
			}
			statusFromErr(w, err)
			return
		}
		sessions = append(sessions, res)
	}
	writeJSON(w, http.StatusCreated, apiv1.UploadSessionBatchCreateResponse{Sessions: sessions})
}

func segmentPath(session domain.UploadSession, index int) string {
	return filepath.Join(session.StageDir, fmt.Sprintf("%s-%05d.part", session.ID, index))
}

func checksumReader(h hash.Hash, r io.Reader, expectedSize int64) (string, int64, error) {
	bufPtr := copyBufPool.Get().(*[]byte)
	buf := *bufPtr
	defer copyBufPool.Put(bufPtr)
	var total int64
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if _, wErr := h.Write(buf[:n]); wErr != nil {
				return "", total, wErr
			}
			total += int64(n)
			if expectedSize >= 0 && total > expectedSize {
				return "", total, fmt.Errorf("segment too large")
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", total, err
		}
	}
	if expectedSize >= 0 && total != expectedSize {
		return "", total, fmt.Errorf("segment size mismatch")
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), total, nil
}

func writeSegmentFile(path string, body io.Reader, expectedSize int64) (string, int64, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", 0, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".upload-segment-*")
	if err != nil {
		return "", 0, err
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmpPath)
		}
	}()
	h := sha256.New()
	bufPtr := copyBufPool.Get().(*[]byte)
	buf := *bufPtr
	defer copyBufPool.Put(bufPtr)
	var written int64
	for {
		n, readErr := body.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if _, err := h.Write(chunk); err != nil {
				_ = tmp.Close()
				return "", 0, err
			}
			if _, err := tmp.Write(chunk); err != nil {
				_ = tmp.Close()
				return "", 0, err
			}
			written += int64(n)
			if written > expectedSize {
				_ = tmp.Close()
				return "", 0, fmt.Errorf("segment too large")
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			_ = tmp.Close()
			return "", 0, readErr
		}
	}
	if written != expectedSize {
		_ = tmp.Close()
		return "", 0, fmt.Errorf("segment size mismatch")
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return "", 0, err
	}
	if err := tmp.Close(); err != nil {
		return "", 0, err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return "", 0, err
	}
	if err := filesystem.SyncDir(filepath.Dir(path)); err != nil {
		return "", 0, err
	}
	committed = true
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), written, nil
}

func (m *uploadSessionManager) handleStatus(w http.ResponseWriter, r *http.Request) {
	sessionID := strings.TrimSpace(r.PathValue("sessionId"))
	if !uploadSessionIDRE.MatchString(sessionID) {
		writeErr(w, http.StatusBadRequest, "invalid session id")
		return
	}
	if !m.authorizeSession(r, sessionID, "status") {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	session, err := m.svc.LookupUploadSession(sessionID)
	if err != nil {
		statusFromErr(w, err)
		return
	}
	res, err := m.responseFor(r, *session, nil)
	if err != nil {
		statusFromErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (m *uploadSessionManager) handlePutSegment(w http.ResponseWriter, r *http.Request) {
	sessionID := strings.TrimSpace(r.PathValue("sessionId"))
	if !uploadSessionIDRE.MatchString(sessionID) {
		writeErr(w, http.StatusBadRequest, "invalid session id")
		return
	}
	index, err := strconv.Atoi(r.PathValue("index"))
	if err != nil || index < 0 {
		writeErr(w, http.StatusBadRequest, "invalid segment index")
		return
	}
	if !m.authorizeSession(r, sessionID, "putSegment") {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if err := m.acquireWriteSlot(r); err != nil {
		writeErr(w, http.StatusServiceUnavailable, "server busy")
		return
	}
	defer m.releaseWriteSlot()

	segLock := m.segmentLock(sessionID, index)
	segLock.Lock()
	defer segLock.Unlock()

	session, err := m.svc.LookupUploadSession(sessionID)
	if err != nil {
		statusFromErr(w, err)
		return
	}
	if session.Phase != domain.UploadSessionInProgress {
		writeErr(w, http.StatusConflict, "upload session is not in_progress")
		return
	}
	offset, expectedSize, err := expectedSegmentSize(*session, index)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid segment index")
		return
	}
	if r.ContentLength > expectedSize {
		writeErr(w, http.StatusRequestEntityTooLarge, "segment exceeds expected size")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, expectedSize+1)
	defer r.Body.Close()

	existingSegments, err := m.svc.ListUploadSegments(sessionID)
	if err != nil {
		statusFromErr(w, err)
		return
	}
	var existing *domain.UploadSegment
	for i := range existingSegments {
		if existingSegments[i].Index == index {
			existing = &existingSegments[i]
			break
		}
	}
	headerChecksum := strings.TrimSpace(r.Header.Get("X-Segment-Checksum"))
	if headerChecksum != "" && !checksumRE.MatchString(headerChecksum) {
		writeErr(w, http.StatusBadRequest, "invalid segment checksum")
		return
	}
	if existing != nil {
		var got string
		var size int64
		wroteMissing := false
		if _, statErr := os.Stat(existing.Path); errors.Is(statErr, os.ErrNotExist) {
			got, size, err = writeSegmentFile(existing.Path, r.Body, expectedSize)
			wroteMissing = err == nil
			if err == nil && got == existing.Checksum && size == existing.Size {
				existing.UpdatedAt = time.Now().UnixMilli()
				_ = m.svc.PutUploadSegment(*existing)
			}
		} else {
			got, size, err = checksumReader(sha256.New(), r.Body, expectedSize)
		}
		if err != nil || size != existing.Size {
			if wroteMissing {
				_ = os.Remove(existing.Path)
			}
			if isMaxBytesError(err) {
				writeErr(w, http.StatusRequestEntityTooLarge, "segment exceeds expected size")
				return
			}
			writeErr(w, http.StatusBadRequest, "invalid segment data")
			return
		}
		if got != existing.Checksum {
			if wroteMissing {
				_ = os.Remove(existing.Path)
			}
			writeErr(w, http.StatusConflict, "segment already exists with different content")
			return
		}
		writeJSON(w, http.StatusOK, apiv1.UploadSegmentResponse{
			SessionID:        sessionID,
			Index:            index,
			UploadedSegments: uploadedSegmentIndexes(existingSegments),
		})
		return
	}

	path := segmentPath(*session, index)
	checksum, written, err := writeSegmentFile(path, r.Body, expectedSize)
	if err != nil {
		if isMaxBytesError(err) {
			writeErr(w, http.StatusRequestEntityTooLarge, "segment exceeds expected size")
			return
		}
		writeErr(w, http.StatusBadRequest, "invalid segment data")
		return
	}
	if headerChecksum != "" && checksum != headerChecksum {
		_ = os.Remove(path)
		writeErr(w, http.StatusBadRequest, "segment checksum mismatch")
		return
	}
	now := time.Now().UnixMilli()
	segment := domain.UploadSegment{
		SessionID: sessionID,
		Index:     index,
		Offset:    offset,
		Size:      written,
		Checksum:  checksum,
		Path:      path,
		UpdatedAt: now,
	}

	lock := m.lock(sessionID)
	lock.Lock()
	current, err := m.svc.LookupUploadSession(sessionID)
	if err != nil {
		lock.Unlock()
		_ = os.Remove(path)
		statusFromErr(w, err)
		return
	}
	if current.Phase != domain.UploadSessionInProgress {
		lock.Unlock()
		_ = os.Remove(path)
		writeErr(w, http.StatusConflict, "upload session is not in_progress")
		return
	}
	if err := m.svc.PutUploadSegment(segment); err != nil {
		lock.Unlock()
		_ = os.Remove(path)
		statusFromErr(w, err)
		return
	}
	current.UpdatedAt = now
	_ = m.svc.UpdateUploadSession(*current)
	lock.Unlock()

	segments, _ := m.svc.ListUploadSegments(sessionID)
	writeJSON(w, http.StatusOK, apiv1.UploadSegmentResponse{
		SessionID:        sessionID,
		Index:            index,
		UploadedSegments: uploadedSegmentIndexes(segments),
	})
}

func isMaxBytesError(err error) bool {
	var maxErr *http.MaxBytesError
	return errors.As(err, &maxErr)
}

func (m *uploadSessionManager) handleCommit(w http.ResponseWriter, r *http.Request) {
	sessionID := strings.TrimSpace(r.PathValue("sessionId"))
	if !uploadSessionIDRE.MatchString(sessionID) {
		writeErr(w, http.StatusBadRequest, "invalid session id")
		return
	}
	if !m.authorizeSession(r, sessionID, "commit") {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	lock := m.lock(sessionID)
	lock.Lock()
	defer lock.Unlock()

	if record, err := m.svc.LookupUploadCommitRecord(sessionID); err == nil && record != nil {
		meta, err := m.svc.GetFile(record.FileID)
		if err != nil {
			statusFromErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, apiv1.UploadSessionCommitResponse{
			Node:     nodeResponse(meta),
			Checksum: record.Checksum,
		})
		return
	}

	session, err := m.svc.LookupUploadSession(sessionID)
	if err != nil {
		statusFromErr(w, err)
		return
	}
	if session.Phase != domain.UploadSessionInProgress && session.Phase != domain.UploadSessionCommitting {
		writeErr(w, http.StatusConflict, "upload session cannot be committed")
		return
	}
	if session.Phase == domain.UploadSessionCommitting {
		meta, recovered, err := m.recoverCommittedSession(*session)
		if err != nil {
			statusFromErr(w, err)
			return
		}
		if recovered {
			writeJSON(w, http.StatusOK, apiv1.UploadSessionCommitResponse{
				Node:     nodeResponse(meta),
				Checksum: session.Checksum,
			})
			return
		}
		writeErr(w, http.StatusConflict, "upload session commit outcome is ambiguous")
		return
	}
	segments, err := m.svc.ListUploadSegments(sessionID)
	if err != nil {
		statusFromErr(w, err)
		return
	}
	if len(segments) != session.TotalSegments {
		writeErr(w, http.StatusConflict, "upload session is incomplete")
		return
	}
	byIndex := make(map[int]domain.UploadSegment, len(segments))
	for _, segment := range segments {
		byIndex[segment.Index] = segment
	}

	completeDir := filepath.Join(filepath.Dir(session.StageDir), uploadSessionCompleteSubdir)
	if err := os.MkdirAll(completeDir, 0o700); err != nil {
		statusFromErr(w, err)
		return
	}
	completePath := filepath.Join(completeDir, session.ID+".complete")
	// An earlier commit attempt may have assembled the file and then failed
	// further along -- on a conflict at the destination, say. Reassembling is
	// not merely wasted work here: the single-segment path moves the staged
	// segment into place, so the input no longer exists. A recorded segment is
	// immutable (re-uploading different bytes is refused with a conflict), so
	// an assembled file can only hold the bytes the session declared, and the
	// checksum below verifies it either way.
	if _, statErr := os.Stat(completePath); errors.Is(statErr, os.ErrNotExist) {
		if err := assembleUploadSession(*session, byIndex, completePath); err != nil {
			statusFromErr(w, err)
			return
		}
	} else if statErr != nil {
		statusFromErr(w, statErr)
		return
	}
	hashes, size, err := hashWholeFile(completePath)
	if err != nil {
		statusFromErr(w, err)
		return
	}
	if size != session.Size || hashes.SHA256 != session.Checksum {
		writeErr(w, http.StatusBadRequest, "checksum mismatch")
		return
	}

	parentID, cleanupParents, err := m.ensureSessionParent(*session)
	if err != nil {
		statusFromErr(w, err)
		return
	}
	session.Phase = domain.UploadSessionCommitting
	session.ParentID = parentID
	session.UpdatedAt = time.Now().UnixMilli()
	if err := m.svc.UpdateUploadSession(*session); err != nil {
		cleanupParents()
		statusFromErr(w, err)
		return
	}
	meta, err := m.svc.ReplaceFileWithHashes(
		session.ParentID,
		session.Filename,
		completePath,
		session.Ownership,
		session.OnConflict,
		hashes,
	)
	if err != nil {
		cleanupParents()
		session.Phase = domain.UploadSessionInProgress
		session.UpdatedAt = time.Now().UnixMilli()
		_ = m.svc.UpdateUploadSession(*session)
		statusFromErr(w, err)
		return
	}
	now := time.Now().UnixMilli()
	session.Phase = domain.UploadSessionCommitted
	session.CompletedAt = now
	session.CompletedNode = meta.ID.String()
	session.UpdatedAt = now
	record := domain.UploadCommitRecord{
		SessionID:   session.ID,
		FileID:      meta.ID,
		Path:        session.Path,
		Checksum:    session.Checksum,
		CompletedAt: now,
	}
	if err := m.svc.CommitUploadSessionState(*session, record); err != nil {
		statusFromErr(w, err)
		return
	}
	_ = m.removeSessionArtifacts(*session)
	writeJSON(w, http.StatusOK, apiv1.UploadSessionCommitResponse{
		Node:     nodeResponse(meta),
		Checksum: session.Checksum,
	})
}

func (m *uploadSessionManager) recoverCommittedSession(session domain.UploadSession) (*domain.FileMeta, bool, error) {
	id, err := m.svc.ResolvePath(session.Path)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, false, nil
		}
		return nil, false, err
	}
	meta, err := m.svc.GetFile(id)
	if err != nil {
		return nil, false, err
	}
	if meta.Type != "file" || meta.Size != session.Size {
		return nil, false, nil
	}
	abs, err := m.svc.ResolveAbsPath(id)
	if err != nil {
		return nil, false, err
	}
	hashes, size, err := hashWholeFile(abs)
	if err != nil {
		return nil, false, err
	}
	if size != session.Size || hashes.SHA256 != session.Checksum {
		return nil, false, nil
	}
	now := time.Now().UnixMilli()
	session.Phase = domain.UploadSessionCommitted
	session.CompletedAt = now
	session.CompletedNode = meta.ID.String()
	session.UpdatedAt = now
	record := domain.UploadCommitRecord{
		SessionID:   session.ID,
		FileID:      meta.ID,
		Path:        session.Path,
		Checksum:    session.Checksum,
		CompletedAt: now,
	}
	if err := m.svc.CommitUploadSessionState(session, record); err != nil {
		return nil, false, err
	}
	_ = m.removeSessionArtifacts(session)
	return meta, true, nil
}

func (m *uploadSessionManager) ensureSessionParent(session domain.UploadSession) (domain.FileID, func(), error) {
	noop := func() {}
	parts := strings.Split(session.Path, "/")
	if len(parts) <= 2 {
		return session.ParentID, noop, nil
	}

	// Almost every commit lands in a directory that already exists, because an
	// earlier file in the same upload created it. One lookup settles that. The
	// loop below reaches the same answer by re-creating every level in turn, and
	// each of those levels costs a path lock, a filesystem walk of its own
	// prefix and an index read -- work that scales with tree depth and was being
	// paid once per file. Nothing is created here, so the rollback stays a
	// no-op, which is what it should be for a directory this commit found.
	parentPath := strings.Join(parts[:len(parts)-1], "/")
	if id, err := m.svc.ResolvePath(parentPath); err == nil {
		return id, noop, nil
	} else if !errors.Is(err, domain.ErrNotFound) {
		return domain.FileID{}, noop, err
	}

	root, _, err := m.mountRootByName(parts[0])
	if err != nil {
		return domain.FileID{}, noop, err
	}
	parentRelParts := parts[1 : len(parts)-1]

	// Note which levels are absent before creating anything, so the rollback
	// only ever removes directories this commit introduced. These are reads;
	// the mkdir below is the single write.
	missing := make([]string, 0, len(parentRelParts))
	for i := range parentRelParts {
		levelPath := parts[0] + "/" + strings.Join(parentRelParts[:i+1], "/")
		if _, err := m.svc.ResolvePath(levelPath); errors.Is(err, domain.ErrNotFound) {
			missing = append(missing, levelPath)
		} else if err != nil {
			return domain.FileID{}, noop, err
		}
	}

	// One recursive mkdir for the whole chain. Calling it once per level made
	// the same directories, but each call re-acquired a path lock and re-walked
	// its own prefix, so a chain of depth d cost d locks and d prefix walks
	// instead of one -- the dominant cost of committing into a deep tree.
	// Levels that were absent and now exist belong to this commit, and that has
	// to be evaluated on the failure path too: a recursive mkdir can create a
	// prefix and then stop at a level where a file sits where a directory
	// belongs, leaving those directories behind with nobody to remove them.
	// A level that still does not resolve is simply left out; the worst outcome
	// is an empty directory nobody cleans up.
	createdIDs := func() []domain.FileID {
		out := make([]domain.FileID, 0, len(missing))
		for _, levelPath := range missing {
			id, err := m.svc.ResolvePath(levelPath)
			if err != nil {
				continue
			}
			out = append(out, id)
		}
		return out
	}

	meta, err := m.svc.MkdirRelative(root.ID, strings.Join(parentRelParts, "/"), true, nil, domain.ConflictSkip)
	if err != nil {
		rollbackEmptyDirs(m.svc, createdIDs())
		return domain.FileID{}, noop, err
	}

	created := createdIDs()
	return meta.ID, func() { rollbackEmptyDirs(m.svc, created) }, nil
}

func rollbackEmptyDirs(svc *domain.Service, ids []domain.FileID) {
	for i := len(ids) - 1; i >= 0; i-- {
		abs, err := svc.ResolveAbsPath(ids[i])
		if err != nil {
			continue
		}
		entries, err := os.ReadDir(abs)
		if err != nil || len(entries) != 0 {
			continue
		}
		_ = svc.Delete(ids[i])
	}
}

// adoptSingleSegment moves a lone staged segment into place instead of copying
// it, reporting whether the move was used.
//
// Most small-file uploads are exactly one segment, and for those the copy was
// the entire cost of commit: the segment is read back, written out a second
// time, then read a third time to hash. A rename produces the same file for one
// directory update. The staged segment and the complete directory are siblings
// under the same session root, so the rename stays within one filesystem.
//
// The per-segment checksum comparison the copy loop performs is not lost. The
// caller hashes the assembled file and rejects the commit unless the size and
// SHA-256 match the session, and with one segment those are the same bytes the
// loop would have covered.
//
// A false return means fall back to copying. Rename can fail for reasons this
// code should not have to enumerate -- a segment staged on another device, a
// filesystem that refuses the operation -- and the copy path reports the real
// error if the input is genuinely unusable.
func adoptSingleSegment(session domain.UploadSession, segments map[int]domain.UploadSegment, completePath string) (bool, error) {
	segment, ok := segments[0]
	if !ok {
		return false, domain.ErrInvalidArgument
	}
	if segment.Size != session.Size {
		return false, fmt.Errorf("segment size mismatch")
	}
	if err := os.Rename(segment.Path, completePath); err != nil {
		return false, nil
	}
	if err := filesystem.SyncDir(filepath.Dir(completePath)); err != nil {
		return false, err
	}
	return true, nil
}

func assembleUploadSession(session domain.UploadSession, segments map[int]domain.UploadSegment, completePath string) error {
	if session.TotalSegments == 1 {
		moved, err := adoptSingleSegment(session, segments, completePath)
		if err != nil {
			return err
		}
		if moved {
			return nil
		}
	}

	tmp := completePath + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		_ = out.Close()
		if !committed {
			_ = os.Remove(tmp)
		}
	}()
	for i := 0; i < session.TotalSegments; i++ {
		segment, ok := segments[i]
		if !ok {
			return domain.ErrInvalidArgument
		}
		_, expectedSize, err := expectedSegmentSize(session, i)
		if err != nil {
			return err
		}
		if segment.Size != expectedSize {
			return fmt.Errorf("segment size mismatch")
		}
		f, err := os.Open(segment.Path)
		if err != nil {
			return err
		}
		got, _, err := checksumReader(sha256.New(), io.TeeReader(f, out), expectedSize)
		_ = f.Close()
		if err != nil {
			return err
		}
		if got != segment.Checksum {
			return fmt.Errorf("segment checksum mismatch")
		}
	}
	if err := out.Sync(); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, completePath); err != nil {
		return err
	}
	if err := filesystem.SyncDir(filepath.Dir(completePath)); err != nil {
		return err
	}
	committed = true
	return nil
}

func (m *uploadSessionManager) handleAbort(w http.ResponseWriter, r *http.Request) {
	sessionID := strings.TrimSpace(r.PathValue("sessionId"))
	if !uploadSessionIDRE.MatchString(sessionID) {
		writeErr(w, http.StatusBadRequest, "invalid session id")
		return
	}
	if !m.authorizeSession(r, sessionID, "abort") {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	lock := m.lock(sessionID)
	lock.Lock()
	defer lock.Unlock()
	session, err := m.svc.LookupUploadSession(sessionID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		statusFromErr(w, err)
		return
	}
	if session.Phase == domain.UploadSessionCommitted {
		writeErr(w, http.StatusConflict, "committed upload session cannot be aborted")
		return
	}
	_ = m.removeSessionArtifacts(*session)
	session.Phase = domain.UploadSessionAborted
	session.UpdatedAt = time.Now().UnixMilli()
	if err := m.svc.UpdateUploadSession(*session); err != nil {
		statusFromErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeSlotsInUse reports how many concurrent segment-write slots are held. The
// limit is already published via /v1/capabilities; this is the usage side of it.
func (m *uploadSessionManager) writeSlotsInUse() int {
	if m == nil {
		return 0
	}
	return len(m.writeSlots)
}

func (m *uploadSessionManager) writeSlotsLimit() int {
	if m == nil {
		return 0
	}
	return cap(m.writeSlots)
}
