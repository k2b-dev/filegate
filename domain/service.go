package domain

import (
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/google/uuid"
	lru "github.com/hashicorp/golang-lru/v2"
)

type pathCacheEntry struct {
	ID FileID
}

type normalizedOwnership struct {
	uid     *int
	gid     *int
	mode    *os.FileMode
	dirMode *os.FileMode
}

type ContentHashes struct {
	MD5Hex string
	SHA256 string
}

// Service is the central orchestrator that manages mounts, caching, and all
// CRUD operations against the index and filesystem store.
type Service struct {
	idx   Index
	store Store
	bus   EventBus

	mountByName   map[string]string
	mountIDByName map[string]FileID
	mountNameByID map[FileID]string
	mountNames    []string

	cache         *lru.Cache[string, pathCacheEntry]
	idPathCache   *lru.Cache[FileID, string]
	pathCacheSize int

	// Cumulative path-cache effectiveness. Occupancy alone cannot tell an
	// undersized cache from a cold one; the hit ratio can.
	pathCacheHits   atomic.Uint64
	pathCacheMisses atomic.Uint64
	dirSync         *coalescedDirSyncer
	mu              sync.RWMutex
	rescanMu        sync.Mutex

	// Versioning subsystem. EnableVersioning wires these from cli config
	// after NewService; default-zero means "feature off" so existing
	// callers (incl. legacy tests) keep working without changes.
	versioningEnabled bool
	versioningCfg     VersioningConfig
	versionLocks      *fileLockMap
	pathLocks         *pathLockManager
}

// NewService creates a Service with the given infrastructure adapters and mount paths.
func NewService(idx Index, store Store, bus EventBus, basePaths []string, pathCacheSize int) (*Service, error) {
	if len(basePaths) == 0 {
		return nil, fmt.Errorf("at least one base path required")
	}

	effectivePathCacheSize := max(pathCacheSize, 1000)
	cache, err := lru.New[string, pathCacheEntry](effectivePathCacheSize)
	if err != nil {
		return nil, err
	}
	idPathCache, err := lru.New[FileID, string](effectivePathCacheSize)
	if err != nil {
		return nil, err
	}

	svc := &Service{
		idx:           idx,
		store:         store,
		bus:           bus,
		mountByName:   make(map[string]string, len(basePaths)),
		mountIDByName: make(map[string]FileID, len(basePaths)),
		mountNameByID: make(map[FileID]string, len(basePaths)),
		cache:         cache,
		idPathCache:   idPathCache,
		pathCacheSize: effectivePathCacheSize,
		dirSync:       newDirSyncer(),
		versionLocks:  newFileLockMap(),
		pathLocks:     newPathLockManager(),
	}

	for _, p := range basePaths {
		abs, err := store.Abs(p)
		if err != nil {
			return nil, fmt.Errorf("base path invalid %q: %w", p, err)
		}
		name := filepath.Base(abs)
		if name == "." || name == string(filepath.Separator) || name == "" {
			return nil, fmt.Errorf("base path %q has invalid mount name", abs)
		}
		if _, exists := svc.mountByName[name]; exists {
			return nil, fmt.Errorf("duplicate mount name %q from base path %q", name, abs)
		}
		svc.mountByName[name] = abs
		svc.mountNames = append(svc.mountNames, name)
	}
	sort.Strings(svc.mountNames)

	if err := svc.bootstrapMounts(); err != nil {
		return nil, err
	}
	if err := svc.Rescan(); err != nil {
		return nil, err
	}

	return svc, nil
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func newID() (FileID, error) {
	u, err := uuid.NewV7()
	if err != nil {
		return FileID{}, err
	}
	var id FileID
	copy(id[:], u[:16])
	return id, nil
}

func parseModeString(v string) (os.FileMode, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, ErrInvalidArgument
	}
	u, err := strconv.ParseUint(v, 8, 32)
	if err != nil {
		return 0, ErrInvalidArgument
	}
	return os.FileMode(u), nil
}

func fileModeToDirMode(mode os.FileMode) os.FileMode {
	dir := mode
	if mode&0o400 != 0 {
		dir |= 0o100
	}
	if mode&0o040 != 0 {
		dir |= 0o010
	}
	if mode&0o004 != 0 {
		dir |= 0o001
	}
	return dir
}

func normalizeOwnership(ownership *Ownership) (*normalizedOwnership, error) {
	if ownership == nil {
		return nil, nil
	}
	out := &normalizedOwnership{
		uid: ownership.UID,
		gid: ownership.GID,
	}
	if (out.uid == nil) != (out.gid == nil) {
		return nil, ErrInvalidArgument
	}
	if ownership.Mode != "" {
		mode, err := parseModeString(ownership.Mode)
		if err != nil {
			return nil, err
		}
		out.mode = &mode
	}
	if ownership.DirMode != "" {
		mode, err := parseModeString(ownership.DirMode)
		if err != nil {
			return nil, err
		}
		out.dirMode = &mode
	}
	if out.mode != nil && out.dirMode == nil {
		derived := fileModeToDirMode(*out.mode)
		out.dirMode = &derived
	}
	return out, nil
}

func ownershipIsEmpty(ownership *Ownership) bool {
	if ownership == nil {
		return true
	}
	return ownership.UID == nil &&
		ownership.GID == nil &&
		strings.TrimSpace(ownership.Mode) == "" &&
		strings.TrimSpace(ownership.DirMode) == ""
}

func deriveFileModeFromDirMode(dirMode os.FileMode) os.FileMode {
	mode := dirMode &^ 0o111
	if mode == 0 {
		return 0o644
	}
	return mode
}

func (s *Service) inheritedOwnershipFromParent(parentID FileID) (*Ownership, error) {
	parent, err := s.GetFile(parentID)
	if err != nil {
		return nil, err
	}
	if parent.Type != "directory" {
		return nil, ErrInvalidArgument
	}
	uid := int(parent.UID)
	gid := int(parent.GID)
	dirMode := os.FileMode(parent.Mode & 0o777)
	fileMode := deriveFileModeFromDirMode(dirMode)
	return &Ownership{
		UID:     &uid,
		GID:     &gid,
		Mode:    fmt.Sprintf("%o", fileMode),
		DirMode: fmt.Sprintf("%o", dirMode),
	}, nil
}

func (s *Service) effectiveOwnership(parentID FileID, ownership *Ownership) (*Ownership, error) {
	if !ownershipIsEmpty(ownership) {
		return ownership, nil
	}
	return s.inheritedOwnershipFromParent(parentID)
}

func sanitizeRelativePath(relPath string) (string, error) {
	rel := strings.TrimSpace(relPath)
	if rel == "" {
		return "", ErrInvalidArgument
	}
	rel = filepath.ToSlash(rel)
	if strings.HasPrefix(rel, "/") {
		return "", ErrInvalidArgument
	}
	rel = path.Clean(rel)
	if rel == "." || rel == "" {
		return "", ErrInvalidArgument
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", ErrInvalidArgument
		}
		// Filegate-owned namespaces are reserved inside every mount.
		// Reject them anywhere in the path: at the mount root they are
		// where internal blobs live, deeper down they would shadow the
		// same names and make recovery/debugging ambiguous.
		if isFilegateReservedName(seg) || isFilegateInternalTempName(seg) {
			return "", ErrForbidden
		}
	}
	return rel, nil
}

func (s *Service) bootstrapMounts() error {
	for _, name := range s.mountNames {
		basePath := s.mountByName[name]
		id, err := s.store.GetID(basePath)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return err
			}
			id, err = newID()
			if err != nil {
				return err
			}
			if err := s.store.SetID(basePath, id); err != nil {
				return err
			}
		}

		s.mountIDByName[name] = id
		s.mountNameByID[id] = name

		entity := Entity{ID: id, ParentID: FileID{}, Name: name, IsDir: true}
		if err := s.idx.Batch(func(b Batch) error {
			b.PutEntity(entity)
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) ListRoot() []MountEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]MountEntry, 0, len(s.mountNames))
	for _, name := range s.mountNames {
		out = append(out, MountEntry{Name: name, ID: s.mountIDByName[name], Path: "/" + name})
	}
	return out
}

func (s *Service) ResolvePath(virtualPath string) (FileID, error) {
	vp, parts, err := normalizeVirtualPathInput(virtualPath)
	if err != nil {
		return FileID{}, err
	}
	return s.resolvePathID(vp, parts)
}

func normalizeVirtualPathInput(virtualPath string) (string, []string, error) {
	raw := strings.TrimSpace(virtualPath)
	raw = strings.TrimPrefix(raw, "/")
	if raw == "" {
		return "", nil, ErrInvalidArgument
	}
	for _, seg := range strings.Split(raw, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", nil, ErrInvalidArgument
		}
	}

	vp := path.Clean("/" + raw)
	vp = strings.TrimPrefix(vp, "/")
	if vp == "" || vp == "." {
		return "", nil, ErrInvalidArgument
	}
	return vp, strings.Split(vp, "/"), nil
}

func (s *Service) resolvePathID(vp string, parts []string) (FileID, error) {
	cached, cacheHit := s.cache.Get(vp)
	if cacheHit {
		s.pathCacheHits.Add(1)
	} else {
		s.pathCacheMisses.Add(1)
	}
	if cacheHit {
		s.idPathCache.Add(cached.ID, "/"+vp)
		return cached.ID, nil
	}

	s.mu.RLock()
	cur, ok := s.mountIDByName[parts[0]]
	s.mu.RUnlock()
	if !ok {
		return FileID{}, ErrNotFound
	}

	for i := 1; i < len(parts); i++ {
		child, err := s.idx.LookupChild(cur, parts[i])
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return FileID{}, ErrNotFound
			}
			return FileID{}, err
		}
		cur = child.ID
	}

	s.cache.Add(vp, pathCacheEntry{ID: cur})
	s.idPathCache.Add(cur, "/"+vp)
	return cur, nil
}

func isWithinBase(realPath, basePath string) bool {
	return realPath == basePath || strings.HasPrefix(realPath, basePath+string(os.PathSeparator))
}

func safeResolvedPath(candidatePath, basePath string) (string, error) {
	realPath, err := filepath.EvalSymlinks(candidatePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			parentReal, parentErr := filepath.EvalSymlinks(filepath.Dir(candidatePath))
			if parentErr != nil {
				if errors.Is(parentErr, os.ErrNotExist) {
					return "", ErrNotFound
				}
				return "", parentErr
			}
			realPath = filepath.Join(parentReal, filepath.Base(candidatePath))
		} else {
			return "", err
		}
	}
	if !isWithinBase(realPath, basePath) {
		return "", ErrForbidden
	}
	return realPath, nil
}

// maxParentChainDepth bounds the parent-chain walks in ResolveAbsPath
// and VirtualPath. A corrupted index with a parent cycle would
// otherwise spin these loops forever; resolveRootID and the pebble-side
// derivePath already guard against the same condition.
const maxParentChainDepth = 4096

func (s *Service) ResolveAbsPath(id FileID) (string, error) {
	if id.IsZero() {
		return "", ErrInvalidArgument
	}

	segments := make([]string, 0, 8)
	cur := id
	for depth := 0; ; depth++ {
		if depth >= maxParentChainDepth {
			return "", fmt.Errorf("parent chain too deep for %s (possible index cycle)", id)
		}
		e, err := s.idx.GetEntity(cur)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return "", ErrNotFound
			}
			return "", err
		}
		segments = append(segments, e.Name)
		if e.ParentID.IsZero() {
			break
		}
		cur = e.ParentID
	}

	mountName := segments[len(segments)-1]
	s.mu.RLock()
	basePath, ok := s.mountByName[mountName]
	s.mu.RUnlock()
	if !ok {
		return "", ErrNotFound
	}
	if len(segments) == 1 {
		return basePath, nil
	}

	rel := make([]string, 0, len(segments)-1)
	for i := len(segments) - 2; i >= 0; i-- {
		rel = append(rel, segments[i])
	}
	candidate := filepath.Join(append([]string{basePath}, rel...)...)
	return safeResolvedPath(candidate, basePath)
}

func (s *Service) VirtualPath(id FileID) (string, error) {
	if cached, ok := s.idPathCache.Get(id); ok {
		return cached, nil
	}

	segments := make([]string, 0, 8)
	cur := id
	for depth := 0; ; depth++ {
		if depth >= maxParentChainDepth {
			return "", fmt.Errorf("parent chain too deep for %s (possible index cycle)", id)
		}
		e, err := s.idx.GetEntity(cur)
		if err != nil {
			return "", err
		}
		segments = append(segments, e.Name)
		if e.ParentID.IsZero() {
			break
		}
		cur = e.ParentID
	}

	for i, j := 0, len(segments)-1; i < j; i, j = i+1, j-1 {
		segments[i], segments[j] = segments[j], segments[i]
	}
	vp := "/" + strings.Join(segments, "/")
	s.idPathCache.Add(id, vp)
	return vp, nil
}

func fileOwnership(info os.FileInfo) (uid uint32, gid uint32, mode uint32) {
	mode = uint32(info.Mode().Perm())
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, mode
	}
	return st.Uid, st.Gid, mode
}

// fileInodeIdentity extracts (device, inode, nlink) from a stat result. On
// platforms or filesystems where Sys() doesn't yield a *syscall.Stat_t the
// returned tuple is all zero — Inode reconciliation treats zero as "unknown"
// and skips, so this is the safe default for cross-platform builds.
func fileInodeIdentity(info os.FileInfo) (device, inode uint64, nlink uint32) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, 0
	}
	return uint64(st.Dev), uint64(st.Ino), uint32(st.Nlink)
}

func buildEntityMetadata(id, parentID FileID, name, absPath string, info os.FileInfo) Entity {
	uid, gid, mode := fileOwnership(info)
	device, inode, nlink := fileInodeIdentity(info)
	exif := map[string]string{}
	mimeType := ""
	if !info.IsDir() {
		mimeType = detectMimeType(name)
		exif = readEXIF(absPath, mimeType, name)
	}
	return Entity{
		ID:       id,
		ParentID: parentID,
		Name:     name,
		IsDir:    info.IsDir(),
		Size:     info.Size(),
		Mtime:    info.ModTime().UnixMilli(),
		UID:      uid,
		GID:      gid,
		Mode:     mode,
		Device:   device,
		Inode:    inode,
		Nlink:    nlink,
		MimeType: mimeType,
		Exif:     exif,
	}
}

// splitVirtualPath decomposes a virtual path "/mount/a/b/c" into
// (mount="mount", relPath="a/b/c"). The mount-only path "/mount"
// returns (mount="mount", relPath=""). Returns ok=false for the root
// "/" or invalid forms — callers treat those as "no flat-key
// position" (e.g. a mount root).
func splitVirtualPath(vp string) (mount, relPath string, ok bool) {
	if !strings.HasPrefix(vp, "/") || vp == "/" {
		return "", "", false
	}
	rest := vp[1:]
	sep := strings.IndexByte(rest, '/')
	if sep < 0 {
		return rest, "", true
	}
	return rest[:sep], rest[sep+1:], true
}

