package httpadapter

import (
	"net/http"
	"slices"

	api "github.com/k2b-dev/filegate/v7/api/v1"
	"github.com/k2b-dev/filegate/v7/domain"
)

// Public identity projection never modifies the durable nodes used by history
// and completion receipts. Transfers use the destination node's root policy.
func (h *Handler) publicNode(node domain.Node) domain.Node {
	root := h.roots[node.Root]
	if root == nil || !root.StableIDs() {
		node.ID = ""
	}
	return node
}

func (h *Handler) publicResult(root *domain.Root, value any) any {
	switch v := value.(type) {
	case domain.Node:
		return h.publicNode(v)
	case domain.Page:
		v.Items = slices.Clone(v.Items)
		for i := range v.Items {
			v.Items[i] = h.publicNode(v.Items[i])
		}
		return v
	case domain.Version:
		if root == nil || !root.StableIDs() {
			v.FileID = ""
		}
		return v
	case []domain.Version:
		if root == nil || !root.StableIDs() {
			v = slices.Clone(v)
			for i := range v {
				v[i].FileID = ""
			}
		}
		return v
	case domain.Session:
		return h.publicSessionResult(v)
	case api.SessionCreated:
		v.Session = h.publicSessionResult(v.Session)
		return v
	case domain.TransferResult:
		if v.Node != nil {
			node := h.publicNode(*v.Node)
			v.Node = &node
		}
		return v
	default:
		return value
	}
}

func (h *Handler) publicSessionResult(session domain.Session) domain.Session {
	if session.Result != nil {
		node := h.publicNode(*session.Result)
		session.Result = &node
	}
	return session
}

func (h *Handler) sendRoot(w http.ResponseWriter, root *domain.Root, status int, value any) {
	send(w, status, h.publicResult(root, value))
}
