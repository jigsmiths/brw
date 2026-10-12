package uninstall

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// caskDependencies maps each formula an installed cask needs to the casks that
// need it, going by the runtime dependencies in the cask receipts. A cask whose
// receipt cannot be read is an error, so nothing is removed on a guess.
func caskDependencies(prefix string) (map[string][]string, error) {
	entries, err := os.ReadDir(filepath.Join(prefix, "Caskroom"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for _, e := range entries {
		token := e.Name()
		meta := filepath.Join(prefix, "Caskroom", token, ".metadata")
		if strings.HasPrefix(token, ".") {
			continue
		}
		if _, err := os.Stat(meta); errors.Is(err, fs.ErrNotExist) {
			continue // never installed, or a dangling link
		}
		b, err := os.ReadFile(filepath.Join(meta, "INSTALL_RECEIPT.json"))
		if err != nil {
			return nil, fmt.Errorf("cask %s: %w", token, err)
		}
		var r struct {
			RuntimeDependencies *struct {
				Formula []struct {
					FullName string `json:"full_name"`
				} `json:"formula"`
			} `json:"runtime_dependencies"`
		}
		if err := json.Unmarshal(b, &r); err != nil {
			return nil, fmt.Errorf("cask %s: %w", token, err)
		}
		if r.RuntimeDependencies == nil {
			continue
		}
		for _, d := range r.RuntimeDependencies.Formula {
			name := strings.TrimPrefix(d.FullName, "homebrew/core/")
			if !slices.Contains(out[name], token) {
				out[name] = append(out[name], token)
			}
		}
	}
	return out, nil
}
