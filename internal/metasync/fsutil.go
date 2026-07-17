package metasync

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// small filesystem helpers shared by snapshot and profile storage.

func mkdirAll(dir string) error { return os.MkdirAll(dir, 0o755) }

func joinPath(parts ...string) string { return filepath.Clean(filepath.Join(parts...)) }

func writeJSONFile(dir, name string, v any) error {
	b, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, name), append(b, '\n'), 0o644)
}

func readJSONFile(dir, name string, v any) error {
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// listJSON returns the *.json base names (without extension) in a directory,
// nil if the directory does not exist.
func listJSON(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			out = append(out, strings.TrimSuffix(e.Name(), ".json"))
		}
	}
	return out, nil
}
