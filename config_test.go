package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func ossYAML() string {
	return `
admin:
  username: admin
  password: secret
aliyun:
  oss:
    endpoint: oss-cn-hangzhou.aliyuncs.com
    bucket: example-bucket
    access_key_id: ak
    access_key_secret: sk
`
}

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(ossYAML()), 0o644))

	cfg, err := loadConfig(path)
	require.NoError(t, err)
	require.Equal(t, "admin", cfg.Admin.Username)
	require.Equal(t, "secret", cfg.Admin.Password)
	require.Equal(t, "https://oss-cn-hangzhou.aliyuncs.com", cfg.Aliyun.OSS.Endpoint)
	require.Equal(t, "example-bucket", cfg.Aliyun.OSS.Bucket)
	require.Equal(t, "ak", cfg.Aliyun.OSS.AccessKeyID)
	require.Equal(t, "sk", cfg.Aliyun.OSS.AccessKeySecret)
	require.Equal(t, defaultUploadWorkspace, cfg.Upload.Workspace)
}

func TestLoadConfigLLM(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(ossYAML()+`llm:
  url: ' https://api.example.com/v1/chat/completions '
  model: ' my-model '
  effort: ' high '
  headers:
    Authorization: ' Bearer token '
    X-Team: blue
`), 0o644))
	cfg, err := loadConfig(path)
	require.NoError(t, err)
	require.Equal(t, "https://api.example.com/v1/chat/completions", cfg.LLM.URL)
	require.Equal(t, "my-model", cfg.LLM.Model)
	require.Equal(t, "high", cfg.LLM.Effort)
	require.Equal(t, map[string]string{"Authorization": "Bearer token", "X-Team": "blue"}, cfg.LLM.Headers)
}

func TestLoadConfigLLMRequiresPair(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	for _, extra := range []string{
		"llm:\n  url: https://api.example.com/v1/chat/completions\n",
		"llm:\n  model: my-model\n",
	} {
		require.NoError(t, os.WriteFile(path, []byte(ossYAML()+extra), 0o644))
		_, err := loadConfig(path)
		require.Error(t, err, extra)
		require.Contains(t, err.Error(), "llm.url and llm.model")
	}
}

func TestLoadConfigUploadWorkspace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(ossYAML()+"upload:\n  workspace: /var/filestor/staging\n"), 0o644))
	cfg, err := loadConfig(path)
	require.NoError(t, err)
	require.Equal(t, "/var/filestor/staging", cfg.Upload.Workspace)
}

func TestLoadConfigTrimsUsername(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
admin:
  username: '  admin  '
  password: secret
aliyun:
  oss:
    endpoint: https://oss-cn-hangzhou.aliyuncs.com
    bucket: b
    access_key_id: ak
    access_key_secret: sk
`), 0o644))
	cfg, err := loadConfig(path)
	require.NoError(t, err)
	require.Equal(t, "admin", cfg.Admin.Username)
}

func TestLoadConfigRejectsMissingAdmin(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("admin:\n  username: admin\n"), 0o644))
	_, err := loadConfig(path)
	require.Error(t, err)
	require.Contains(t, err.Error(), "admin.password")

	require.NoError(t, os.WriteFile(path, []byte("admin:\n  password: secret\n"), 0o644))
	_, err = loadConfig(path)
	require.Error(t, err)
	require.Contains(t, err.Error(), "admin.username")
}

func TestLoadConfigRejectsMissingOSS(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	cases := []struct {
		yaml string
		want string
	}{
		{"admin:\n  username: a\n  password: p\n", "aliyun.oss.endpoint"},
		{"admin:\n  username: a\n  password: p\naliyun:\n  oss:\n    bucket: b\n    access_key_id: ak\n    access_key_secret: sk\n", "aliyun.oss.endpoint"},
		{"admin:\n  username: a\n  password: p\naliyun:\n  oss:\n    endpoint: https://oss-cn-hangzhou.aliyuncs.com\n    access_key_id: ak\n    access_key_secret: sk\n", "aliyun.oss.bucket"},
		{"admin:\n  username: a\n  password: p\naliyun:\n  oss:\n    endpoint: https://oss-cn-hangzhou.aliyuncs.com\n    bucket: b\n    access_key_secret: sk\n", "aliyun.oss.access_key_id"},
		{"admin:\n  username: a\n  password: p\naliyun:\n  oss:\n    endpoint: https://oss-cn-hangzhou.aliyuncs.com\n    bucket: b\n    access_key_id: ak\n", "aliyun.oss.access_key_secret"},
	}
	for _, tc := range cases {
		require.NoError(t, os.WriteFile(path, []byte(tc.yaml), 0o644))
		_, err := loadConfig(path)
		require.Error(t, err, tc.want)
		require.Contains(t, err.Error(), tc.want)
	}
}
