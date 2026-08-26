package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func baseYAML() string {
	return `
admin:
  username: admin
  password: secret
s3:
  endpoint: s3.oss-cn-hangzhou.aliyuncs.com
  region: cn-hangzhou
  bucket: example-bucket
  access_key_id: ak
  secret_access_key: sk
`
}

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(baseYAML()), 0o644))

	cfg, err := loadConfig(path)
	require.NoError(t, err)
	require.Equal(t, "admin", cfg.Admin.Username)
	require.Equal(t, "secret", cfg.Admin.Password)
	require.Equal(t, "https://s3.oss-cn-hangzhou.aliyuncs.com", cfg.S3.Endpoint)
	require.Equal(t, "cn-hangzhou", cfg.S3.Region)
	require.Equal(t, "example-bucket", cfg.S3.Bucket)
	require.Equal(t, "ak", cfg.S3.AccessKeyID)
	require.Equal(t, "sk", cfg.S3.SecretAccessKey)
	require.False(t, cfg.S3.ForcePathStyle)
	require.Equal(t, defaultUploadWorkspace, cfg.Upload.Workspace)
}

func TestLoadConfigLLM(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(baseYAML()+`llm:
  chat:
    openai:
      url: ' https://api.example.com/v1/chat/completions '
      model: ' my-model '
      effort: ' high '
      headers:
        Authorization: ' Bearer token '
        X-Team: blue
`), 0o644))
	cfg, err := loadConfig(path)
	require.NoError(t, err)
	require.Equal(t, "https://api.example.com/v1/chat/completions", cfg.LLM.Chat.OpenAI.URL)
	require.Equal(t, "my-model", cfg.LLM.Chat.OpenAI.Model)
	require.Equal(t, "high", cfg.LLM.Chat.OpenAI.Effort)
	require.Equal(t, map[string]string{"Authorization": "Bearer token", "X-Team": "blue"}, cfg.LLM.Chat.OpenAI.Headers)
}

func TestLoadConfigLLMEmbeddings(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(baseYAML()+`llm:
  chat:
    openai:
      url: https://api.example.com/v1/chat/completions
      model: my-model
      headers:
        Authorization: Bearer token
  embeddings:
    bailian_multimodal_embedding:
      url: ' https://emb.example.com/v1/embeddings '
      model: ' emb-model '
      dimensions: 1024
      headers:
        Authorization: ' Bearer emb-token '
`), 0o644))
	cfg, err := loadConfig(path)
	require.NoError(t, err)
	emb := cfg.LLM.Embeddings.BailianMultimodalEmbedding
	require.Equal(t, "https://emb.example.com/v1/embeddings", emb.URL)
	require.Equal(t, "emb-model", emb.Model)
	require.Equal(t, 1024, emb.Dimensions)
	require.Equal(t, map[string]string{"Authorization": "Bearer emb-token"}, emb.Headers)

	// chat and embeddings are independent: embeddings gets no chat defaults.
	require.NoError(t, os.WriteFile(path, []byte(baseYAML()+`llm:
  chat:
    openai:
      url: https://api.example.com/v1/chat/completions
      model: my-model
      headers:
        Authorization: Bearer token
`), 0o644))
	cfg, err = loadConfig(path)
	require.NoError(t, err)
	emb = cfg.LLM.Embeddings.BailianMultimodalEmbedding
	require.Empty(t, emb.URL)
	require.Empty(t, emb.Model)
	require.Empty(t, emb.Headers)
	require.Zero(t, emb.Dimensions)
}

func TestLoadConfigLLMEmbeddingsRequiresPair(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	for _, extra := range []string{
		"llm:\n  embeddings:\n    bailian_multimodal_embedding:\n      model: emb-model\n",
		"llm:\n  embeddings:\n    bailian_multimodal_embedding:\n      url: https://emb.example.com/v1/embeddings\n",
	} {
		require.NoError(t, os.WriteFile(path, []byte(baseYAML()+extra), 0o644))
		_, err := loadConfig(path)
		require.Error(t, err, extra)
		require.Contains(t, err.Error(), "llm.embeddings.bailian_multimodal_embedding.url and llm.embeddings.bailian_multimodal_embedding.model")
	}
}