// preserveS3Metadata copies the ETag and S3-only metadata fields from
// src onto dst. buildEntityMetadata constructs an Entity purely from
// filesystem stat info and would otherwise wipe these fields on any
// rename/move/sync — preserveS3Metadata gives callers that already
// hold the prior entity a way to keep them across the rebuild.
//
// Callers that legitimately want to clear the fields (REST overwrite
// of an S3-uploaded file via syncSingleAfterLocalWrite) skip this and
// let the fields default to zero.
func preserveS3Metadata(dst *Entity, src *Entity) {
	if dst == nil || src == nil {
		return
	}
	dst.ETagMD5 = src.ETagMD5
	dst.SHA256 = src.SHA256
	dst.MultipartETag = src.MultipartETag
	dst.ContentType = src.ContentType
	dst.ContentEncoding = src.ContentEncoding
	dst.ContentDisposition = src.ContentDisposition
	dst.S3UserMetadata = src.S3UserMetadata
}

// hashFileContent reads absPath once and returns Filegate's persisted content
// hashes. Returns zero values with no error for non-regular files.
func hashFileContent(absPath string, info os.FileInfo) (ContentHashes, error) {
	if info != nil && !info.Mode().IsRegular() {
		return ContentHashes{}, nil
	}
	f, err := os.Open(absPath)
	if err != nil {
		return ContentHashes{}, err
	}
	defer f.Close()
	md5Hash := md5.New()
	shaHash := sha256.New()
	dst := io.MultiWriter(md5Hash, shaHash)
	buf := make([]byte, 128*1024)
	if _, err := io.CopyBuffer(dst, f, buf); err != nil {
		return ContentHashes{}, err
	}
	return ContentHashes{
		MD5Hex: hex.EncodeToString(md5Hash.Sum(nil)),
		SHA256: "sha256:" + hex.EncodeToString(shaHash.Sum(nil)),
	}, nil
}

