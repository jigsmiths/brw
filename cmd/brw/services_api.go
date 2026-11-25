package main

import (
	"context"
	"encoding/json"

	"github.com/jigsmiths/brw/internal/rb"
	"github.com/jigsmiths/brw/internal/services"
)

// apiTarget is a formula that is not installed, with the service block the API lists for it.
// It is not ok when the API does not know the formula.
func (e *env) apiTarget(prefix, name string) (t serviceTarget, ok bool) {
	t = serviceTarget{name: name}
	idx, client, err := e.indexAndClient(context.Background(), prefix)
	if err != nil {
		return t, false
	}
	f, ok := idx.Lookup(name)
	if !ok {
		return t, false
	}
	raw, err := client.Raw(f.Name)
	if err != nil {
		return t, true
	}
	var entry struct {
		Service json.RawMessage `json:"service"`
	}
	if json.Unmarshal(raw, &entry) != nil {
		return t, true
	}
	block, err := rb.ServiceFromAPI(entry.Service, prefix)
	if err != nil {
		return t, true
	}
	t.block, t.requiresRoot = block, block.RequiresRoot
	if block.MacName != "" && block.MacName != services.Names(name)[0] {
		t.explicit = block.MacName
	}
	return t, true
}
