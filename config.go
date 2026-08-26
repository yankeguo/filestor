package main

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

const defaultUploadWorkspace = "upload-workspace"

type Config struct {
	Admin  AdminConfig  `yaml:"admin"`
	S3     S3Config     `yaml:"s3"`
	Upload UploadConfig `yaml:"upload"`
	LLM    LLMConfig    `yaml:"llm"`
}

type AdminConfig struct {
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

// S3Config targets any S3-compatible object storage (Aliyun OSS, Qcloud COS,
// AWS S3, MinIO, ...). force_path_style is only needed by vendors that do not
// support virtual-hosted buckets (e.g. MinIO).
type S3Config struct {
	Endpoint        string `yaml:"endpoint"`
	Region          string `yaml:"region"`
	Bucket          string `yaml:"bucket"`
	AccessKeyID     string `yaml:"access_key_id"`
	SecretAccessKey string `yaml:"secret_access_key"`
	ForcePathStyle  bool   `yaml:"force_path_style"`
}

type UploadConfig struct {
	Workspace string `yaml:"workspace"`
}

// LLMConfig groups optional chat, embeddings, and vector-store backends.
// Each capability is keyed by provider; unknown provider keys are rejected.
type LLMConfig struct {
	Chat       ChatConfig       `yaml:"chat"`
	Embeddings EmbeddingsConfig `yaml:"embeddings"`
	Vectors    VectorsConfig    `yaml:"vectors"`
}

// ChatConfig holds chat providers. openai is an OpenAI-compatible chat
// endpoint used by POST /upload/analyze; url and model must be set together.
type ChatConfig struct {
	OpenAI OpenAIChatConfig `yaml:"openai"`
}

type OpenAIChatConfig struct {
	URL     string            `yaml:"url"`
	Model   string            `yaml:"model"`
	Effort  string            `yaml:"effort"`
	Headers map[string]string `yaml:"headers"`
}

// EmbeddingsConfig holds embeddings providers. bailian_multimodal_embedding
// is independent of chat (no fallback); url and model must be set together,
// dimensions is optional.
type EmbeddingsConfig struct {
	BailianMultimodalEmbedding BailianMultimodalEmbeddingConfig `yaml:"bailian_multimodal_embedding"`
}

type BailianMultimodalEmbeddingConfig struct {
	URL        string            `yaml:"url"`
	Model      string            `yaml:"model"`
	Headers    map[string]string `yaml:"headers"`
	Dimensions int               `yaml:"dimensions"`
}

// VectorsConfig holds vector-store providers. aliyun_oss_vectors is
// independent of embeddings (no fallback); url, username, password,
// database, and table must be set together.
type VectorsConfig struct {
	AliyunOSSVectors AliyunOSSVectorsConfig `yaml:"aliyun_oss_vectors"`
}

type AliyunOSSVectorsConfig struct {
	URL      string `yaml:"url"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
	Database string `yaml:"database"`
	Table    string `yaml:"table"`
}

func loadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	// KnownFields rejects misspelled keys instead of silently dropping them.
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, err
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) validate() error {
	if c == nil {
		return fmt.Errorf("config is nil")
	}
	c.Admin.Username = strings.TrimSpace(c.Admin.Username)
	if c.Admin.Username == "" {
		return fmt.Errorf("config: admin.username is required")
	}
	if c.Admin.Password == "" {
		return fmt.Errorf("config: admin.password is required")
	}
	c.S3.Endpoint = normalizeEndpoint(c.S3.Endpoint)
	if strings.HasPrefix(c.S3.Endpoint, "http://") {
		log.Println("config: s3.endpoint uses plain http; traffic to the object store is unencrypted")
	}
	c.S3.Region = strings.TrimSpace(c.S3.Region)
	c.S3.Bucket = strings.TrimSpace(c.S3.Bucket)
	c.S3.AccessKeyID = strings.TrimSpace(c.S3.AccessKeyID)
	c.S3.SecretAccessKey = strings.TrimSpace(c.S3.SecretAccessKey)
	if c.S3.Endpoint == "" {
		return fmt.Errorf("config: s3.endpoint is required")
	}
	if c.S3.Region == "" {
		return fmt.Errorf("config: s3.region is required")
	}
	if c.S3.Bucket == "" {
		return fmt.Errorf("config: s3.bucket is required")
	}
	if c.S3.AccessKeyID == "" {
		return fmt.Errorf("config: s3.access_key_id is required")
	}
	if c.S3.SecretAccessKey == "" {
		return fmt.Errorf("config: s3.secret_access_key is required")
	}
	c.Upload.Workspace = strings.TrimSpace(c.Upload.Workspace)
	if c.Upload.Workspace == "" {
		c.Upload.Workspace = defaultUploadWorkspace
	}
	openai := &c.LLM.Chat.OpenAI
	openai.URL = strings.TrimSpace(openai.URL)
	openai.Model = strings.TrimSpace(openai.Model)
	openai.Effort = strings.TrimSpace(openai.Effort)
	if (openai.URL == "") != (openai.Model == "") {
		return fmt.Errorf("config: llm.chat.openai.url and llm.chat.openai.model must be set together")
	}
	openai.Headers = normalizeHeaders(openai.Headers)
	emb := &c.LLM.Embeddings.BailianMultimodalEmbedding
	emb.URL = strings.TrimSpace(emb.URL)
	emb.Model = strings.TrimSpace(emb.Model)
	emb.Headers = normalizeHeaders(emb.Headers)
	if (emb.URL == "") != (emb.Model == "") {
		return fmt.Errorf("config: llm.embeddings.bailian_multimodal_embedding.url and llm.embeddings.bailian_multimodal_embedding.model must be set together")
	}
	if emb.Dimensions < 0 {
		return fmt.Errorf("config: llm.embeddings.bailian_multimodal_embedding.dimensions must not be negative")
	}
	vec := &c.LLM.Vectors.AliyunOSSVectors
	vec.URL = strings.TrimSpace(vec.URL)
	vec.Username = strings.TrimSpace(vec.Username)
	vec.Database = strings.TrimSpace(vec.Database)
	vec.Table = strings.TrimSpace(vec.Table)
	n := 0
	if vec.URL != "" {
		n++
	}
	if vec.Username != "" {
		n++
	}
	if vec.Password != "" {
		n++
	}
	if vec.Database != "" {
		n++
	}
	if vec.Table != "" {
		n++
	}
	if n != 0 && n != 5 {
		return fmt.Errorf("config: llm.vectors.aliyun_oss_vectors.url, llm.vectors.aliyun_oss_vectors.username, llm.vectors.aliyun_oss_vectors.password, llm.vectors.aliyun_oss_vectors.database, and llm.vectors.aliyun_oss_vectors.table must be set together")
	}
	return nil
}

// normalizeHeaders trims header names and values, dropping entries with an
// empty name.
func normalizeHeaders(h map[string]string) map[string]string {
	for k, v := range h {
		nk, nv := strings.TrimSpace(k), strings.TrimSpace(v)
		if nk == "" {
			delete(h, k)
			continue
		}
		if nk != k || nv != v {
			delete(h, k)
			h[nk] = nv
		}
	}
	return h
}

func normalizeEndpoint(ep string) string {
	ep = strings.TrimSpace(ep)
	if ep == "" {
		return ""
	}
	if strings.HasPrefix(ep, "http://") || strings.HasPrefix(ep, "https://") {
		return ep
	}
	return "https://" + ep
}
