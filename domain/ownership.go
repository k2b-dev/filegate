package domain

import (
	"os"
)

func (r *Root) SetOwnership(p string, o *Ownership) (Node, error) {
	p, e := CleanPath(p)
	if e != nil {
		return Node{}, e
	}
	if e = validateOwnership(o); e != nil {
		return Node{}, e
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if e = r.guard(); e != nil {
		return Node{}, e
	}
	f, e := r.Files.Open(p, os.O_RDONLY, 0)
	if e != nil {
		return Node{}, e
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil {
		return Node{}, e
	}
	if e = applyOwner(f, o, st.IsDir()); e != nil {
		return Node{}, e
	}
	if e = f.Sync(); e != nil {
		return Node{}, e
	}
	n, e := r.node(p, true)
	if e == nil {
		e = r.indexNode(n)
	}
	return n, e
}