func hashFileSHA256(absPath string, info os.FileInfo) (string, error) {
	if info != nil && !info.Mode().IsRegular() {
		return "", nil
	}
	f, err := os.Open(absPath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	buf := make([]byte, 128*1024)
	if _, err := io.CopyBuffer(h, f, buf); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

func fileMetaFromEntity(entity *Entity, vp string) *FileMeta {
	meta := &FileMeta{
		ID:       entity.ID,
		Type:     "file",
		Name:     entity.Name,
		Path:     vp,
		Size:     entity.Size,
		Mtime:    entity.Mtime,
		UID:      entity.UID,
		GID:      entity.GID,
		Mode:     entity.Mode,
		MimeType: entity.MimeType,
		Exif:     entity.Exif,
		ETag:     entity.ETagMD5,
		SHA256:   entity.SHA256,
		IsRoot:   entity.ParentID.IsZero(),
	}
	if entity.IsDir {
		meta.Type = "directory"
		meta.MimeType = ""
		meta.Exif = nil
	} else if meta.MimeType == "" {
		meta.MimeType = "application/octet-stream"
	}
	return meta
}

func sameFileSnapshot(before, after os.FileInfo) bool {
	if before == nil || after == nil {
		return false
	}
	beforeDev, beforeInode, _ := fileInodeIdentity(before)
	afterDev, afterInode, _ := fileInodeIdentity(after)
	return before.Size() == after.Size() &&
		before.ModTime().UnixNano() == after.ModTime().UnixNano() &&
		beforeDev == afterDev &&
		beforeInode == afterInode
}

func (s *Service) EnsureFileSHA256(id FileID) (*FileMeta, error) {
	entity, err := s.idx.GetEntity(id)
	if err != nil {
		return nil, err
	}
	vp, err := s.VirtualPath(id)
	if err != nil {
		return nil, err
	}
	if entity.IsDir || entity.SHA256 != "" {
		return fileMetaFromEntity(entity, vp), nil
	}
	abs, err := s.ResolveAbsPath(id)
	if err != nil {
		return nil, err
	}

	var sha string
	for attempt := 0; attempt < 2; attempt++ {
		before, err := os.Lstat(abs)
		if err != nil {
			return nil, err
		}
		if !before.Mode().IsRegular() {
			return nil, ErrInvalidArgument
		}
		sha, err = hashFileSHA256(abs, before)
		if err != nil {
			return nil, err
		}
		after, err := os.Lstat(abs)
		if err != nil {
			return nil, err
		}
		if sameFileSnapshot(before, after) {
			break
		}
		if attempt == 1 {
			return nil, ErrConflict
		}
	}

	after, err := os.Lstat(abs)
	if err != nil {
		return nil, err
	}
	fresh, err := s.idx.GetEntity(id)
	if err != nil {
		return nil, err
	}
	if fresh.IsDir || fresh.SHA256 != "" {
		return fileMetaFromEntity(fresh, vp), nil
	}
	afterDev, afterInode, _ := fileInodeIdentity(after)
	if fresh.Size != after.Size() ||
		fresh.Mtime != after.ModTime().UnixMilli() ||
		(fresh.Device != 0 && fresh.Inode != 0 && (fresh.Device != afterDev || fresh.Inode != afterInode)) {
		return nil, ErrConflict
	}
	fresh.SHA256 = sha
	if err := s.idx.Batch(func(b Batch) error {
		b.PutEntity(*fresh)
		return nil
	}); err != nil {
		return nil, err
	}
	return fileMetaFromEntity(fresh, vp), nil
}

func (s *Service) GetFileByVirtualPath(virtualPath string) (*FileMeta, error) {
	vp, parts, err := normalizeVirtualPathInput(virtualPath)
	if err != nil {
		return nil, err
	}
	id, err := s.resolvePathID(vp, parts)
	if err != nil {
		return nil, err
	}
	entity, err := s.idx.GetEntity(id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return fileMetaFromEntity(entity, "/"+vp), nil
}

func (s *Service) GetFile(id FileID) (*FileMeta, error) {
	entity, err := s.idx.GetEntity(id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	vp, err := s.VirtualPath(id)
	if err != nil {
		return nil, err
	}
	return fileMetaFromEntity(entity, vp), nil
}

func (s *Service) ensureIndexed(absPath string) (FileID, error) {
	id, err := s.store.GetID(absPath)
	if err == nil {
		// Same reasoning as parentIDForSync: an xattr is not proof of an
		// index row, so verify before handing the ID back. A path whose
		// xattr survived without its entity -- a sync interrupted between
		// the two writes, or a file copied in with the attribute intact --
		// is repaired here rather than returned as a dangling reference.
		if _, getErr := s.idx.GetEntity(id); getErr == nil {
			return id, nil
		} else if !errors.Is(getErr, ErrNotFound) {
			return FileID{}, getErr
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return FileID{}, err
	}

	if err := s.syncSingle(absPath); err != nil {
		return FileID{}, err
	}
	return s.store.GetID(absPath)
}

func (s *Service) computeDirectorySizeByIDBudget(dirID FileID, remainingNodes *int, deadline time.Time) (int64, bool) {
	if remainingNodes == nil || *remainingNodes <= 0 {
		return 0, false
	}
	if !deadline.IsZero() && time.Now().After(deadline) {
		return 0, false
	}

	var total int64
	stack := []FileID{dirID}
	seen := map[FileID]struct{}{}
	for len(stack) > 0 {
		if remainingNodes != nil {
			if *remainingNodes <= 0 {
				return 0, false
			}
			*remainingNodes--
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			return 0, false
		}
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if _, exists := seen[cur]; exists {
			continue
		}
		seen[cur] = struct{}{}
		children, err := s.listAllChildren(cur)
		if err != nil {
			continue
		}
		// Each enumerated child is one unit of work — decrementing only
		// per popped directory lets a single huge directory consume an
		// unbounded number of GetEntity calls under one budget tick.
		for _, child := range children {
			if remainingNodes != nil {
				if *remainingNodes <= 0 {
					return 0, false
				}
				*remainingNodes--
			}
			if !deadline.IsZero() && time.Now().After(deadline) {
				return 0, false
			}
			if child.IsDir {
				stack = append(stack, child.ID)
				continue
			}
			entity, err := s.idx.GetEntity(child.ID)
			if err != nil {
				continue
			}
			total += entity.Size
		}
	}
	return total, true
}

const recursiveSizeNodeBudget = 10_000_000

// RecursiveDirectorySize returns the sum of regular file sizes under dirID.
func (s *Service) RecursiveDirectorySize(dirID FileID) (int64, bool) {
	remainingNodes := recursiveSizeNodeBudget
	deadline := time.Now().Add(10 * time.Second)
	return s.computeDirectorySizeByIDBudget(dirID, &remainingNodes, deadline)
}

func (s *Service) ListNodeChildren(parentID FileID, cursor string, pageSize int, computeRecursiveSizes bool) (*ListedNodes, error) {
	if pageSize <= 0 {
		pageSize = 100
	}
	if pageSize > 1000 {
		pageSize = 1000
	}

	meta, err := s.GetFile(parentID)
	if err != nil {
		return nil, err
	}
	if meta.Type != "directory" {
		return nil, ErrInvalidArgument
	}
	var after ChildCursor
	if cursor != "" {
		parsed, ok := ParseChildCursor(cursor)
		if !ok {
			// Legacy bare-name cursor: resolve its sort zone via a
			// lookup, exactly like the pre-typed-cursor behavior —
			// including ErrInvalidArgument when the name is unknown.
			// Typed tokens never hit this path, so pagination with a
			// server-issued NextCursor survives concurrent deletion
			// of the cursor entry.
			entry, err := s.idx.LookupChild(parentID, cursor)
			if err != nil {
				return nil, ErrInvalidArgument
			}
			parsed = ChildCursor{Name: entry.Name, IsDir: entry.IsDir}
		}
		after = parsed
	}

	entries, err := s.idx.ListChildren(parentID, after, pageSize+1)
	if err != nil {
		return nil, err
	}
	hasMore := len(entries) > pageSize
	if hasMore {
		entries = entries[:pageSize]
	}

	items := make([]FileMeta, 0, pageSize)
	remainingNodes := recursiveSizeNodeBudget
	deadline := time.Now().Add(10 * time.Second)
	parentVP, parentVPErr := s.VirtualPath(parentID)
	for _, entry := range entries {
		childMeta, err := s.GetFile(entry.ID)
		if err != nil {
			continue
		}
		if childMeta.Type == "directory" && computeRecursiveSizes {
			if size, ok := s.computeDirectorySizeByIDBudget(entry.ID, &remainingNodes, deadline); ok {
				childMeta.Size = size
			}
		}
		// Override Name + Path with the directory entry's view, not the
		// entity's stored canonical name. For hardlinks two dirents may
		// share an entity ID but each must surface under its own name in
		// a directory listing — otherwise listing the dir would return
		// duplicates of the entity's canonical name.
		copyMeta := *childMeta
		copyMeta.Name = entry.Name
		if parentVPErr == nil {
			copyMeta.Path = parentVP + "/" + entry.Name
		}
		items = append(items, copyMeta)
	}

	listed := &ListedNodes{Items: items}
	if hasMore && len(entries) > 0 {
		listed.NextCursor = EncodeChildCursor(entries[len(entries)-1])
	}
	return listed, nil
}

func splitEvenly(limit, n int) []int {
	if n <= 0 {
		return nil
	}
	if limit < 0 {
		limit = 0
	}
	out := make([]int, n)
	base := limit / n
	rest := limit % n
	for i := range n {
		out[i] = base
		if i < rest {
			out[i]++
		}
	}
	return out
}

func hasHiddenSegment(relPath string) bool {
	for _, seg := range strings.Split(relPath, "/") {
		if strings.HasPrefix(seg, ".") {
			return true
		}
	}
	return false
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if _, exists := seen[v]; exists {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

func sanitizeVirtualPath(raw string) (string, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return "", ErrInvalidArgument
	}
	v = strings.TrimPrefix(v, "/")
	return sanitizeRelativePath(v)
}

func globErrorCause(err error) string {
	switch {
	case errors.Is(err, ErrNotFound):
		return "not found"
	case errors.Is(err, ErrForbidden):
		return "forbidden"
	case errors.Is(err, ErrInvalidArgument):
		return "invalid path"
	default:
		return err.Error()
	}
}

func (s *Service) SearchGlob(req GlobSearchRequest) (*GlobSearchResponse, error) {
	pattern := strings.TrimSpace(req.Pattern)
	if pattern == "" {
		return nil, ErrInvalidArgument
	}
	if _, err := doublestar.Match(pattern, ""); err != nil {
		return nil, ErrInvalidArgument
	}

	limit := req.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 5000 {
		limit = 5000
	}

	includeFiles := req.IncludeFiles
	includeDirs := req.IncludeDirs
	if !includeFiles && !includeDirs {
		includeFiles = true
	}

	response := &GlobSearchResponse{
		Results: make([]FileMeta, 0, limit),
		Errors:  make([]GlobSearchError, 0),
		Paths:   make([]GlobSearchPathResult, 0),
	}

	s.mu.RLock()
	defaultMounts := append([]string(nil), s.mountNames...)
	s.mu.RUnlock()

	requestedPaths := uniqueStrings(req.Paths)
	if len(requestedPaths) == 0 {
		for _, mount := range defaultMounts {
			requestedPaths = append(requestedPaths, "/"+mount)
		}
	}
	if len(requestedPaths) == 0 || limit == 0 {
		return response, nil
	}

	type searchTarget struct {
		requestedPath string
		virtualPath   string
		absPath       string
	}
	targets := make([]searchTarget, 0, len(requestedPaths))
	seenVirtualPaths := make(map[string]struct{}, len(requestedPaths))
	for _, requestedPath := range requestedPaths {
		virtualPath, err := sanitizeVirtualPath(requestedPath)
		if err != nil {
			response.Errors = append(response.Errors, GlobSearchError{
				Path:  requestedPath,
				Cause: "invalid path",
			})
			continue
		}
		id, err := s.ResolvePath(virtualPath)
		if err != nil {
			response.Errors = append(response.Errors, GlobSearchError{
				Path:  requestedPath,
				Cause: globErrorCause(err),
			})
			continue
		}
		meta, err := s.GetFile(id)
		if err != nil {
			response.Errors = append(response.Errors, GlobSearchError{
				Path:  requestedPath,
				Cause: globErrorCause(err),
			})
			continue
		}
		if meta.Type != "directory" {
			response.Errors = append(response.Errors, GlobSearchError{
				Path:  requestedPath,
				Cause: "path is not a directory",
			})
			continue
		}
		absPath, err := s.ResolveAbsPath(id)
		if err != nil {
			response.Errors = append(response.Errors, GlobSearchError{
				Path:  requestedPath,
				Cause: globErrorCause(err),
			})
			continue
		}
		virtualCanonical, err := s.VirtualPath(id)
		if err != nil {
			response.Errors = append(response.Errors, GlobSearchError{
				Path:  requestedPath,
				Cause: globErrorCause(err),
			})
			continue
		}
		if _, exists := seenVirtualPaths[virtualCanonical]; exists {
			continue
		}
		seenVirtualPaths[virtualCanonical] = struct{}{}
		targets = append(targets, searchTarget{
			requestedPath: requestedPath,
			virtualPath:   virtualCanonical,
			absPath:       absPath,
		})
	}
	if len(targets) == 0 {
		return response, nil
	}

	quotas := splitEvenly(limit, len(targets))
	type rootResult struct {
		items    []FileMeta
		err      string
		pathInfo GlobSearchPathResult
	}
	out := make([]rootResult, len(targets))

	var wg sync.WaitGroup
	for i := range targets {
		target := targets[i]
		quota := quotas[i]
		if quota <= 0 {
			out[i].pathInfo = GlobSearchPathResult{
				Path:     target.virtualPath,
				Returned: 0,
				HasMore:  true,
			}
			continue
		}

		wg.Add(1)
		go func(idx int, virtualPath, rootPath string, maxItems int) {
			defer wg.Done()

			if _, err := os.Stat(rootPath); err != nil {
				if errors.Is(err, os.ErrNotExist) {
					out[idx].err = "path does not exist"
					return
				}
				out[idx].err = err.Error()
				return
			}

			items := make([]FileMeta, 0, maxItems)
			hasMore := false
			walkErr := filepath.WalkDir(rootPath, func(current string, d os.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				if current == rootPath {
					return nil
				}

				rel, err := filepath.Rel(rootPath, current)
				if err != nil {
					return nil
				}
				rel = filepath.ToSlash(rel)

				if !req.ShowHidden && hasHiddenSegment(rel) {
					if d.IsDir() {
						return filepath.SkipDir
					}
					return nil
				}

				match, err := doublestar.Match(pattern, rel)
				if err != nil || !match {
					return nil
				}

				if d.IsDir() && !includeDirs {
					return nil
				}
				if !d.IsDir() && !includeFiles {
					return nil
				}
				if len(items) >= maxItems {
					hasMore = true
					return io.EOF
				}

				id, err := s.ensureIndexed(current)
				if err != nil {
					return nil
				}
				meta, err := s.GetFile(id)
				if err != nil {
					return nil
				}
				items = append(items, *meta)
				return nil
			})
			if walkErr != nil && !errors.Is(walkErr, io.EOF) {
				out[idx].err = walkErr.Error()
			}
			out[idx].pathInfo = GlobSearchPathResult{
				Path:     virtualPath,
				Returned: len(items),
				HasMore:  hasMore,
			}
			out[idx].items = items
		}(i, target.virtualPath, target.absPath, quota)
	}
	wg.Wait()

	for i := range targets {
		if out[i].pathInfo.Path == "" {
			out[i].pathInfo = GlobSearchPathResult{
				Path:     targets[i].virtualPath,
				Returned: len(out[i].items),
				HasMore:  false,
			}
		}
		response.Paths = append(response.Paths, out[i].pathInfo)

		if out[i].err != "" {
			response.Errors = append(response.Errors, GlobSearchError{
				Path:  targets[i].requestedPath,
				Cause: out[i].err,
			})
			continue
		}
		response.Results = append(response.Results, out[i].items...)
	}

	return response, nil
}

func (s *Service) OpenContent(id FileID) (io.ReadCloser, int64, bool, error) {
	// Read consistency: a stat-then-open sequence has a TOCTOU window
	// where an overwrite (tmp+rename) can swap the inode out from
	// under us — Content-Length and body would then mismatch on the
	// wire, which S3 clients (and rclone) treat as integrity
	// failures. Path-lock alone closes the window only against
	// filegate-managed writers; an external `mv` over the file would
	// still race. The robust fix is to OPEN first and derive the
	// size from the opened fd via Stat(), so the size we report is
	// guaranteed to match the bytes the fd will deliver — Linux
	// POSIX semantics guarantee an opened fd keeps reading the
	// inode it was opened against, regardless of what subsequent
	// renames do at the same path.
	abs, err := s.ResolveAbsPath(id)
	if err != nil {
		return nil, 0, false, err
	}
	// Stat first to detect directories — opening a directory and
	// trying to read it yields os-specific weirdness on Linux.
	info, err := s.store.Stat(abs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, 0, false, ErrNotFound
		}
		return nil, 0, false, err
	}
	if info.IsDir() {
		return nil, 0, true, nil
	}
	// Open via os.Open directly so we can derive size from the
	// fd. The store interface returns an io.ReadCloser, which
	// hides the underlying *os.File and prevents Stat-on-fd.
	f, err := os.Open(abs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, 0, false, ErrNotFound
		}
		return nil, 0, false, err
	}
	fdInfo, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, false, err
	}
	return f, fdInfo.Size(), false, nil
}

func (s *Service) WriteContent(id FileID, body io.Reader) error {
	// withFilePointLock acquires the path-lock, then the file-id-lock,
	// then re-resolves the path to verify the file is still where we
	// thought before letting fn run. This is the "lock then
	// revalidate" pattern formalised: the path-lock prevents another
	// path-mutating op (rename, delete, S3 PUT to the same target)
	// from interleaving, and the revalidate check catches the case
	// where one already raced ahead between our initial path read
	// and the lock acquisition.
	return s.withFilePointLock(id, func() error {
		meta, err := s.GetFile(id)
		if err != nil {
			return err
		}
		if meta.Type != "file" {
			return ErrInvalidArgument
		}

		abs, err := s.ResolveAbsPath(id)
		if err != nil {
			return err
		}

		preserveID := meta.ID
		// Snapshot the existing bytes BEFORE the atomic write clobbers
		// them. captureBeforeOverwrite is best-effort and never fails
		// the user's write — the worst-case is a missed version, not a
		// missed mutation.
		s.captureBeforeOverwrite(id, abs)
		hashes, err := s.writeFileAtomic(abs, body, os.FileMode(meta.Mode), ownershipFromFileMeta(meta), &preserveID, false)
		if err != nil {
			return err
		}
		if err := s.syncSingleAfterLocalWrite(abs, hashes); err != nil {
			return err
		}
		s.bus.Publish(Event{Type: EventUpdated, ID: id, Path: abs, At: time.Now()})
		return nil
	})
}

// ReplaceFile places the file at srcPath under parentID/name, honoring the
// supplied ConflictMode. The returned FileMeta reflects the actually-used
// final name, which may differ from `name` when mode is ConflictRename.
func (s *Service) ReplaceFile(parentID FileID, name string, srcPath string, ownership *Ownership, mode ConflictMode) (*FileMeta, error) {
	return s.replaceFile(parentID, name, srcPath, ownership, mode, ContentHashes{})
}

func (s *Service) ReplaceFileWithHashes(parentID FileID, name string, srcPath string, ownership *Ownership, mode ConflictMode, hashes ContentHashes) (*FileMeta, error) {
	return s.replaceFile(parentID, name, srcPath, ownership, mode, hashes)
}

func (s *Service) replaceFile(parentID FileID, name string, srcPath string, ownership *Ownership, mode ConflictMode, hashes ContentHashes) (*FileMeta, error) {
	name = strings.TrimSpace(name)
	if name == "" || strings.Contains(name, "/") {
		return nil, ErrInvalidArgument
	}
	parentMeta, err := s.GetFile(parentID)
	if err != nil {
		return nil, err
	}
	if parentMeta.Type != "directory" {
		return nil, ErrInvalidArgument
	}

	// Acquire the destination path-lock to serialize against any
	// concurrent path-mutating op on the same target (S3 PutObject
	// with the same key, REST PUT to the same virtual path, another
	// upload-session commit that lost a race). Held for the entire body
	// of ReplaceFile.
	parentVP, err := s.VirtualPath(parentID)
	if err != nil {
		return nil, err
	}
	pmount, prel, vpOK := splitVirtualPath(parentVP)
	if !vpOK {
		return nil, ErrInvalidArgument
	}
	var dstRel string
	if prel == "" {
		dstRel = name
	} else {
		dstRel = prel + "/" + name
	}
	dstRelease := s.pathLocks.AcquirePoint(pathLockKey(pmount, dstRel))
	defer dstRelease()

	parentAbs, err := s.ResolveAbsPath(parentID)
	if err != nil {
		return nil, err
	}
	targetPath := filepath.Join(parentAbs, name)

	// Resolve the conflict before we touch anything else. We want to fail
	// fast and preserve the existing file untouched if the mode demands it.
	if info, statErr := s.store.Stat(targetPath); statErr == nil {
		if info.IsDir() {
			// A file upload cannot replace a directory regardless of mode —
			// silently nuking a subtree from a single PUT is too dangerous
			// and not what `overwrite` is supposed to mean here.
			return nil, ErrConflict
		}
		switch mode {
		case ConflictError, "":
			return nil, ErrConflict
		case ConflictRename:
			targetPath = makeUniquePath(targetPath)
		case ConflictOverwrite:
			// fall through — the existing replace path below handles it.
		default:
			return nil, ErrInvalidArgument
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return nil, statErr
	}

	existingID, err := s.store.GetID(targetPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	// Hold the per-file mutation lock for overwrite cases. Race
	// argument: a concurrent Delete on the existing entity could
	// complete between our existingID lookup and the rename below,
	// leaving us about to write to a slot whose entity we believe
	// still exists. We re-check the entity inside the lock — if
	// it's been deleted, fall through to fresh-slot create.
	if !existingID.IsZero() {
		mu := s.versionLocks.Acquire(existingID)
		mu.Lock()
		defer mu.Unlock()
		// Revalidate inside the lock: if the file got deleted between
		// the GetID call above and our lock acquire, the existingID
		// is stale. Treat as a fresh slot.
		if _, geErr := s.idx.GetEntity(existingID); errors.Is(geErr, ErrNotFound) {
			existingID = FileID{}
		} else if geErr == nil {
			s.captureBeforeOverwrite(existingID, targetPath)
		}
	}
	sourceID, err := s.store.GetID(srcPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	fallbackCopy := false

	// On Linux, rename(2) is an atomic replace when the target exists and
	// is a regular file. Doing a Remove+Rename creates a TOCTOU window
	// where readers see "no such file" between the two syscalls AND a
	// crash between the two leaves the target permanently gone. Rely on
	// rename's atomic replace; reserve the copy fallback for cross-device
	// failures only.
	if err := s.store.Rename(srcPath, targetPath); err != nil {
		fallbackCopy = true
		src, openErr := s.store.OpenRead(srcPath)
		if openErr != nil {
			return nil, openErr
		}
		defer src.Close()

		dst, createErr := s.store.OpenWrite(targetPath, 0o644)
		if createErr != nil {
			return nil, createErr
		}
		if _, copyErr := io.Copy(dst, src); copyErr != nil {
			_ = dst.Close()
			return nil, copyErr
		}
		if closeErr := dst.Close(); closeErr != nil {
			return nil, closeErr
		}
		if err := syncFilePath(targetPath); err != nil {
			return nil, err
		}
		_ = s.store.Remove(srcPath)
	}

	if !existingID.IsZero() {
		if err := s.store.SetID(targetPath, existingID); err != nil {
			return nil, err
		}
	} else if fallbackCopy && !sourceID.IsZero() {
		if err := s.store.SetID(targetPath, sourceID); err != nil {
			return nil, err
		}
	}
	effectiveOwnership, err := s.effectiveOwnership(parentID, ownership)
	if err != nil {
		return nil, err
	}
	if err := s.applyOwnership(targetPath, effectiveOwnership, false); err != nil {
		return nil, err
	}
	if hashes.MD5Hex != "" || hashes.SHA256 != "" {
		if err := s.syncSingleAfterLocalWrite(targetPath, hashes); err != nil {
			return nil, err
		}
	} else if err := s.syncSingle(targetPath); err != nil {
		return nil, err
	}
	id, err := s.store.GetID(targetPath)
	if err != nil {
		return nil, err
	}
	// existingID was captured BEFORE the rename, so it tells us whether the
	// target slot was already populated. Replace = update, fresh slot = create.
	eventType := EventCreated
	if !existingID.IsZero() {
		eventType = EventUpdated
	}
	s.bus.Publish(Event{Type: eventType, ID: id, Path: targetPath, At: time.Now()})
	if eventType == EventCreated {
		// Auto V1 for the freshly-placed file (subject to the size floor).
		s.captureFirstVersion(id, targetPath)
	}
	return s.GetFile(id)
}

func applyOwnershipOne(path string, isDir bool, normalized *normalizedOwnership) error {
	if normalized.uid != nil && normalized.gid != nil {
		if err := os.Chown(path, *normalized.uid, *normalized.gid); err != nil {
			if !errors.Is(err, syscall.EPERM) && !errors.Is(err, syscall.EACCES) {
				return err
			}
			info, statErr := os.Stat(path)
			if statErr != nil {
				return err
			}
			st, ok := info.Sys().(*syscall.Stat_t)
			if !ok || int(st.Uid) != *normalized.uid || int(st.Gid) != *normalized.gid {
				return err
			}
		}
	}
	if isDir {
		if normalized.dirMode != nil {
			if err := os.Chmod(path, *normalized.dirMode); err != nil {
				return err
			}
		}
		return nil
	}
	if normalized.mode != nil {
		if err := os.Chmod(path, *normalized.mode); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) applyOwnership(absPath string, ownership *Ownership, recursive bool) error {
	normalized, err := normalizeOwnership(ownership)
	if err != nil {
		return err
	}
	if normalized == nil {
		return nil
	}

	if !recursive {
		info, err := os.Stat(absPath)
		if err != nil {
			return err
		}
		return applyOwnershipOne(absPath, info.IsDir(), normalized)
	}

	return filepath.WalkDir(absPath, func(current string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.Type()&os.ModeSymlink != 0 {
			// Never follow symlinks during recursive ownership changes.
			return nil
		}
		isDir := d.IsDir()
		return applyOwnershipOne(current, isDir, normalized)
	})
}

func (s *Service) isMountRoot(id FileID) bool {
	e, err := s.idx.GetEntity(id)
	if err != nil {
		return false
	}
	return e.ParentID.IsZero()
}

// isMountRootAbsPath reports whether absPath is exactly the on-disk
// path of one of the configured mount roots. Used to apply mount-root-
// only invariants (e.g. excluding the .fg-versions namespace from
// reconcile passes).
func (s *Service) isMountRootAbsPath(absPath string) bool {
	cleaned := filepath.Clean(absPath)
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, mountAbs := range s.mountByName {
		if mountAbs == cleaned {
			return true
		}
	}
	return false
}

func isFilegateReservedName(name string) bool {
	return name == versionsDirName || name == uploadsDirName
}

// isPathInsideReservedNamespace reports whether absPath sits inside
// any mount's reserved Filegate-owned subtree. Used by every code path
// that might receive a filesystem-driven path the user shouldn't be
// able to manipulate via the public API.
func (s *Service) isPathInsideReservedNamespace(absPath string) bool {
	cleaned := filepath.Clean(absPath)
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, mountAbs := range s.mountByName {
		for _, name := range []string{versionsDirName, uploadsDirName} {
			nsRoot := filepath.Join(mountAbs, name)
			if cleaned == nsRoot {
				return true
			}
			if strings.HasPrefix(cleaned, nsRoot+string(filepath.Separator)) {
				return true
			}
		}
	}
	return false
}

func isFilegateInternalTempName(name string) bool {
	return strings.HasPrefix(name, ".") && strings.Contains(name, ".filegate-tmp-")
}

// MkdirRelative creates relPath under parentID. The leaf segment respects
// the supplied ConflictMode (error/skip/rename). Intermediate segments are
// always treated as ConflictSkip — an existing directory in the middle of
// the path is reused, otherwise mkdir -p style traversal would be
// impossible.
//
// Allowed modes: ConflictError, ConflictSkip, ConflictRename. Any other
// mode (notably ConflictOverwrite — which would mean recursive subtree
// deletion) returns ErrInvalidArgument; use Transfer with overwrite for
// that.
func (s *Service) MkdirRelative(parentID FileID, relPath string, recursive bool, ownership *Ownership, mode ConflictMode) (*FileMeta, error) {
	switch mode {
	case "":
		mode = ConflictError
	case ConflictError, ConflictSkip, ConflictRename:
		// allowed
	default:
		return nil, ErrInvalidArgument
	}

	parentMeta, err := s.GetFile(parentID)
	if err != nil {
		return nil, err
	}
	if parentMeta.Type != "directory" {
		return nil, ErrInvalidArgument
	}
	rel, err := sanitizeRelativePath(relPath)
	if err != nil {
		return nil, err
	}

	// Path-lock the LEAF (the deepest path being created). Intermediate
	// segments use os.MkdirAll which is idempotent under FS-level
	// race; the leaf is what races with sibling creates / deletes /
	// renames at the same path.
	parentVP, err := s.VirtualPath(parentID)
	if err != nil {
		return nil, err
	}
	pmount, prel, vpOK := splitVirtualPath(parentVP)
	if !vpOK {
		return nil, ErrInvalidArgument
	}
	var leafRel string
	if prel == "" {
		leafRel = rel
	} else {
		leafRel = prel + "/" + rel
	}
	mkRelease := s.pathLocks.AcquirePoint(pathLockKey(pmount, leafRel))
	defer mkRelease()

	effectiveOwnership, err := s.effectiveOwnership(parentID, ownership)
	if err != nil {
		return nil, err
	}
	normalized, err := normalizeOwnership(effectiveOwnership)
	if err != nil {
		return nil, err
	}

	parentAbs, err := s.ResolveAbsPath(parentID)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(rel, "/")
	current := parentAbs
	createdAny := false
	// The exact levels this call created, top down. The loop creates one level
	// per iteration -- it walks down, so a segment's parent always exists by the
	// time MkdirAll runs -- which makes this the precise chain to index.
	createdChain := make([]string, 0, len(parts))
	for i, seg := range parts {
		isLeaf := i == len(parts)-1
		next := filepath.Join(current, seg)
		info, lstatErr := os.Lstat(next)
		if lstatErr == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				return nil, ErrForbidden
			}
			// rename at the leaf always wins: even if the existing entry is
			// a file (type mismatch), we just produce a unique sibling name
			// and create our directory there.
			if isLeaf && mode == ConflictRename {
				next = makeUniquePath(next)
			} else if !info.IsDir() {
				// Existing file blocks any non-rename mode at any segment:
				// no mode can sensibly turn a file into a directory.
				return nil, ErrConflict
			} else if !isLeaf {
				// Intermediate dir: always reuse — otherwise mkdir -p fails.
				current = next
				continue
			} else {
				// Leaf is an existing dir; user's mode decides.
				switch mode {
				case ConflictError:
					return nil, ErrConflict
				case ConflictSkip:
					current = next
					continue
				}
			}
		} else if !errors.Is(lstatErr, os.ErrNotExist) {
			return nil, lstatErr
		}

		if !recursive && i < len(parts)-1 {
			return nil, ErrNotFound
		}

		dirPerm := os.FileMode(0o755)
		if normalized != nil && normalized.dirMode != nil {
			dirPerm = *normalized.dirMode
		}
		if err := s.store.MkdirAll(next, dirPerm); err != nil {
			return nil, err
		}
		createdAny = true
		createdChain = append(createdChain, next)
		current = next
	}

	targetAbs := current
	if createdAny {
		// Only touch the exact directory levels in this request's creation chain.
		// A recursive walk from the first new parent can enter sibling requests'
		// temporary files while they are being atomically renamed, turning a
		// successful concurrent write into an ENOENT from chown/chmod.
		if normalized != nil {
			for _, created := range createdChain {
				if err := applyOwnershipOne(created, true, normalized); err != nil {
					return nil, err
				}
			}
		}
		// One batch for the whole chain instead of one synced write per level.
		if err := s.indexNewDirChain(createdChain); err != nil {
			return nil, err
		}
	}

	id, err := s.ensureIndexed(targetAbs)
	if err != nil {
		return nil, err
	}
	// Only emit when we actually created the leaf directory. Idempotent
	// "skip on existing" calls are no-ops semantically and emit nothing.
	if createdAny {
		s.bus.Publish(Event{Type: EventCreated, ID: id, Path: targetAbs, At: time.Now()})
	}
	return s.GetFile(id)
}

// WriteContentByVirtualPath writes body to the file at virtualPath. The
// returned FileMeta reflects the actually-written name, which may differ
// from the requested one when mode is ConflictRename. The bool result is
// true when a new file was created (false when an existing one was
// replaced).
func (s *Service) WriteContentByVirtualPath(virtualPath string, body io.Reader, mode ConflictMode) (*FileMeta, bool, error) {
	vp, err := sanitizeVirtualPath(virtualPath)
	if err != nil {
		return nil, false, err
	}
	parts := strings.Split(vp, "/")
	if len(parts) < 2 {
		return nil, false, ErrInvalidArgument
	}
	fileName := strings.TrimSpace(parts[len(parts)-1])
	if fileName == "" {
		return nil, false, ErrInvalidArgument
	}

	s.mu.RLock()
	mountID, ok := s.mountIDByName[parts[0]]
	s.mu.RUnlock()
	if !ok {
		return nil, false, ErrNotFound
	}

	parentID := mountID
	if len(parts) > 2 {
		parentPath := strings.Join(parts[1:len(parts)-1], "/")
		// Parent path must always be skip-on-existing-dir, otherwise every
		// PUT to data/foo/bar.txt would 409 the second time. The user's
		// onConflict only governs the leaf file.
		parentMeta, err := s.MkdirRelative(mountID, parentPath, true, nil, ConflictSkip)
		if err != nil {
			return nil, false, err
		}
		parentID = parentMeta.ID
	}

	targetID, err := s.ResolvePath(vp)
	if err == nil {
		targetMeta, getErr := s.GetFile(targetID)
		if getErr != nil {
			return nil, false, getErr
		}
		// Rename always succeeds: pick a unique sibling name regardless of
		// whether the existing target is a file or a directory. The new
		// upload becomes its own file with the unique name.
		if mode == ConflictRename {
			parentAbs, resolveErr := s.ResolveAbsPath(parentID)
			if resolveErr != nil {
				return nil, false, resolveErr
			}
			fileName = filepath.Base(makeUniquePath(filepath.Join(parentAbs, fileName)))
		} else if targetMeta.Type != "file" {
			// For error/overwrite, refuse to act on a non-file target
			// (overwriting a directory subtree from a single PUT is too
			// dangerous; that's what Transfer with overwrite is for).
			return nil, false, ErrConflict
		} else {
			switch mode {
			case ConflictError, "":
				return nil, false, ErrConflict
			case ConflictOverwrite:
				if writeErr := s.WriteContent(targetID, body); writeErr != nil {
					return nil, false, writeErr
				}
				updated, getErr := s.GetFile(targetID)
				if getErr != nil {
					return nil, false, getErr
				}
				return updated, false, nil
			default:
				return nil, false, ErrInvalidArgument
			}
		}
	} else if !errors.Is(err, ErrNotFound) {
		return nil, false, err
	}

	updated, err := s.createAndWriteContent(parentID, fileName, body, nil)
	if err != nil {
		return nil, false, err
	}
	return updated, true, nil
}

func (s *Service) CreateChild(parentID FileID, name string, isDir bool, ownership *Ownership) (*FileMeta, error) {
	name = strings.TrimSpace(name)
	if name == "" || strings.Contains(name, "/") {
		return nil, ErrInvalidArgument
	}
	parentMeta, err := s.GetFile(parentID)
	if err != nil {
		return nil, err
	}
	if parentMeta.Type != "directory" {
		return nil, ErrInvalidArgument
	}

	// Path-lock the new child's path so a concurrent CreateChild,
	// Delete, or rename on the same name can't race with us. The
	// existence-check + create section runs inside the lock.
	parentVP, err := s.VirtualPath(parentID)
	if err != nil {
		return nil, err
	}
	pmount, prel, vpOK := splitVirtualPath(parentVP)
	if !vpOK {
		return nil, ErrInvalidArgument
	}
	var newRel string
	if prel == "" {
		newRel = name
	} else {
		newRel = prel + "/" + name
	}
	newRelease := s.pathLocks.AcquirePoint(pathLockKey(pmount, newRel))
	defer newRelease()

	parentAbs, err := s.ResolveAbsPath(parentID)
	if err != nil {
		return nil, err
	}
	abs := filepath.Join(parentAbs, name)
	if _, err := s.store.Stat(abs); err == nil {
		return nil, ErrConflict
	}
	effectiveOwnership, err := s.effectiveOwnership(parentID, ownership)
	if err != nil {
		return nil, err
	}
	normalized, err := normalizeOwnership(effectiveOwnership)
	if err != nil {
		return nil, err
	}

	filePerm := os.FileMode(0o644)
	dirPerm := os.FileMode(0o755)
	if normalized != nil {
		if normalized.mode != nil {
			filePerm = *normalized.mode
		}
		if normalized.dirMode != nil {
			dirPerm = *normalized.dirMode
		}
	}

	// Atomic no-replace creates. The Stat pre-check above gives the
	// friendly ErrConflict for the common case, but it leaves a window
	// against external writers; Mkdir / commitNoReplace (via
	// writeFileAtomic mustNotExist) fail with EEXIST instead of
	// truncating whatever appeared in between.
	var hashes ContentHashes
	if isDir {
		if err := os.Mkdir(abs, dirPerm); err != nil {
			if errors.Is(err, os.ErrExist) {
				return nil, ErrConflict
			}
			return nil, err
		}
		if err := s.applyOwnership(abs, effectiveOwnership, true); err != nil {
			return nil, err
		}
	} else {
		newFileID, idErr := newID()
		if idErr != nil {
			return nil, idErr
		}
		// writeFileAtomic applies ownership to the temp file before the
		// atomic link, so no separate applyOwnership pass is needed.
		var writeErr error
		hashes, writeErr = s.writeFileAtomic(abs, strings.NewReader(""), filePerm, effectiveOwnership, &newFileID, true)
		if writeErr != nil {
			return nil, writeErr
		}
	}
	var syncErr error
	if isDir {
		syncErr = s.syncSingle(abs)
	} else {
		syncErr = s.syncSingleAfterLocalWrite(abs, hashes)
	}
	if syncErr != nil {
		return nil, syncErr
	}
	id, err := s.store.GetID(abs)
	if err != nil {
		return nil, err
	}
	s.bus.Publish(Event{Type: EventCreated, ID: id, Path: abs, At: time.Now()})
	return s.GetFile(id)
}

func (s *Service) UpdateNode(id FileID, name *string, ownership *Ownership, recursiveOwnership bool) (*FileMeta, error) {
	if name == nil && ownership == nil {
		return nil, ErrInvalidArgument
	}

	// Pre-lock peek: determine whether this is a rename and (if so)
	// what old + new path-lock keys to acquire. We read the entity
	// once here, then take the appropriate locks, then re-resolve
	// inside the lock to confirm the file is still where we thought
	// (lock-then-revalidate pattern — see withFilePointLock for the
	// rationale).
	peek, err := s.idx.GetEntity(id)
	if err != nil {
		return nil, err
	}
	if peek.ParentID.IsZero() && name != nil {
		return nil, ErrForbidden
	}
	oldKey, oldKeyOK := s.pathLockKeyForID(id)
	if !oldKeyOK {
		return nil, ErrForbidden
	}
	var release func()
	if name != nil && strings.TrimSpace(*name) != peek.Name {
		newName := strings.TrimSpace(*name)
		// Compute the new lock key — same parent, new leaf name.
		parentVP, perr := s.VirtualPath(peek.ParentID)
		if perr != nil {
			return nil, perr
		}
		pmount, prel, ok := splitVirtualPath(parentVP)
		if !ok {
			return nil, ErrInvalidArgument
		}
		var newRel string
		if prel == "" {
			newRel = newName
		} else {
			newRel = prel + "/" + newName
		}
		newKey := pathLockKey(pmount, newRel)
		if peek.IsDir {
			release = s.pathLocks.AcquireSubtreePair(oldKey, newKey)
		} else {
			release = s.pathLocks.AcquirePointPair(oldKey, newKey)
		}
	} else if peek.IsDir && recursiveOwnership {
		// Recursive ownership update touches every descendant —
		// take a subtree lock so concurrent path-mutating ops on
		// the descendants serialize.
		release = s.pathLocks.AcquireSubtree(oldKey)
	} else {
		release = s.pathLocks.AcquirePoint(oldKey)
	}
	defer release()

	fileMu := s.versionLocks.Acquire(id)
	fileMu.Lock()
	defer fileMu.Unlock()

	entity, err := s.idx.GetEntity(id)
	if err != nil {
		return nil, err
	}
	if currentKey, ok := s.pathLockKeyForID(id); !ok || currentKey != oldKey {
		// File moved or vanished between the peek and the lock.
		return nil, ErrNotFound
	}

	abs, err := s.ResolveAbsPath(id)
	if err != nil {
		return nil, err
	}
	oldCachePath := ""
	if vp, err := s.VirtualPath(id); err == nil {
		oldCachePath = normalizeCacheKey(vp)
	}

	renamed := false
	if name != nil {
		newName := strings.TrimSpace(*name)
		if newName == "" || strings.Contains(newName, "/") {
			return nil, ErrInvalidArgument
		}
		if entity.ParentID.IsZero() {
			return nil, ErrForbidden
		}

		if newName != entity.Name {
			renamed = true
			parentAbs, err := s.ResolveAbsPath(entity.ParentID)
			if err != nil {
				return nil, err
			}
			targetAbs := filepath.Join(parentAbs, newName)
			if _, err := s.store.Stat(targetAbs); err == nil {
				return nil, ErrConflict
			} else if err != nil && !errors.Is(err, os.ErrNotExist) {
				return nil, err
			}

			// No-replace rename: the Stat above gives the friendly
			// conflict answer, but an external writer can still drop a
			// file at targetAbs in between — plain rename(2) would
			// silently clobber it.
			if err := s.store.RenameNoReplace(abs, targetAbs); err != nil {
				if errors.Is(err, os.ErrExist) {
					return nil, ErrConflict
				}
				return nil, err
			}
			abs = targetAbs

			info, err := s.store.Stat(abs)
			if err != nil {
				return nil, err
			}
			oldName := entity.Name
			entity.Name = newName
			// PutEntity in the index layer auto-detects same-id
			// renames and rekeys the affected flat-key entries
			// (descendant rewrite for directories, leaf swap for
			// files). No domain-side ReKey/Del needed here.
			if err := s.idx.Batch(func(b Batch) error {
				b.DelChild(entity.ParentID, oldName)
				b.PutEntity(*entity)
				b.PutChild(entity.ParentID, newName, DirEntry{
					ID:    id,
					Name:  newName,
					IsDir: info.IsDir(),
					Size:  info.Size(),
					Mtime: info.ModTime().UnixMilli(),
				})
				return nil
			}); err != nil {
				return nil, err
			}
		}
	}

	if err := s.applyOwnership(abs, ownership, recursiveOwnership); err != nil {
		return nil, err
	}
	if recursiveOwnership && entity.IsDir {
		if entity.ParentID.IsZero() {
			if err := s.RescanMount(abs); err != nil {
				return nil, err
			}
		} else {
			if err := s.syncSubtree(abs); err != nil {
				return nil, err
			}
		}
	} else {
		// syncSingle is not suitable for mount roots because it tries to index the parent.
		if !entity.ParentID.IsZero() {
			if err := s.syncSingle(abs); err != nil {
				return nil, err
			}
		} else {
			if err := s.RescanMount(abs); err != nil {
				return nil, err
			}
		}
	}

	if oldCachePath != "" {
		s.invalidateCachePrefix(oldCachePath)
		if parent := parentCacheKey(oldCachePath); parent != "" {
			s.cache.Remove(parent)
		}
	}
	s.invalidateCacheByID(id)
	// Rename → EventMoved (entity stays, path/name changes). Pure
	// ownership change → EventUpdated. abs has been reassigned to the
	// post-rename path inside the rename branch.
	eventType := EventUpdated
	if renamed {
		eventType = EventMoved
	}
	s.bus.Publish(Event{Type: eventType, ID: id, Path: abs, At: time.Now()})
	return s.GetFile(id)
}

func makeUniquePath(target string) string {
	dir := filepath.Dir(target)
	base := filepath.Base(target)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	for i := 1; i <= 999; i++ {
		candidate := filepath.Join(dir, fmt.Sprintf("%s-%02d%s", stem, i, ext))
		if _, err := os.Stat(candidate); errors.Is(err, os.ErrNotExist) {
			return candidate
		}
	}
	return filepath.Join(dir, fmt.Sprintf("%s-%d%s", stem, time.Now().UnixMilli(), ext))
}

func (s *Service) copyPath(sourceAbs, targetAbs string, preserveIDs bool) error {
	linfo, err := os.Lstat(sourceAbs)
	if err != nil {
		return err
	}
	if linfo.Mode()&os.ModeSymlink != 0 {
		return ErrForbidden
	}

	info, err := s.store.Stat(sourceAbs)
	if err != nil {
		return err
	}

	if info.IsDir() {
		if err := s.store.MkdirAll(targetAbs, info.Mode().Perm()); err != nil {
			return err
		}
		if preserveIDs {
			if id, err := s.store.GetID(sourceAbs); err == nil {
				if err := s.store.SetID(targetAbs, id); err != nil {
					return err
				}
			}
		}
		entries, err := s.store.ReadDir(sourceAbs)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := s.copyPath(filepath.Join(sourceAbs, entry.Name()), filepath.Join(targetAbs, entry.Name()), preserveIDs); err != nil {
				return err
			}
		}
		return nil
	}

	r, err := s.store.OpenRead(sourceAbs)
	if err != nil {
		return err
	}
	defer r.Close()
	w, err := s.store.OpenWrite(targetAbs, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(w, r); err != nil {
		_ = w.Close()
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	if err := os.Chmod(targetAbs, info.Mode().Perm()); err != nil {
		return err
	}
	if preserveIDs {
		if id, err := s.store.GetID(sourceAbs); err == nil {
			if err := s.store.SetID(targetAbs, id); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Service) Transfer(req TransferRequest) (*FileMeta, error) {
	req.Op = strings.ToLower(strings.TrimSpace(req.Op))
	if req.Op != "move" && req.Op != "copy" {
		return nil, ErrInvalidArgument
	}
	if strings.TrimSpace(req.TargetName) == "" || strings.Contains(req.TargetName, "/") {
		return nil, ErrInvalidArgument
	}
	// Caller (HTTP adapter) must pass a parsed/validated ConflictMode.
	// Defensively normalize an empty value to the default to avoid an
	// uncovered code path if a non-HTTP caller forgot to.
	if req.OnConflict == "" {
		req.OnConflict = ConflictError
	}
	switch req.OnConflict {
	case ConflictError, ConflictOverwrite, ConflictRename:
		// allowed for transfer
	default:
		return nil, ErrInvalidArgument
	}
	recursiveOwnership := true
	if req.RecursiveOwnership != nil {
		recursiveOwnership = *req.RecursiveOwnership
	}

	// Peek both endpoints to determine lock flavor (point vs subtree)
	// before acquiring. The locks are released via defer below; lock-
	// then-revalidate pattern means we re-check the source's path
	// after lock acquisition before acting.
	sourcePeek, err := s.idx.GetEntity(req.SourceID)
	if err != nil {
		return nil, err
	}
	if sourcePeek.ParentID.IsZero() {
		return nil, ErrForbidden
	}
	parentPeek, err := s.idx.GetEntity(req.TargetParentID)
	if err != nil {
		return nil, err
	}
	if !parentPeek.IsDir {
		return nil, ErrInvalidArgument
	}
	srcKey, srcOK := s.pathLockKeyForID(req.SourceID)
	if !srcOK {
		return nil, ErrForbidden
	}
	parentVP, err := s.VirtualPath(req.TargetParentID)
	if err != nil {
		return nil, err
	}
	parentMount, parentRel, vpOK := splitVirtualPath(parentVP)
	if !vpOK {
		return nil, ErrInvalidArgument
	}
	var dstRel string
	if parentRel == "" {
		dstRel = req.TargetName
	} else {
		dstRel = parentRel + "/" + req.TargetName
	}
	dstKey := pathLockKey(parentMount, dstRel)
	// Always subtree-pair regardless of source flavor: ConflictOverwrite
	// can call deleteSubtree on a destination directory, and the dest
	// shape isn't known until we Stat under the lock. Point-locks on a
	// directory destination would let descendants be racily mutated
	// during the overwrite-delete. Pessimistic — Transfer is rare.
	release := s.pathLocks.AcquireSubtreePair(srcKey, dstKey)
	defer release()

	sourceMeta, err := s.GetFile(req.SourceID)
	if err != nil {
		return nil, err
	}
	if sourceMeta.IsRoot {
		return nil, ErrForbidden
	}
	if currentKey, ok := s.pathLockKeyForID(req.SourceID); !ok || currentKey != srcKey {
		// Source moved or was deleted between peek and lock.
		return nil, ErrNotFound
	}
	parentMeta, err := s.GetFile(req.TargetParentID)
	if err != nil {
		return nil, err
	}
	if parentMeta.Type != "directory" {
		return nil, ErrInvalidArgument
	}
	effectiveOwnership, err := s.effectiveOwnership(req.TargetParentID, req.Ownership)
	if err != nil {
		return nil, err
	}

	sourceAbs, err := s.ResolveAbsPath(req.SourceID)
	if err != nil {
		return nil, err
	}
	targetParentAbs, err := s.ResolveAbsPath(req.TargetParentID)
	if err != nil {
		return nil, err
	}
	if req.Op == "move" && sourceMeta.Type == "directory" {
		rel, relErr := filepath.Rel(sourceAbs, targetParentAbs)
		if relErr == nil && rel != "." && !strings.HasPrefix(rel, "..") {
			return nil, ErrInvalidArgument
		}
	}

	targetAbs := filepath.Join(targetParentAbs, req.TargetName)
	finalTargetName := req.TargetName
	if _, err := s.store.Stat(targetAbs); err == nil {
		switch req.OnConflict {
		case ConflictOverwrite:
			if existing, lookupErr := s.idx.LookupChild(req.TargetParentID, req.TargetName); lookupErr == nil {
				if err := s.deleteSubtree(existing.ID); err != nil {
					return nil, err
				}
			}
			if err := s.store.RemoveAll(targetAbs); err != nil {
				return nil, err
			}
		case ConflictRename:
			targetAbs = makeUniquePath(targetAbs)
			finalTargetName = filepath.Base(targetAbs)
		case ConflictError:
			return nil, ErrConflict
		default:
			return nil, ErrConflict
		}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	if req.Op == "move" {
		// No-replace rename: the conflict was resolved above (error
		// checked / target deleted / unique name picked), but an
		// external writer can still drop a file at targetAbs in the
		// window — plain rename(2) would silently clobber it. EEXIST
		// must NOT fall through to the copy path, which would write
		// over the target; only genuine rename failures (EXDEV) do.
		if err := s.store.RenameNoReplace(sourceAbs, targetAbs); err != nil {
			if errors.Is(err, os.ErrExist) {
				return nil, ErrConflict
			}
			if err := s.copyPath(sourceAbs, targetAbs, true); err != nil {
				return nil, err
			}
			if err := s.store.RemoveAll(sourceAbs); err != nil {
				return nil, err
			}
		}
		if err := s.applyOwnership(targetAbs, effectiveOwnership, recursiveOwnership); err != nil {
			return nil, err
		}
		if err := s.reParentNode(req.SourceID, sourceMeta.Name, req.TargetParentID, finalTargetName); err != nil {
			return nil, err
		}
		if recursiveOwnership && sourceMeta.Type == "directory" {
			if err := s.syncSubtree(targetAbs); err != nil {
				return nil, err
			}
		}
		// Move keeps the entity ID; path changed. EventMoved with the new
		// absolute path.
		s.bus.Publish(Event{Type: EventMoved, ID: req.SourceID, Path: targetAbs, At: time.Now()})
		return s.GetFile(req.SourceID)
	}

	if err := s.copyPath(sourceAbs, targetAbs, false); err != nil {
		return nil, err
	}
	if err := s.applyOwnership(targetAbs, effectiveOwnership, recursiveOwnership); err != nil {
		return nil, err
	}
	if err := s.syncSubtree(targetAbs); err != nil {
		return nil, err
	}
	newID, err := s.store.GetID(targetAbs)
	if err != nil {
		return nil, err
	}
	// Copy creates a fresh entity at the target path. EventCreated
	// covers the subtree root; descendants of a copied directory are
	// already represented under the same root.
	s.bus.Publish(Event{Type: EventCreated, ID: newID, Path: targetAbs, At: time.Now()})
	return s.GetFile(newID)
}

func (s *Service) reParentNode(id FileID, oldName string, newParentID FileID, newName string) error {
	entity, err := s.idx.GetEntity(id)
	if err != nil {
		return err
	}
	oldParentID := entity.ParentID
	// Capture the pre-move path while the index still reflects it: the
	// moved node's descendants have id→path cache entries under THIS
	// prefix, and they must be dropped after the move (the full-purge
	// that used to hide this is gone).
	oldVP, oldVPErr := s.VirtualPath(id)

	newParentAbs, err := s.ResolveAbsPath(newParentID)
	if err != nil {
		return err
	}
	newAbs := filepath.Join(newParentAbs, newName)
	info, err := s.store.Stat(newAbs)
	if err != nil {
		return err
	}

	updated := buildEntityMetadata(id, newParentID, newName, newAbs, info)
	// reParentNode rebuilds the entity purely from filesystem stat
	// info, so without this the ETag and any S3-uploaded metadata
	// would be wiped on every rename/move. The bytes haven't changed,
	// only the path, so we preserve the prior ETag and S3 fields.
	preserveS3Metadata(&updated, entity)

	if err := s.idx.Batch(func(b Batch) error {
		b.DelChild(oldParentID, oldName)
		// PutEntity in the index layer auto-detects the same-id
		// rename and rekeys descendant flat-key entries (for
		// directories) or swaps the leaf flat-key (for files). We
		// don't need to call ReKeyFlatPrefix here.
		b.PutEntity(updated)
		b.PutChild(newParentID, newName, DirEntry{
			ID:    id,
			Name:  newName,
			IsDir: updated.IsDir,
			Size:  updated.Size,
			Mtime: updated.Mtime,
		})
		return nil
	}); err != nil {
		return err
	}
	oldParentPath, _ := s.VirtualPath(oldParentID)
	newParentPath, _ := s.VirtualPath(newParentID)
	if oldVPErr == nil {
		s.invalidateCachePrefix(oldVP)
	} else {
		s.purgePathCaches()
	}
	s.invalidateCacheByID(id)
	s.InvalidatePathCache(oldParentPath)
	s.InvalidatePathCache(newParentPath)
	return nil
}

func (s *Service) syncSubtree(absPath string) error {
	absPath = filepath.Clean(absPath)

	parentAbs := filepath.Dir(absPath)
	parentID, err := s.store.GetID(parentAbs)
	if err != nil {
		return err
	}

	type item struct {
		entity Entity
		entry  DirEntry
	}
	pathToID := map[string]FileID{parentAbs: parentID}
	collected := make([]item, 0, 64)

	err = filepath.WalkDir(absPath, func(current string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}

		parent := filepath.Dir(current)
		pid, ok := pathToID[parent]
		if !ok {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return nil
		}
		// Same conflict-aware ID resolution as the Rescan walk uses.
		// syncSubtree is invoked for newly-added directory trees (e.g.
		// after a Transfer that brought in a sub-tree containing files
		// with pre-existing xattrs); the conflict rule must run here too.
		id, err := s.resolveOrReissueID(current, info)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}

		pathToID[current] = id
		entity := buildEntityMetadata(id, pid, d.Name(), current, info)
		collected = append(collected, item{
			entity: entity,
			entry: DirEntry{
				ID:    id,
				Name:  d.Name(),
				IsDir: d.IsDir(),
				Size:  info.Size(),
				Mtime: info.ModTime().UnixMilli(),
			},
		})
		return nil
	})
	if err != nil {
		return err
	}

	if err := s.idx.Batch(func(b Batch) error {
		for _, it := range collected {
			b.PutEntity(it.entity)
			b.PutChild(it.entity.ParentID, it.entity.Name, it.entry)
		}
		return nil
	}); err != nil {
		return err
	}
	// Cache invalidation happens here, but no event is emitted: syncSubtree
	// is a low-level primitive used by both create-style flows (Transfer
	// copy) and update-style flows (Transfer move with recursive ownership).
	// The semantic event (Created / Moved / Updated) is emitted by the
	// public caller for the subtree root.
	if rootID, idErr := s.store.GetID(absPath); idErr == nil {
		s.invalidateCacheByID(rootID)
	} else {
		s.purgePathCaches()
	}
	return nil
}

func (s *Service) Delete(id FileID) error {
	// Determine point vs subtree lock by reading the entity once
	// up-front. The result is best-effort — if the file vanished
	// between the peek and the lock acquire, withFile/Subtree returns
	// ErrNotFound from the revalidate check inside.
	entity, err := s.idx.GetEntity(id)
	if err != nil {
		return err
	}
	if entity.ParentID.IsZero() {
		return ErrForbidden
	}

	fn := func() error { return s.deleteCoreLocked(id) }
	if entity.IsDir {
		return s.withSubtreeLockByID(id, fn)
	}
	return s.withFilePointLock(id, fn)
}

// deleteCoreLocked is the actual delete body, factored out so that
// other methods (DeleteIfMatch — see service_s3_write.go) can run
// it inside a lock that ALSO performs other invariant checks.
// Caller MUST already hold the appropriate path-lock + file-id-lock
// for id (typically via withFilePointLock or withSubtreeLockByID).
func (s *Service) deleteCoreLocked(id FileID) error {
	meta, err := s.GetFile(id)
	if err != nil {
		return err
	}
	if meta.IsRoot {
		return ErrForbidden
	}
	abs, err := s.ResolveAbsPath(id)
	if err != nil {
		return err
	}
	if err := s.store.RemoveAll(abs); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrNotFound
		}
		return err
	}
	if err := s.deleteSubtree(id); err != nil {
		return err
	}
	// EventDeleted is published by deleteSubtree itself.
	return nil
}

// resolveOrReissueID returns the stable ID for absPath. It reads the
// user.filegate.id xattr; if missing it mints a fresh UUID and writes it.
// If the xattr names an existing entity that is anchored to a DIFFERENT
// inode (snapshot copy or `cp -a` clone — the xattr was preserved by the
// copy operation), it re-issues a fresh ID for absPath instead of letting
// the new path silently steal the original's stable identity.
//
// This is the key invariant that lets Filegate stay correct in the face
// of filesystem operations that duplicate xattrs (snapshots, cp -a):
// xattr identity is stable across in-place modifications but explicitly
// not stable across path duplication.
func (s *Service) resolveOrReissueID(absPath string, info os.FileInfo) (FileID, error) {
	id, err := s.store.GetID(absPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return FileID{}, err
		}
		return s.claimID(absPath)
	}

	device, inode, _ := fileInodeIdentity(info)
	existing, err := s.idx.GetEntity(id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// xattr names an ID we don't know yet — first time indexing this entity.
			return id, nil
		}
		return FileID{}, err
	}
	// Older entities written before format-version 5 have Inode/Device == 0;
	// treat zero as "no inode info recorded" and trust the xattr.
	if existing.Inode == 0 || (existing.Device == device && existing.Inode == inode) {
		return id, nil
	}
	// Different inode in the entity record — possible snapshot/cp-a
	// duplicate. Verify by stat'ing the recorded path. If it's gone or
	// at a different inode, the recorded entity is stale and it's safe
	// to take over its ID. Otherwise, re-issue.
	existingAbs, err := s.claimedAbsPath(existing)
	if err != nil || existingAbs == absPath {
		return id, nil
	}
	existingInfo, err := s.store.Stat(existingAbs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Recorded entity's path is gone — stale, take it over.
			return id, nil
		}
		// Other stat errors (permission, transient IO, weird FS): be
		// conservative and re-issue. Letting the new path silently take
		// over the existing ID on a flaky stat would let a duplicate
		// xattr clobber the live original.
		return s.mintAndSetID(absPath)
	}
	existingDev, existingIno, _ := fileInodeIdentity(existingInfo)
	if existingDev == existing.Device && existingIno == existing.Inode {
		// Confirmed conflict: another live path owns this xattr ID at a
		// different inode. Mint a fresh ID for our path so we don't
		// trample the original's identity.
		return s.mintAndSetID(absPath)
	}
	// Recorded path's inode shifted — entity is stale, take it over.
	return id, nil
}

// claimID assigns an ID to a path that has none, tolerating a concurrent
// claimant.
//
// First-time indexing is reachable from several requests at once -- two uploads
// creating sibling directories both recurse into the shared parent -- and an
// unconditional write there let each caller keep its own ID while the xattr
// held only the last one. Whoever writes first wins and everyone adopts that
// value.
func (s *Service) claimID(absPath string) (FileID, error) {
	id, err := newID()
	if err != nil {
		return FileID{}, err
	}
	settled, _, err := s.store.SetIDIfAbsent(absPath, id)
	if err != nil {
		return FileID{}, err
	}
	return settled, nil
}

// mintAndSetID generates a fresh UUID v7 and writes it to absPath's xattr
// unconditionally. Only for deliberate re-issue on xattr conflict, where an
// existing value is exactly what has to be replaced; first-time indexing goes
// through claimID.
func (s *Service) mintAndSetID(absPath string) (FileID, error) {
	id, err := newID()
	if err != nil {
		return FileID{}, err
	}
	if err := s.store.SetID(absPath, id); err != nil {
		return FileID{}, err
	}
	return id, nil
}

// claimedAbsPath reconstructs the absolute filesystem path that an entity
// record claims to live at, by joining its parent's path with its Name.
// Distinct from ResolveAbsPath which runs EvalSymlinks via safeResolvedPath
// and returns ErrForbidden when the resolved real path falls outside the
// watched mount — that's the right semantics for the public API but wrong
// here, where we want the literal path the entity record stores so we
// can ask "does THAT path still exist with the expected inode?" without
// dereferencing whatever symlink chain may have grown over it.
func (s *Service) claimedAbsPath(e *Entity) (string, error) {
	if e == nil {
		return "", ErrNotFound
	}
	if e.ParentID.IsZero() {
		return s.ResolveAbsPath(e.ID)
	}
	parentAbs, err := s.ResolveAbsPath(e.ParentID)
	if err != nil {
		return "", err
	}
	return filepath.Join(parentAbs, e.Name), nil
}

// parentIDForSync returns the ID of parentAbs, indexing it first when it is
// not already in the index.
//
// The parent has to be present in the INDEX, not merely carry an xattr. Those
// are two separate writes, and a request that claimed the parent's ID a moment
// ago has done the first but not yet the second -- two uploads creating
// sibling directories under a freshly created shared parent hit that window
// routinely. Trusting the xattr alone anchors the child to a parent whose
// entity does not exist yet, and VirtualPath then walks into a dead end and
// reports a directory that plainly exists as not found.
func (s *Service) parentIDForSync(parentAbs string) (FileID, error) {
	id, err := s.store.GetID(parentAbs)
	if err == nil {
		if _, getErr := s.idx.GetEntity(id); getErr == nil {
			return id, nil
		} else if !errors.Is(getErr, ErrNotFound) {
			return FileID{}, getErr
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return FileID{}, err
	}
	if err := s.syncSingle(parentAbs); err != nil {
		return FileID{}, err
	}
	return s.store.GetID(parentAbs)
}

// indexNewDirChain records a run of directories that were created together, in
// one write.
//
// syncSingle indexes one path per call and reaches ancestors by recursing, which
// costs a synced index write per level. For an upload committing into a tree that
// does not exist yet, that is the dominant cost of the whole commit: measured at
// roughly one 1.8 ms durable write per level, eight levels deep, against a 26 ms
// commit.
//
// Nothing about a fresh chain needs separate writes. The levels were created
// together and are only reachable through each other, so one atomic batch is
// both cheaper and a stronger guarantee than a chain that can be observed
// half-indexed.
//
// Directories only. Files carry S3 extension fields that syncSingle preserves by
// reading the entity it is replacing, and a freshly created path has none.
//
// A crash before the batch commits leaves the directories on disk and absent
// from the index, which is the state the index is designed to recover from: the
// next resolve indexes them, and a rescan rebuilds them from the filesystem.
func (s *Service) indexNewDirChain(absPaths []string) error {
	// Only a contiguous run can be chained, and the caller cannot promise one.
	// Concurrency punches holes in it: another request may create an intermediate
	// level between the caller's lstat and its mkdir, so that level is skipped
	// while levels above it were created. Chaining across such a gap would anchor
	// a directory to its grandparent and lose a path component -- which is a
	// corrupt index, not a slow one, so it is resolved here rather than trusted
	// to the caller.
	//
	// Levels dropped from the run are not lost. The parent resolution below
	// indexes whatever the first batched level hangs off, recursing upward as far
	// as it needs to.
	for i := len(absPaths) - 1; i > 0; i-- {
		if filepath.Dir(absPaths[i]) != absPaths[i-1] {
			absPaths = absPaths[i:]
			break
		}
	}
	if len(absPaths) == 0 {
		return nil
	}

	// The chain hangs off a parent that must already be in the index, which is
	// what parentIDForSync guarantees -- including indexing it first if some
	// other request only just claimed its ID.
	parentID, err := s.parentIDForSync(filepath.Dir(absPaths[0]))
	if err != nil {
		return err
	}

	type chainLevel struct {
		entity   Entity
		parentID FileID
		name     string
		entry    DirEntry
	}
	levels := make([]chainLevel, 0, len(absPaths))
	for _, absPath := range absPaths {
		info, err := os.Lstat(absPath)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return ErrInvalidArgument
		}
		id, err := s.resolveOrReissueID(absPath, info)
		if err != nil {
			return err
		}
		name := filepath.Base(absPath)
		levels = append(levels, chainLevel{
			entity:   buildEntityMetadata(id, parentID, name, absPath, info),
			parentID: parentID,
			name:     name,
			entry: DirEntry{
				ID:    id,
				Name:  name,
				IsDir: true,
				Size:  info.Size(),
				Mtime: info.ModTime().UnixMilli(),
			},
		})
		parentID = id
	}

	if err := s.idx.Batch(func(b Batch) error {
		for i := range levels {
			b.PutEntity(levels[i].entity)
			b.PutChild(levels[i].parentID, levels[i].name, levels[i].entry)
		}
		return nil
	}); err != nil {
		return err
	}

	// The new levels were not cached -- they did not exist -- but the listing of
	// the directory they were added to was.
	for i := range levels {
		s.invalidateCacheByID(levels[i].entity.ID)
	}
	if parentVP, err := s.VirtualPath(levels[0].parentID); err == nil {
		s.InvalidatePathCache(parentVP)
	}
	return nil
}

func (s *Service) syncSingle(absPath string) error {
	// Filegate-owned namespaces are reserved. Detector-driven
	// SyncAbsPath calls land here for any path the FS reports —
	// including internal blobs we own. Without this guard, a
	// freshly-written blob would get an entity record + child edge in
	// the index, exposing it through the public path API.
	if s.isPathInsideReservedNamespace(absPath) {
		return nil
	}
	if isFilegateInternalTempName(filepath.Base(absPath)) {
		return nil
	}
	// Lstat is sufficient for the symlink check AND the metadata path.
	// For non-symlinks Lstat == Stat, and we already reject symlinks
	// here — so the second Stat call would be a redundant syscall.
	info, err := os.Lstat(absPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrNotFound
		}
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return ErrForbidden
	}

	id, err := s.resolveOrReissueID(absPath, info)
	if err != nil {
		return err
	}

	parentAbs := filepath.Dir(absPath)
	parentID, err := s.parentIDForSync(parentAbs)
	if err != nil {
		return err
	}

	name := filepath.Base(absPath)
	entity := buildEntityMetadata(id, parentID, name, absPath, info)

	// Preserve S3-extension fields when the file looks unchanged
	// from what we already stored. The plan's "any non-S3 write
	// clears multipart_etag" rule targets genuine external
	// mutations — a detector poll re-scanning a file we just
	// wrote (via S3 PutObject / multipart Complete / CopyObject)
	// is NOT an external mutation, and clearing the S3 metadata
	// on every poll silently breaks the multipart-ETag contract.
	//
	// Cheap signal first: size + mtime + inode all match. If any
	// of those differ, the file is definitely different — clear
	// the S3 fields per plan §7.
	//
	// Caveat: rsync --inplace -t (and tools that explicitly
	// preserve mtime when rewriting bytes) can produce a same-
	// size, same-mtime, same-inode write with different content.
	// To catch that, when the cheap signal says "looks unchanged"
	// AND we have a stored ETagMD5, verify the live bytes hash
	// to the same value before preserving. Verification only
	// runs on a candidate match — typical idle polls of unchanged
	// files cost the hash; that's bounded by the poll interval.
	if existing, eErr := s.idx.GetEntity(id); eErr == nil && existing != nil && !existing.IsDir {
		if existing.Size == info.Size() &&
			existing.Mtime == info.ModTime().UnixMilli() &&
			existing.Inode == entity.Inode {
			preserve := true
			if existing.ETagMD5 != "" {
				live, hashErr := s.hashLocalFileHashes(absPath)
				if hashErr != nil || live.MD5Hex != existing.ETagMD5 {
					preserve = false
				}
			}
			if preserve {
				entity.ETagMD5 = existing.ETagMD5
				entity.SHA256 = existing.SHA256
				entity.MultipartETag = existing.MultipartETag
				entity.ContentType = existing.ContentType
				entity.ContentEncoding = existing.ContentEncoding
				entity.ContentDisposition = existing.ContentDisposition
				entity.S3UserMetadata = existing.S3UserMetadata
			}
		}
	}

	entry := DirEntry{
		ID:    id,
		Name:  name,
		IsDir: info.IsDir(),
		Size:  info.Size(),
		Mtime: info.ModTime().UnixMilli(),
	}

	if err := s.idx.Batch(func(b Batch) error {
		b.PutEntity(entity)
		b.PutChild(parentID, name, entry)
		return nil
	}); err != nil {
		return err
	}

	s.invalidateCacheByID(id)
	if parentVP, err := s.VirtualPath(parentID); err == nil {
		s.InvalidatePathCache(parentVP)
		// Also invalidate the file's own VP — InvalidatePathCache only
		// removes the exact key, and a re-issued ID at an existing path
		// would otherwise keep returning the stale ID via cache.
		s.InvalidatePathCache(parentVP + "/" + name)
	}
	// No event emission here — syncSingle is a low-level primitive that
	// cannot tell create from update from move. The public callers
	// (CreateChild, WriteContent, Transfer, SyncAbsPath, ...) publish the
	// semantically-correct event after invoking syncSingle.
	return nil
}

// syncSingleAfterLocalWrite runs syncSingle, then persists the caller-supplied
// content hashes on the entity and clears all
// S3-only metadata fields. Call this from REST/non-S3 write paths
// immediately after writeFileAtomic so the entity row carries an ETag
// and the S3-overwrite-clear-semantics from the plan are honoured.
//
// Cost: one extra Pebble Get + one extra batched Set vs plain
// syncSingle. The extra round-trip is acceptable for the consistency
// win (every local write produces an indexed ETag, and any prior
// multipart_etag from an earlier S3 multipart upload is dropped — the
// honest signal that the file changed via a non-S3 protocol).
func (s *Service) syncSingleAfterLocalWrite(absPath string, hashes ContentHashes) error {
	if err := s.syncSingle(absPath); err != nil {
		return err
	}
	id, err := s.store.GetID(absPath)
	if err != nil {
		return err
	}
	// Read OUTSIDE the batch: Index.Batch holds the index's read lock
	// for the whole closure, and GetEntity re-acquires it — a recursive
	// RLock that deadlocks once a writer (Close) is queued in between.
	entity, err := s.idx.GetEntity(id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// File was deleted between syncSingle and the follow-up
			// read. Harmless — nothing to update.
			return nil
		}
		return err
	}
	if hashes.MD5Hex != "" {
		entity.ETagMD5 = hashes.MD5Hex
	}
	if hashes.SHA256 != "" {
		entity.SHA256 = hashes.SHA256
	}
	// REST/non-S3 overwrite semantics: clear all S3-only fields.
	// See plan §7 rule 4. If a future S3 write wants to set them,
	// it will use a different domain entry-point that preserves
	// or sets explicitly.
	entity.MultipartETag = ""
	entity.ContentType = ""
	entity.ContentEncoding = ""
	entity.ContentDisposition = ""
	entity.S3UserMetadata = nil
	return s.idx.Batch(func(b Batch) error {
		b.PutEntity(*entity)
		return nil
	})
}

