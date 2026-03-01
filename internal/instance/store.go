package instance

import (
	"encoding/json"
	"os"
	"os/user"
	"path/filepath"
)

// PersistedInstance is the on-disk representation of a managed instance.
type PersistedInstance struct {
	ID             string `json:"id"`
	SessionID      string `json:"session_id"`
	Cwd            string `json:"cwd"`
	Model          string `json:"model,omitempty"`
	PermissionMode string `json:"permission_mode,omitempty"`
	HasSession     bool   `json:"has_session"`
	BackendID      string `json:"backend_id,omitempty"`
}

func storePath() string {
	dir := filepath.Join(os.TempDir(), "transitive")
	if u, err := user.Current(); err == nil {
		dir = filepath.Join(u.HomeDir, ".transitive")
	}
	return filepath.Join(dir, "instances.json")
}

func loadStore() ([]PersistedInstance, error) {
	data, err := os.ReadFile(storePath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var instances []PersistedInstance
	if err := json.Unmarshal(data, &instances); err != nil {
		return nil, err
	}
	return instances, nil
}

func saveStore(instances []PersistedInstance) error {
	path := storePath()
	data, err := json.MarshalIndent(instances, "", "  ")
	if err != nil {
		return err
	}
	os.MkdirAll(filepath.Dir(path), 0o700)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