func TestLoadConfigLLMVectors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(baseYAML()+`llm:
  embeddings:
    bailian_multimodal_embedding:
      url: https://emb.example.com/v1/embeddings
      model: emb-model
  vectors:
    aliyun_oss_vectors:
      url: ' https://vectors.example.com '
      username: ' vec-user '
      password: vec-pass
      database: ' vec-db '
      table: ' vec-table '
`), 0o644))
	cfg, err := loadConfig(path)
	require.NoError(t, err)
	vec := cfg.LLM.Vectors.AliyunOSSVectors
	require.Equal(t, "https://vectors.example.com", vec.URL)
	require.Equal(t, "vec-user", vec.Username)
	require.Equal(t, "vec-pass", vec.Password)
	require.Equal(t, "vec-db", vec.Database)
	require.Equal(t, "vec-table", vec.Table)

	// embeddings and vectors are independent: vectors gets no embeddings defaults.
	require.NoError(t, os.WriteFile(path, []byte(baseYAML()+`llm:
  embeddings:
    bailian_multimodal_embedding:
      url: https://emb.example.com/v1/embeddings
      model: emb-model
`), 0o644))
	cfg, err = loadConfig(path)
	require.NoError(t, err)
	vec = cfg.LLM.Vectors.AliyunOSSVectors
	require.Empty(t, vec.URL)
	require.Empty(t, vec.Username)
	require.Empty(t, vec.Password)
	require.Empty(t, vec.Database)
	require.Empty(t, vec.Table)
}

func TestLoadConfigLLMVectorsRequiresTogether(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	full := map[string]string{
		"url":      "https://vectors.example.com",
		"username": "vec-user",
		"password": "vec-pass",
		"database": "vec-db",
		"table":    "vec-table",
	}
	keys := []string{"url", "username", "password", "database", "table"}
	for _, skip := range keys {
		var b strings.Builder
		b.WriteString("llm:\n  vectors:\n    aliyun_oss_vectors:\n")
		for _, k := range keys {
			if k == skip {
				continue
			}
			b.WriteString("      " + k + ": " + full[k] + "\n")
		}
		require.NoError(t, os.WriteFile(path, []byte(baseYAML()+b.String()), 0o644))
		_, err := loadConfig(path)
		require.Error(t, err, skip)
		require.Contains(t, err.Error(), "llm.vectors.aliyun_oss_vectors.url, llm.vectors.aliyun_oss_vectors.username, llm.vectors.aliyun_oss_vectors.password, llm.vectors.aliyun_oss_vectors.database, and llm.vectors.aliyun_oss_vectors.table")
	}
}

func TestLoadConfigLLMRequiresPair(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	for _, extra := range []string{
		"llm:\n  chat:\n    openai:\n      url: https://api.example.com/v1/chat/completions\n",
		"llm:\n  chat:\n    openai:\n      model: my-model\n",
	} {
		require.NoError(t, os.WriteFile(path, []byte(baseYAML()+extra), 0o644))
		_, err := loadConfig(path)
		require.Error(t, err, extra)
		require.Contains(t, err.Error(), "llm.chat.openai.url and llm.chat.openai.model")
	}
}

func TestLoadConfigUploadWorkspace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(baseYAML()+"upload:\n  workspace: /var/filestor/staging\n"), 0o644))
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
s3:
  endpoint: https://s3.oss-cn-hangzhou.aliyuncs.com
  region: cn-hangzhou
  bucket: b
  access_key_id: ak
  secret_access_key: sk
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

func TestLoadConfigRejectsMissingS3(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	cases := []struct {
		yaml string
		want string
	}{
		{"admin:\n  username: a\n  password: p\n", "s3.endpoint"},
		{"admin:\n  username: a\n  password: p\ns3:\n  region: r\n  bucket: b\n  access_key_id: ak\n  secret_access_key: sk\n", "s3.endpoint"},
		{"admin:\n  username: a\n  password: p\ns3:\n  endpoint: https://s3.oss-cn-hangzhou.aliyuncs.com\n  bucket: b\n  access_key_id: ak\n  secret_access_key: sk\n", "s3.region"},
		{"admin:\n  username: a\n  password: p\ns3:\n  endpoint: https://s3.oss-cn-hangzhou.aliyuncs.com\n  region: r\n  access_key_id: ak\n  secret_access_key: sk\n", "s3.bucket"},
		{"admin:\n  username: a\n  password: p\ns3:\n  endpoint: https://s3.oss-cn-hangzhou.aliyuncs.com\n  region: r\n  bucket: b\n  secret_access_key: sk\n", "s3.access_key_id"},
		{"admin:\n  username: a\n  password: p\ns3:\n  endpoint: https://s3.oss-cn-hangzhou.aliyuncs.com\n  region: r\n  bucket: b\n  access_key_id: ak\n", "s3.secret_access_key"},
	}
	for _, tc := range cases {
		require.NoError(t, os.WriteFile(path, []byte(tc.yaml), 0o644))
		_, err := loadConfig(path)
		require.Error(t, err, tc.want)
		require.Contains(t, err.Error(), tc.want)
	}
}