func (s *Service) mountForAbsPath(absPath string) (mountName, rel string, ok bool) {
	absPath = filepath.Clean(absPath)
	s.mu.RLock()
	defer s.mu.RUnlock()

	bestLen := -1
	bestName := ""
	bestBase := ""
	for name, base := range s.mountByName {
		if !isWithinBase(absPath, base) {
			continue
		}
		if len(base) > bestLen {
			bestLen = len(base)
			bestName = name
			bestBase = base
		}
	}
	if bestLen < 0 {
		return "", "", false
	}
	relative, err := filepath.Rel(bestBase, absPath)
	if err != nil {
		return "", "", false
	}
	if relative == "." {
		relative = ""
	}
	return bestName, relative, true
}

func (s *Service) virtualPathFromAbs(absPath string) (string, error) {
	mountName, rel, ok := s.mountForAbsPath(absPath)
	if !ok {
		return "", ErrForbidden
	}
	vp := "/" + mountName
	if rel != "" {
		vp += "/" + filepath.ToSlash(rel)
	}
	return vp, nil
}

func (s *Service) SyncAbsPath(absPath string) error {
	absPath = filepath.Clean(strings.TrimSpace(absPath))
	if absPath == "" {
		return ErrInvalidArgument
	}
	vp, err := s.virtualPathFromAbs(absPath)
	if err != nil {
		return err
	}
	parts := strings.Split(strings.TrimPrefix(vp, "/"), "/")
	if len(parts) <= 1 {
		return nil
	}

	// Decide create vs update by checking the index BEFORE syncing. If
	// the path already has an xattr ID and that ID has an index entity,
	// the sync will be an update. Otherwise — no xattr, no entity, or a
	// conflict-driven re-issue inside syncSingle — it counts as a fresh
	// entity from the index's point of view, so EventCreated.
	preExisted := false
	if preID, idErr := s.store.GetID(absPath); idErr == nil && !preID.IsZero() {
		if _, entityErr := s.idx.GetEntity(preID); entityErr == nil {
			preExisted = true
		}
	}

	if err := s.syncSingle(absPath); err != nil {
		return err
	}

	postID, err := s.store.GetID(absPath)
	if err != nil {
		// Sync succeeded but we can't read back the ID. Skip the event
		// rather than publish with a zero ID.
		return nil
	}
	eventType := EventCreated
	if preExisted {
		eventType = EventUpdated
	}
	s.bus.Publish(Event{Type: eventType, ID: postID, Path: absPath, At: time.Now()})
	return nil
}

