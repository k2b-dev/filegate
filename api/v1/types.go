// Package apiv1 defines the root-scoped Filegate wire contract.
package apiv1

import (
	"github.com/k2b-dev/filegate/v4/domain"
	"time"
)

type Node = domain.Node
type Page = domain.Page
type RootInfo = domain.RootInfo
type Version = domain.Version
type Session = domain.Session
type SessionStatus struct {
	ID          string         `json:"id"`
	Root        string         `json:"root"`
	Size        int64          `json:"size"`
	ChunkSize   int64          `json:"chunkSize"`
	Expires     time.Time      `json:"expires"`
	State       SessionState   `json:"state"`
	Segments    map[int]string `json:"segments"`
	Received    int64          `json:"received"`
	TerminalAt  *time.Time     `json:"terminalAt,omitempty"`
	RetainUntil *time.Time     `json:"retainUntil,omitempty"`
}
type SessionState = domain.SessionState
type DirectoryOptions = domain.DirectoryOptions
type DirectoryACLs = domain.DirectoryACLs
type WriteOptions = domain.WriteOptions
type Ownership = domain.Ownership
type ACLScope = domain.ACLScope
type ACLPermissions = domain.ACLPermissions
type ACLTag = domain.ACLTag
type ACLEntry = domain.ACLEntry
type ACL = domain.ACL

const (
	SessionOpen      = domain.SessionOpen
	SessionCommitted = domain.SessionCommitted
	SessionAborted   = domain.SessionAborted
	SessionExpired   = domain.SessionExpired

	AccessACL      = domain.AccessACL
	DefaultACL     = domain.DefaultACL
	ACLOwner       = domain.ACLOwner
	ACLUser        = domain.ACLUser
	ACLOwningGroup = domain.ACLOwningGroup
	ACLGroup       = domain.ACLGroup
	ACLMask        = domain.ACLMask
	ACLOther       = domain.ACLOther
)

type Metadata = domain.Metadata
type Stats = domain.Stats
type Error struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}
type DirectRequest struct {
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	ExpiresIn int    `json:"expiresIn,omitempty"`
	WriteOptions
}
type DirectURL struct {
	URL     string    `json:"url"`
	Method  string    `json:"method"`
	Expires time.Time `json:"expires"`
}
type SessionLeaseRequest struct {
	ExpiresIn  int  `json:"expiresIn,omitempty"`
	AllowAbort bool `json:"allowAbort,omitempty"`
}
type SessionLease struct {
	URL        string    `json:"url"`
	Expires    time.Time `json:"expires"`
	Operations []string  `json:"operations"`
}
type SessionRequest struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
	WriteOptions
	SessionLeaseRequest
}
type SessionCreated struct {
	Session Session      `json:"session"`
	Lease   SessionLease `json:"lease"`
}
type MkdirRequest struct {
	Path string `json:"path"`
	DirectoryOptions
}
type TransferRequest struct {
	Path       string `json:"path"`
	TargetRoot string `json:"targetRoot"`
	TargetPath string `json:"targetPath"`
	Move       bool   `json:"move"`
	WriteOptions
}
type VersionRequest struct {
	Pinned   bool     `json:"pinned"`
	Metadata Metadata `json:"metadata,omitempty"`
}
type System struct {
	Version          string    `json:"version"`
	Started          time.Time `json:"started"`
	UptimeSeconds    int64     `json:"uptimeSeconds"`
	Ready            bool      `json:"ready"`
	MaintenanceError string    `json:"maintenanceError,omitempty"`
}

type ArchiveItem struct {
	Root        string `json:"root"`
	Path        string `json:"path"`
	ArchivePath string `json:"archivePath"`
}
type ArchiveRequest struct {
	Items     []ArchiveItem `json:"items"`
	ExpiresIn int           `json:"expiresIn,omitempty"`
}
type ArchiveLease struct {
	URL      string    `json:"url"`
	Method   string    `json:"method"`
	Expires  time.Time `json:"expires"`
	Manifest string    `json:"manifest"`
}
