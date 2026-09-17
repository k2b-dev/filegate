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
type WriteOptions = domain.WriteOptions
type Ownership = domain.Ownership
type ACLScope = domain.ACLScope
type ACLPermissions = domain.ACLPermissions
type ACLTag = domain.ACLTag
type ACLEntry = domain.ACLEntry
type ACL = domain.ACL

const (
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
type SessionRequest struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
	WriteOptions
}
type SessionCreated struct {
	Session
	URL string `json:"url"`
}
type MkdirRequest struct {
	Path      string     `json:"path"`
	Ownership *Ownership `json:"ownership,omitempty"`
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