func (s *Service) RemoveAbsPath(absPath string) error {
	absPath = filepath.Clean(strings.TrimSpace(absPath))
	if absPath == "" {
		return ErrInvalidArgument
	}
	vp, err := s.virtualPathFromAbs(absPath)
	if err != nil {
		if errors.Is(err, ErrForbidden) {
			return nil
		}
		return err
	}
	id, err := s.ResolvePath(vp)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	if s.isMountRoot(id) {
		return ErrForbidden
	}
	if err := s.deleteSubtree(id); err != nil {
		return err
	}
	// EventDeleted is published by deleteSubtree itself.
	return nil
}

func (s *Service) listAllChildren(parentID FileID) ([]DirEntry, error) {
	var cursor ChildCursor
	out := make([]DirEntry, 0, 32)
	for {
		chunk, err := s.idx.ListChildren(parentID, cursor, 1000)
		if err != nil {
			return nil, err
		}
		if len(chunk) == 0 {
			break
		}
		out = append(out, chunk...)
		if len(chunk) < 1000 {
			break
		}
		last := chunk[len(chunk)-1]
		cursor = ChildCursor{Name: last.Name, IsDir: last.IsDir}
	}
	return out, nil
}

// ReconcileDirectory enforces the invariant that Children[parent_id] equals
// readdir(parent_abs_path). It walks the directory on disk, walks the
// indexed children, and drops any indexed name that no longer exists. New
// names on disk are picked up through the normal syncSingle path triggered
// by the detector — ReconcileDirectory does not synthesize sync calls for
// them.
//
// This is the cheap correctness primitive that lets the detector consumer
// catch stale namespace edges left behind by external operations the inode
// stream alone cannot describe (hardlink unlink, in-subvol rename, etc.).
// Intended to run after a detector batch for every parent dir touched by
// an event; safe to call any time under load.
//
// Skipped silently when:
//   - parentAbsPath isn't inside any watched mount (e.g. /tmp leaks),
//   - the parent directory itself doesn't exist on disk,
//   - the parent isn't indexed (we have nothing to reconcile against).
func (s *Service) ReconcileDirectory(parentAbsPath string) error {
	parentAbsPath = filepath.Clean(strings.TrimSpace(parentAbsPath))
	if parentAbsPath == "" {
		return nil
	}
	// Only reconcile inside a mount.
	if _, _, ok := s.mountForAbsPath(parentAbsPath); !ok {
		return nil
	}
	parentID, err := s.store.GetID(parentAbsPath)
	if err != nil {
		// Parent not indexed — nothing to reconcile from this side. The
		// next sync of any child will also walk up and index the parent.
		return nil
	}
	diskEntries, err := s.store.ReadDir(parentAbsPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Parent went away while we were processing — let the next
			// detector batch handle it.
			return nil
		}
		return err
	}
	// Build a lookup of names that exist on disk. Symlinks are skipped to
	// match Filegate's overall symlink-rejection policy. Reserved
	// Filegate namespaces are filtered at the mount root; public
	// reconcile must not index them.
	mountRoot := s.isMountRootAbsPath(parentAbsPath)
	onDisk := make(map[string]struct{}, len(diskEntries))
	for _, e := range diskEntries {
		if e.Type()&os.ModeSymlink != 0 {
			continue
		}
		if isFilegateInternalTempName(e.Name()) {
			continue
		}
		if mountRoot && isFilegateReservedName(e.Name()) {
			continue
		}
		onDisk[e.Name()] = struct{}{}
	}

	// Two-way reconcile:
	//   1. Index entries with no on-disk counterpart -> stale, drop the
	//      child edge (not the entity — it may be referenced from another
	//      parent via a hardlink).
	//   2. On-disk entries with no index counterpart -> new, sync them.
	//      This catches operations the detector's inode stream misses
	//      (in-subvol hardlink rename, mkdir on btrfs without contents,
	//      etc.).
	indexed, err := s.listAllChildren(parentID)
	if err != nil {
		return err
	}
	indexedByName := make(map[string]struct{}, len(indexed))
	for _, c := range indexed {
		indexedByName[c.Name] = struct{}{}
	}
	stale := make([]DirEntry, 0)
	for _, child := range indexed {
		if _, ok := onDisk[child.Name]; ok {
			continue
		}
		stale = append(stale, child)
	}
	if len(stale) > 0 {
		if err := s.idx.Batch(func(b Batch) error {
			for _, c := range stale {
				b.DelChild(parentID, c.Name)
			}
			return nil
		}); err != nil {
			return err
		}
		parentVP, vpErr := s.VirtualPath(parentID)
		for _, c := range stale {
			s.invalidateCacheByID(c.ID)
			if vpErr == nil {
				s.InvalidatePathCache(parentVP + "/" + c.Name)
			}
			// Note: EventDeleted here describes a namespace-edge removal,
			// not necessarily the underlying entity going away. Subscribers
			// that want entity-lifecycle semantics need to GetEntity(id)
			// to confirm.
			s.bus.Publish(Event{Type: EventDeleted, ID: c.ID, Path: filepath.Join(parentAbsPath, c.Name), At: time.Now()})
		}
		if vpErr == nil {
			s.InvalidatePathCache(parentVP)
		}
	}

	// Add on-disk entries that the index doesn't know about. syncSingle
	// handles xattr conflict resolution and entity creation. We log and
	// continue on per-child errors so one broken file doesn't block the
	// rest of the directory. Each new child is emitted as EventCreated
	// (we know it's new because it's not in indexedByName).
	for _, e := range diskEntries {
		if e.Type()&os.ModeSymlink != 0 {
			continue
		}
		if _, ok := indexedByName[e.Name()]; ok {
			continue
		}
		childAbs := filepath.Join(parentAbsPath, e.Name())
		if syncErr := s.syncSingle(childAbs); syncErr != nil && !errors.Is(syncErr, ErrNotFound) && !errors.Is(syncErr, ErrForbidden) {
			// Per-child errors are logged, not fatal: an unstattable
			// child must not poison the whole directory sync. The next
			// ReconcileDirectory pass retries.
			log.Printf("[filegate] ReconcileDirectory: syncSingle(%q) failed: %v", childAbs, syncErr)
			continue
		}
		if newID, err := s.store.GetID(childAbs); err == nil {
			s.bus.Publish(Event{Type: EventCreated, ID: newID, Path: childAbs, At: time.Now()})
		}
	}
	return nil
}

