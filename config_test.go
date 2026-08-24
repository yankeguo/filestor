package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	require.NoError(t, os.WriteFile(path, []byte(`
admin:
  username: admin
  password: secret
`), 0o644))

	cfg, err := loadConfig(path)
	require.NoError(t, err)
	require.Equal(t, "admin", cfg.Admin.Username)
	require.Equal(t, "secret", cfg.Admin.Password)
}

func TestLoadConfigTrimsUsername(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	require.NoError(t, os.WriteFile(path, []byte("admin:\n  username: '  admin  '\n  password: secret\n"), 0o644))
	cfg, err := loadConfig(path)
	require.NoError(t, err)
	require.Equal(t, "admin", cfg.Admin.Username)
}

func TestLoadConfigRejectsMissingAdmin(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	require.NoError(t, os.WriteFile(path, []byte("admin:\n  username: admin\n"), 0o644))
	_, err := loadConfig(path)
	require.Error(t, err)
	require.Contains(t, err.Error(), "admin.password")

	require.NoError(t, os.WriteFile(path, []byte("admin:\n  password: secret\n"), 0o644))
	_, err = loadConfig(path)
	require.Error(t, err)
	require.Contains(t, err.Error(), "admin.username")
}
