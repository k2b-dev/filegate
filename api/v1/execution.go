package apiv1

import (
	"bytes"
	"encoding/json"

	"github.com/k2b-dev/filegate/v6/domain"
)

// ExecutionContext explicitly selects the destination filesystem actor.
// Omitting TargetExecution inherits the source actor; service never means a
// retry with elevated permissions after a Unix actor fails.
type ExecutionContext struct {
	Mode     string             `json:"mode"`
	Identity *ExecutionIdentity `json:"identity,omitempty"`
}

// UnmarshalJSON requires both numeric IDs, including an explicit zero GID.
// Unknown fields remain invalid inside this discriminated request object.
func (e *ExecutionContext) UnmarshalJSON(data []byte) error {
	var wire struct {
		Mode     string          `json:"mode"`
		Identity json.RawMessage `json:"identity"`
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&wire); err != nil {
		return domain.ErrInvalid
	}
	switch wire.Mode {
	case "service":
		if len(wire.Identity) != 0 {
			return domain.ErrInvalid
		}
		*e = ExecutionContext{Mode: "service"}
		return nil
	case "unix":
		var identity struct {
			UID    *uint32  `json:"uid"`
			GID    *uint32  `json:"gid"`
			Groups []uint32 `json:"groups"`
		}
		d = json.NewDecoder(bytes.NewReader(wire.Identity))
		d.DisallowUnknownFields()
		if err := d.Decode(&identity); err != nil || identity.UID == nil || identity.GID == nil {
			return domain.ErrInvalid
		}
		normalized, err := domain.NormalizeExecution(&ExecutionIdentity{UID: *identity.UID, GID: *identity.GID, Groups: identity.Groups})
		if err != nil {
			return err
		}
		*e = ExecutionContext{Mode: "unix", Identity: normalized}
		return nil
	default:
		return domain.ErrInvalid
	}
}