func (s *Service) deleteSubtree(rootID FileID) error {
	if _, err := s.idx.GetEntity(rootID); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	// Capture the path BEFORE the entity is torn down so the EventDeleted
	// we publish at the end carries a meaningful Path field.
	rootAbsPath, _ := s.ResolveAbsPath(rootID)

	stack := []FileID{rootID}
	order := make([]Entity, 0, 64)
	seen := make(map[FileID]struct{}, 64)
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if _, exists := seen[cur]; exists {
			continue
		}
		seen[cur] = struct{}{}

		entity, err := s.idx.GetEntity(cur)
		if err != nil {
			continue
		}
		order = append(order, *entity)
		if !entity.IsDir {
			continue
		}
		children, err := s.listAllChildren(cur)
		if err != nil {
			return err
		}
		for _, child := range children {
			stack = append(stack, child.ID)
		}
	}

	if len(order) == 0 {
		return nil
	}
	rootPath := ""
	if vp, err := s.VirtualPath(rootID); err == nil {
		rootPath = normalizeCacheKey(vp)
	}
	if err := s.idx.Batch(func(b Batch) error {
		for i := len(order) - 1; i >= 0; i-- {
			e := order[i]
			if !e.ParentID.IsZero() {
				b.DelChild(e.ParentID, e.Name)
			}
			b.DelEntity(e.ID)
		}
		return nil
	}); err != nil {
		return err
	}

	if rootPath != "" {
		s.invalidateCachePrefix(rootPath)
		if parent := parentCacheKey(rootPath); parent != "" {
			s.cache.Remove(parent)
		}
	} else {
		s.purgePathCaches()
	}

	// Mark every deleted file's versions as orphans so the pruner can
	// apply the post-delete grace policy. The mark runs under each
	// descendant's per-file lock so a concurrent SnapshotVersion on
	// the same id can't slip a fresh DeletedAt=0 version in between
	// our list-versions and put-versions phases. Root is already
	// locked by the public caller (Delete) — locking it again here
	// would deadlock on the non-reentrant sync.Mutex.
	if s.VersioningEnabled() {
		now := time.Now().UnixMilli()
		for _, e := range order {
			if e.IsDir {
				continue
			}
			if e.ID != rootID {
				childMu := s.versionLocks.Acquire(e.ID)
				childMu.Lock()
				if _, err := s.idx.MarkVersionsDeleted(e.ID, now); err != nil {
					log.Printf("[filegate] versioning: orphan-mark %s failed: %v", e.ID, err)
				}
				childMu.Unlock()
				continue
			}
			if _, err := s.idx.MarkVersionsDeleted(e.ID, now); err != nil {
				log.Printf("[filegate] versioning: orphan-mark %s failed: %v", e.ID, err)
			}
		}
	}

	// Single bulk EventDeleted for the subtree root. Callers (Delete,
	// RemoveAbsPath, Transfer overwrite) used to publish this themselves
	// — that's now centralised here so no caller can forget.
	s.bus.Publish(Event{Type: EventDeleted, ID: rootID, Path: rootAbsPath, At: time.Now()})
	return nil
}

