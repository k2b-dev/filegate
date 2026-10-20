package domain

import (
	"encoding/json"
	"unicode/utf8"
)

// JSON strings replace invalid UTF-8. Durable records without a path-bearing
// key retain the original bytes separately; existing UTF-8 records keep their
// encoding. Node's public JSON representation remains unchanged.
func pathBytes(p string) []byte {
	if utf8.ValidString(p) {
		return nil
	}
	return []byte(p)
}

type claimJSON claim

func (c claim) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		claimJSON
		PathBytes []byte `json:",omitempty"`
	}{claimJSON(c), pathBytes(c.Path)})
}
func (c *claim) UnmarshalJSON(b []byte) error {
	var stored struct {
		claimJSON
		PathBytes []byte
	}
	if err := json.Unmarshal(b, &stored); err != nil {
		return err
	}
	*c = claim(stored.claimJSON)
	if stored.PathBytes != nil {
		c.Path = string(stored.PathBytes)
	}
	return nil
}

type mutationJSON mutation

func (m mutation) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		mutationJSON
		FromBytes []byte `json:",omitempty"`
		ToBytes   []byte `json:",omitempty"`
	}{mutationJSON(m), pathBytes(m.From), pathBytes(m.To)})
}
func (m *mutation) UnmarshalJSON(b []byte) error {
	var stored struct {
		mutationJSON
		FromBytes []byte
		ToBytes   []byte
	}
	if err := json.Unmarshal(b, &stored); err != nil {
		return err
	}
	*m = mutation(stored.mutationJSON)
	if stored.FromBytes != nil {
		m.From = string(stored.FromBytes)
	}
	if stored.ToBytes != nil {
		m.To = string(stored.ToBytes)
	}
	return nil
}

func (p *publication) UnmarshalJSON(b []byte) error {
	type publicationJSON publication
	var stored publicationJSON
	if err := json.Unmarshal(b, &stored); err != nil {
		return err
	}
	*p = publication(stored)
	// The inode claim and publication always address the same target. Its
	// lossless path also restores the embedded Node before deriving index keys.
	p.Path = p.Claim.Path
	p.Node.Path = p.Path
	return nil
}

type transferReceiptJSON transferReceipt

func (t transferReceipt) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		transferReceiptJSON
		SourcePathBytes []byte `json:",omitempty"`
		TargetPathBytes []byte `json:",omitempty"`
	}{transferReceiptJSON(t), pathBytes(t.SourcePath), pathBytes(t.TargetPath)})
}
func (t *transferReceipt) UnmarshalJSON(b []byte) error {
	var stored struct {
		transferReceiptJSON
		SourcePathBytes []byte
		TargetPathBytes []byte
	}
	if err := json.Unmarshal(b, &stored); err != nil {
		return err
	}
	*t = transferReceipt(stored.transferReceiptJSON)
	if stored.SourcePathBytes != nil {
		t.SourcePath = string(stored.SourcePathBytes)
	}
	if stored.TargetPathBytes != nil {
		t.TargetPath = string(stored.TargetPathBytes)
	}
	return nil
}

// Session is also a wire type. Only its private storage wrapper adds raw bytes.
func (s sessionReceipt) MarshalJSON() ([]byte, error) {
	type receiptJSON sessionReceipt
	return json.Marshal(struct {
		receiptJSON
		PathBytes []byte `json:"pathBytes,omitempty"`
	}{receiptJSON(s), pathBytes(s.Path)})
}

func (s *sessionReceipt) UnmarshalJSON(b []byte) error {
	type receiptJSON sessionReceipt
	var stored struct {
		receiptJSON
		PathBytes []byte `json:"pathBytes"`
	}
	if err := json.Unmarshal(b, &stored); err != nil {
		return err
	}
	*s = sessionReceipt(stored.receiptJSON)
	if stored.PathBytes != nil {
		s.Path = string(stored.PathBytes)
	}
	return nil
}
