package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const adminS3YAML = `
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

const chatBlock = `  chat:
    openai:
      url: https://api.example.com/v1/chat/completions
      model: my-model
`

const embBlock = `  embeddings:
    bailian_multimodal_embedding:
      url: https://emb.example.com/v1/embeddings
      model: emb-model
`

const vecBlock = `  vectors:
    aliyun_oss_vectors:
      url: https://example-bucket.cn-hangzhou.oss-vectors.aliyuncs.com
      access_key_id: vec-ak
      access_key_secret: vec-sk
      account_id: 1234567890123456
      index: vec-index
`

const llmBlock = "llm:\n" + chatBlock + embBlock + vecBlock

func baseYAML() string {
	return adminS3YAML + llmBlock
}

// yamlWithLLM builds a full config around a custom llm section.
func yamlWithLLM(llm string) string {
	return adminS3YAML + llm
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
	require.NoError(t, os.WriteFile(path, []byte(yamlWithLLM(`llm:
  chat:
    openai:
      url: ' https://api.example.com/v1/chat/completions '
      model: ' my-model '
      effort: ' high '
      headers:
        Authorization: ' Bearer token '
        X-Team: blue
`+embBlock+vecBlock)), 0o644))
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
	require.NoError(t, os.WriteFile(path, []byte(yamlWithLLM(`llm:
`+chatBlock+`  embeddings:
    bailian_multimodal_embedding:
      url: ' https://emb.example.com/v1/embeddings '
      model: ' emb-model '
      dimensions: 1024
      headers:
        Authorization: ' Bearer emb-token '
`+vecBlock)), 0o644))
	cfg, err := loadConfig(path)
	require.NoError(t, err)
	emb := cfg.LLM.Embeddings.BailianMultimodalEmbedding
	require.Equal(t, "https://emb.example.com/v1/embeddings", emb.URL)
	require.Equal(t, "emb-model", emb.Model)
	require.Equal(t, 1024, emb.Dimensions)
	require.Equal(t, map[string]string{"Authorization": "Bearer emb-token"}, emb.Headers)
}

func TestLoadConfigLLMRequired(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	cases := []struct {
		name string
		yaml string
		want string
	}{
		{"no llm at all", adminS3YAML, "llm.chat.openai.url is required"},
		{"chat url missing", yamlWithLLM("llm:\n  chat:\n    openai:\n      model: my-model\n" + embBlock + vecBlock), "llm.chat.openai.url is required"},
		{"chat model missing", yamlWithLLM("llm:\n  chat:\n    openai:\n      url: https://api.example.com/v1/chat/completions\n" + embBlock + vecBlock), "llm.chat.openai.model is required"},
		{"embeddings url missing", yamlWithLLM("llm:\n" + chatBlock + "  embeddings:\n    bailian_multimodal_embedding:\n      model: emb-model\n" + vecBlock), "llm.embeddings.bailian_multimodal_embedding.url is required"},
		{"embeddings model missing", yamlWithLLM("llm:\n" + chatBlock + "  embeddings:\n    bailian_multimodal_embedding:\n      url: https://emb.example.com/v1/embeddings\n" + vecBlock), "llm.embeddings.bailian_multimodal_embedding.model is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, os.WriteFile(path, []byte(tc.yaml), 0o644))
			_, err := loadConfig(path)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestLoadConfigLLMVectors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(yamlWithLLM(`llm:
`+chatBlock+embBlock+`  vectors:
    aliyun_oss_vectors:
      url: ' https://example-bucket.cn-hangzhou.oss-vectors.aliyuncs.com '
      access_key_id: ' vec-ak '
      access_key_secret: ' vec-sk '
      account_id: ' 1234567890123456 '
      index: ' vec-index '
`)), 0o644))
	cfg, err := loadConfig(path)
	require.NoError(t, err)
	vec := cfg.LLM.Vectors.AliyunOSSVectors
	require.Equal(t, "https://example-bucket.cn-hangzhou.oss-vectors.aliyuncs.com", vec.URL)
	require.Equal(t, "vec-ak", vec.AccessKeyID)
	require.Equal(t, "vec-sk", vec.AccessKeySecret)
	require.Equal(t, "1234567890123456", vec.AccountID)
	require.Equal(t, "vec-index", vec.Index)
}

func TestLoadConfigLLMVectorsRequired(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	full := map[string]string{
		"url":               "https://example-bucket.cn-hangzhou.oss-vectors.aliyuncs.com",
		"access_key_id":     "vec-ak",
		"access_key_secret": "vec-sk",
		"account_id":        "1234567890123456",
		"index":             "vec-index",
	}
	keys := []string{"url", "access_key_id", "access_key_secret", "account_id", "index"}
	for _, skip := range keys {
		var b strings.Builder
		b.WriteString("  vectors:\n    aliyun_oss_vectors:\n")
		for _, k := range keys {
			if k == skip {
				continue
			}
			b.WriteString("      " + k + ": " + full[k] + "\n")
		}
		require.NoError(t, os.WriteFile(path, []byte(yamlWithLLM("llm:\n"+chatBlock+embBlock+b.String())), 0o644))
		_, err := loadConfig(path)
		require.Error(t, err, skip)
		require.Contains(t, err.Error(), "llm.vectors.aliyun_oss_vectors."+skip+" is required")
	}
}

func TestLoadConfigLLMVectorsHostShape(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	bad := strings.Replace(baseYAML(),
		"https://example-bucket.cn-hangzhou.oss-vectors.aliyuncs.com",
		"https://example-bucket.oss-cn-hangzhou.aliyuncs.com", 1)
	require.NoError(t, os.WriteFile(path, []byte(bad), 0o644))
	_, err := loadConfig(path)
	require.Error(t, err)
	require.Contains(t, err.Error(), "oss-vectors.aliyuncs.com")
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
`+llmBlock), 0o644))
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

func TestLoadConfigLLMURLRequiresScheme(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	bad := strings.Replace(baseYAML(),
		"https://api.example.com/v1/chat/completions",
		"api.example.com/v1/chat/completions", 1)
	require.NoError(t, os.WriteFile(path, []byte(bad), 0o644))
	_, err := loadConfig(path)
	require.Error(t, err)
	require.Contains(t, err.Error(), "llm.chat.openai.url must start with http:// or https://")
}