func (s *Service) Rescan() error {
	s.rescanMu.Lock()
	defer s.rescanMu.Unlock()
	return s.rescanWithScope(nil)
}

func (s *Service) RescanMount(absPath string) error {
	absPath = filepath.Clean(strings.TrimSpace(absPath))
	if absPath == "" {
		return ErrInvalidArgument
	}
	mountName, _, ok := s.mountForAbsPath(absPath)
	if !ok {
		return nil
	}
	target := map[string]struct{}{mountName: {}}
	s.rescanMu.Lock()
	defer s.rescanMu.Unlock()
	return s.rescanWithScope(target)
}

func (s *Service) resolveRootID(id FileID, memo map[FileID]FileID) (FileID, bool, error) {
	var zero FileID
	if id.IsZero() {
		return zero, false, nil
	}
	if memo == nil {
		memo = make(map[FileID]FileID, 64)
	}
	if rootID, ok := memo[id]; ok {
		return rootID, !rootID.IsZero(), nil
	}

	current := id
	chain := make([]FileID, 0, 8)
	seen := make(map[FileID]struct{}, 8)
	for {
		if rootID, ok := memo[current]; ok {
			for _, n := range chain {
				memo[n] = rootID
			}
			return rootID, !rootID.IsZero(), nil
		}
		if _, exists := seen[current]; exists {
			for _, n := range chain {
				memo[n] = zero
			}
			return zero, false, nil
		}
		seen[current] = struct{}{}
		chain = append(chain, current)

		entity, err := s.idx.GetEntity(current)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				for _, n := range chain {
					memo[n] = zero
				}
				return zero, false, nil
			}
			return zero, false, err
		}
		if entity.ParentID.IsZero() {
			for _, n := range chain {
				memo[n] = entity.ID
			}
			return entity.ID, true, nil
		}
		current = entity.ParentID
	}
}

func (s *Service) rescanWithScope(targetMounts map[string]struct{}) error {
	type scanned struct {
		entity Entity
		entry  DirEntry
	}

	seen := make(map[FileID]struct{}, 1024)
	collected := make([]scanned, 0, 1024)
	targetRootIDs := make(map[FileID]struct{}, len(s.mountIDByName))
	// dirAbsPathsVisited captures every directory the walk descended into,
	// in walk order. Used at the end to fire ReconcileDirectory on each so
	// stale child edges (which the entity-level prune misses) get cleaned.
	dirAbsPathsVisited := make([]string, 0, 64)

	for _, mountName := range s.mountNames {
		if targetMounts != nil {
			if _, ok := targetMounts[mountName]; !ok {
				continue
			}
		}
		basePath := s.mountByName[mountName]
		mountID := s.mountIDByName[mountName]
		targetRootIDs[mountID] = struct{}{}

		info, err := s.store.Stat(basePath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return err
		}
		rootEntity := buildEntityMetadata(mountID, FileID{}, mountName, basePath, info)
		seen[mountID] = struct{}{}
		collected = append(collected, scanned{entity: rootEntity})
		// Mount root is itself a directory the walk just visited.
		dirAbsPathsVisited = append(dirAbsPathsVisited, basePath)

		pathToID := map[string]FileID{basePath: mountID}
		err = filepath.WalkDir(basePath, func(current string, d os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return nil
			}
			if current == basePath {
				return nil
			}
			// Filegate-owned namespaces are reserved at the mount root.
			// Exposing them through rescan would let users list/delete
			// internal blobs through the public path API. SkipDir
			// prunes the whole subtree.
			if d.IsDir() {
				for _, name := range []string{versionsDirName, uploadsDirName} {
					if current == filepath.Join(basePath, name) {
						return filepath.SkipDir
					}
				}
			}
			if isFilegateInternalTempName(d.Name()) {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if d.Type()&os.ModeSymlink != 0 {
				return nil
			}
			parent := filepath.Dir(current)
			parentID, ok := pathToID[parent]
			if !ok {
				return nil
			}

			info, err := d.Info()
			if err != nil {
				return nil
			}
			// Use resolveOrReissueID so the xattr-conflict path applies
			// during a Rescan too — without this, walking a directory
			// containing an original AND its `cp -a` copy would let both
			// paths claim the same stable ID and the second PutEntity
			// would silently overwrite the first.
			id, err := s.resolveOrReissueID(current, info)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					return nil
				}
				return err
			}
			seen[id] = struct{}{}
			pathToID[current] = id
			if d.IsDir() {
				dirAbsPathsVisited = append(dirAbsPathsVisited, current)
			}
			entity := buildEntityMetadata(id, parentID, d.Name(), current, info)
			// Rescan is the authoritative ETag-population path for any
			// file whose ETag isn't already correct (legacy, externally
			// added, or rebuilt index). Operators run rescan when they
			// want eager population — accept the extra read pass per
			// regular file.
			//
			// We also prefer an existing index ETag when the file hasn't
			// changed since it was last indexed. This skips the hash for
			// the common case where rescan is run repeatedly against an
			// unchanged dataset.
			if !d.IsDir() && info.Mode().IsRegular() {
				prior, _ := s.idx.GetEntity(id)
				if prior != nil && prior.ETagMD5 != "" && prior.Size == info.Size() && prior.Mtime == info.ModTime().UnixMilli() {
					preserveS3Metadata(&entity, prior)
				} else {
					hashes, hashErr := hashFileContent(current, info)
					if hashErr != nil {
						log.Printf("[filegate] rescan: hash failed for %s: %v", current, hashErr)
					} else if hashes.MD5Hex != "" {
						entity.ETagMD5 = hashes.MD5Hex
						entity.SHA256 = hashes.SHA256
					}
					// Preserve S3-only fields if any prior row exists,
					// even when we recomputed the ETag (the S3-set
					// fields are independent of body bytes).
					if prior != nil {
						entity.MultipartETag = prior.MultipartETag
						entity.ContentType = prior.ContentType
						entity.ContentEncoding = prior.ContentEncoding
						entity.ContentDisposition = prior.ContentDisposition
						entity.S3UserMetadata = prior.S3UserMetadata
					}
				}
			}
			collected = append(collected, scanned{
				entity: entity,
				entry: DirEntry{
					ID:    id,
					Name:  d.Name(),
					IsDir: d.IsDir(),
					Size:  info.Size(),
					Mtime: info.ModTime().UnixMilli(),
				},
			})
			return nil
		})
		if err != nil {
			return err
		}
	}

	if err := s.idx.Batch(func(b Batch) error {
		for _, item := range collected {
			b.PutEntity(item.entity)
			if !item.entity.ParentID.IsZero() {
				b.PutChild(item.entity.ParentID, item.entity.Name, item.entry)
			}
		}
		return nil
	}); err != nil {
		return err
	}

	type staleRef struct {
		id        FileID
		parentID  FileID
		name      string
		isDir     bool
		flatMount string // empty for dirs / mount roots / unresolvable
		flatRel   string
	}
	rootMemo := make(map[FileID]FileID, 4096)
	stale := make([]staleRef, 0, 1024)
	if err := s.idx.ForEachEntity(func(e Entity) error {
		if _, ok := seen[e.ID]; ok {
			return nil
		}
		if targetMounts != nil {
			rootID, ok, err := s.resolveRootID(e.ID, rootMemo)
			if err != nil {
				return err
			}
			if !ok {
				return nil
			}
			if _, isTarget := targetRootIDs[rootID]; !isTarget {
				return nil
			}
		}
		ref := staleRef{
			id:       e.ID,
			parentID: e.ParentID,
			name:     e.Name,
			isDir:    e.IsDir,
		}
		// Pre-compute the file's flat-key path NOW, while the entire
		// stale subtree is still intact in the index. The batched
		// delete pass below may delete a parent before its child, in
		// which case derivePath in DelEntity's auto-maintenance would
		// fail to resolve. The explicit DelFlatKey call uses these
		// pre-computed values and works regardless of delete order.
		if !e.IsDir {
			if vp, err := s.VirtualPath(e.ID); err == nil {
				if m, r, ok := splitVirtualPath(vp); ok {
					ref.flatMount = m
					ref.flatRel = r
				}
			}
		}
		stale = append(stale, ref)
		return nil
	}); err != nil {
		return err
	}
	for start := 0; start < len(stale); start += 4096 {
		end := start + 4096
		if end > len(stale) {
			end = len(stale)
		}
		chunk := stale[start:end]
		if err := s.idx.Batch(func(b Batch) error {
			for _, e := range chunk {
				if !e.parentID.IsZero() {
					b.DelChild(e.parentID, e.name)
				}
				if e.flatMount != "" {
					b.DelFlatKey(e.flatMount, e.flatRel)
				}
				b.DelEntity(e.id)
			}
			return nil
		}); err != nil {
			return err
		}
	}

	// Final dir-sync pass: reconcile each visited directory against its
	// on-disk readdir. The entity-level prune above only drops entities
	// not seen in the FS walk, but stale child EDGES that point at a
	// still-living shared entity (hardlink alias unlinked while the
	// gateway was offline, snapshot file removed but the originals
	// survive, etc.) are invisible to the entity prune. ReconcileDirectory
	// catches those.
	for _, parentAbs := range dirAbsPathsVisited {
		if err := s.ReconcileDirectory(parentAbs); err != nil {
			log.Printf("[filegate] Rescan: ReconcileDirectory(%q) failed: %v", parentAbs, err)
		}
	}

	// Flat-key sweep: drop any flat-key entry whose referenced fileID
	// is gone (orphaned by a previous deletion that had a broken
	// parent chain) or whose path no longer matches the entity's
	// current location (left behind by a rename whose ReKey didn't
	// reach this entry — e.g. a detector race, an old format-bump
	// rebuild, or a hard-link transition that bypassed our index
	// maintenance). Cost is O(num flat-keys) per swept mount, which
	// rescan callers already accept for the FS walk.
	if err := s.sweepStaleFlatKeysForMounts(targetMounts); err != nil {
		log.Printf("[filegate] Rescan: flat-key sweep failed: %v", err)
	}

	s.purgePathCaches()
	eventPath := "*"
	if len(targetMounts) == 1 {
		for mountName := range targetMounts {
			eventPath = "/" + mountName
		}
	}
	s.bus.Publish(Event{Type: EventScanned, Path: eventPath, At: time.Now()})
	return nil
}

// sweepStaleFlatKeysForMounts walks the flat-key index for each
// in-scope mount and drops entries that no longer correspond to a
// live file at the listed path. Two staleness conditions:
//
//  1. The referenced FileID has no entity row anymore. The orphan
//     came from a delete that couldn't derive its path (parent chain
//     broken at delete time), or from an old rebuild that didn't yet
//     populate flat-keys.
//  2. The entity exists but its current VirtualPath disagrees with
//     the flat-key entry's (mount, relPath). The orphan came from a
//     rename whose ReKey missed this entry — caused by a same-id
//     directory rename that fell through detector/race seams.
//
// Sweep is run from Rescan after the entity-level prune. Each iterator callback
// only copies one bounded page. Entity/path lookups and deletes happen after
// IterateFlatKeys releases the index read lock, so this sweep cannot deadlock
// Close by re-entering the index while a writer is waiting.
func (s *Service) sweepStaleFlatKeysForMounts(targetMounts map[string]struct{}) error {
	const pageSize = 4096
	type entry struct {
		rel string
		id  FileID
	}
	type stale struct{ mount, rel string }
	mounts := s.mountNames
	for _, mountName := range mounts {
		if targetMounts != nil {
			if _, ok := targetMounts[mountName]; !ok {
				continue
			}
		}
		after := ""
		for {
			page := make([]entry, 0, pageSize)
			err := s.idx.IterateFlatKeys(mountName, "", after, pageSize, func(rel string, id FileID) (bool, error) {
				page = append(page, entry{rel: rel, id: id})
				return true, nil
			})
			if err != nil {
				return err
			}
			if len(page) == 0 {
				break
			}

			orphans := make([]stale, 0, len(page))
			for _, indexed := range page {
				entity, err := s.idx.GetEntity(indexed.id)
				if err != nil && !errors.Is(err, ErrNotFound) {
					return err
				}
				if entity == nil {
					orphans = append(orphans, stale{mountName, indexed.rel})
					continue
				}
				// Path drift: derive the entity's current path and compare to
				// this flat-key entry. If it differs, the flat-key is stale.
				vp, vpErr := s.VirtualPath(indexed.id)
				if vpErr != nil {
					// Can't resolve current path → conservative: leave the entry,
					// don't risk deleting a briefly unresolvable live key.
					continue
				}
				gotMount, gotRel, ok := splitVirtualPath(vp)
				if !ok || gotMount != mountName || gotRel != indexed.rel {
					orphans = append(orphans, stale{mountName, indexed.rel})
				}
			}

			// A page is also the maximum delete batch size.
			if len(orphans) > 0 {
				if err := s.idx.Batch(func(b Batch) error {
					for _, o := range orphans {
						b.DelFlatKey(o.mount, o.rel)
					}
					return nil
				}); err != nil {
					return err
				}
			}

			after = page[len(page)-1].rel
			if len(page) < pageSize {
				break
			}
		}
	}
	return nil
}

func (s *Service) Stats() (*ServiceStats, error) {
	s.mu.RLock()
	mountNames := append([]string(nil), s.mountNames...)
	mountIDByName := make(map[string]FileID, len(s.mountIDByName))
	for k, v := range s.mountIDByName {
		mountIDByName[k] = v
	}
	pathCacheEntries := s.cache.Len()
	pathCacheCapacity := s.pathCacheSize
	s.mu.RUnlock()

	mountByID := make(map[FileID]*StatsMount, len(mountNames))
	for _, name := range mountNames {
		mountID := mountIDByName[name]
		mountByID[mountID] = &StatsMount{
			ID:   mountID,
			Name: name,
			Path: "/" + name,
		}
	}
	rootMemo := make(map[FileID]FileID, 4096)
	totalEntities := 0
	totalFiles := 0
	totalDirs := 0
	if err := s.idx.ForEachEntity(func(e Entity) error {
		totalEntities++
		if e.IsDir {
			totalDirs++
		} else {
			totalFiles++
		}

		if e.ParentID.IsZero() {
			return nil
		}
		rootID, ok, err := s.resolveRootID(e.ID, rootMemo)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		mount := mountByID[rootID]
		if mount == nil {
			return nil
		}
		if e.IsDir {
			mount.Dirs++
		} else {
			mount.Files++
		}
		return nil
	}); err != nil {
		return nil, err
	}

	mounts := make([]StatsMount, 0, len(mountNames))
	for _, name := range mountNames {
		mountID := mountIDByName[name]
		if mount, ok := mountByID[mountID]; ok {
			mounts = append(mounts, *mount)
		}
	}

	util := 0.0
	if pathCacheCapacity > 0 {
		util = float64(pathCacheEntries) / float64(pathCacheCapacity)
	}

	return &ServiceStats{
		GeneratedAt:        time.Now().UnixMilli(),
		TotalEntities:      totalEntities,
		TotalFiles:         totalFiles,
		TotalDirs:          totalDirs,
		PathCacheEntries:   pathCacheEntries,
		PathCacheCapacity:  pathCacheCapacity,
		PathCacheUtilRatio: util,
		Mounts:             mounts,
	}, nil
}

// InvalidatePathCache drops the path→id mapping for one exact path.
// It does NOT touch the id→path cache: entries there only go stale
// when an id's path changes, and every mutator handles that through
// invalidateCacheByID / invalidateCachePrefix on the affected ids. A
// dead id's leftover mapping is unreachable (entity lookups fail
// first) and ages out of the LRU.
func (s *Service) InvalidatePathCache(path string) {
	key := normalizeCacheKey(path)
	if key == "" {
		return
	}
	s.cache.Remove(key)
}

func (s *Service) purgePathCaches() {
	s.cache.Purge()
	s.idPathCache.Purge()
}

func normalizeCacheKey(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || v == "/" {
		return ""
	}
	v = strings.TrimPrefix(v, "/")
	v = strings.Trim(strings.ReplaceAll(v, "\\", "/"), "/")
	v = path.Clean(v)
	if v == "." || v == "" {
		return ""
	}
	return v
}

func parentCacheKey(v string) string {
	if v == "" {
		return ""
	}
	p := path.Dir(v)
	if p == "." || p == "/" {
		return ""
	}
	return p
}

// invalidateCachePrefix drops every cached mapping at or under
// pathPrefix from BOTH caches. The id→path side is scanned the same
// way the path→id side always was — same O(cache size) cost, but hot
// entries for unrelated paths survive instead of being purged on
// every mutation.
func (s *Service) invalidateCachePrefix(pathPrefix string) {
	prefix := normalizeCacheKey(pathPrefix)
	if prefix == "" {
		s.purgePathCaches()
		return
	}
	for _, key := range s.cache.Keys() {
		if key == prefix || strings.HasPrefix(key, prefix+"/") {
			s.cache.Remove(key)
		}
	}
	for _, id := range s.idPathCache.Keys() {
		vp, ok := s.idPathCache.Peek(id)
		if !ok {
			continue
		}
		key := normalizeCacheKey(vp)
		if key == prefix || strings.HasPrefix(key, prefix+"/") {
			s.idPathCache.Remove(id)
		}
	}
}

func (s *Service) invalidateCacheByID(id FileID) {
	s.idPathCache.Remove(id)
	vp, err := s.VirtualPath(id)
	if err != nil {
		s.purgePathCaches()
		return
	}
	key := normalizeCacheKey(vp)
	if key == "" {
		return
	}
	s.invalidateCachePrefix(key)
	parent := parentCacheKey(key)
	if parent != "" {
		s.cache.Remove(parent)
	}
}

// PathCacheStats reports occupancy and cumulative effectiveness of the virtual
// path cache. Hits and misses are cumulative since process start, so a caller
// wanting a rate should sample twice.
func (s *Service) PathCacheStats() (entries, capacity int, hits, misses uint64) {
	s.mu.RLock()
	entries = s.cache.Len()
	capacity = s.pathCacheSize
	s.mu.RUnlock()
	return entries, capacity, s.pathCacheHits.Load(), s.pathCacheMisses.Load()
}
