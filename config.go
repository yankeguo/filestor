package main

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	defaultUploadWorkspace   = "upload-workspace"
	defaultEmbeddingsDialect = "bailian_multimodal_embedding"
)

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

// LLMConfig groups the optional OpenAI-compatible endpoints.
type LLMConfig struct {
	Chat       ChatConfig       `yaml:"chat"`
	Embeddings EmbeddingsConfig `yaml:"embeddings"`
}

// ChatConfig describes an optional OpenAI-compatible chat endpoint used by
// POST /upload/analyze. url and model must be set together.
type ChatConfig struct {
	URL     string            `yaml:"url"`
	Model   string            `yaml:"model"`
	Effort  string            `yaml:"effort"`
	Headers map[string]string `yaml:"headers"`
}

// EmbeddingsConfig describes an optional OpenAI-compatible embeddings
// endpoint. url and model must be set together; dimensions is optional.
// dialect selects the request/response shape; empty defaults to
// bailian_multimodal_embedding, currently the only supported value.
type EmbeddingsConfig struct {
	URL        string            `yaml:"url"`
	Model      string            `yaml:"model"`
	Headers    map[string]string `yaml:"headers"`
	Dimensions int               `yaml:"dimensions"`
	Dialect    string            `yaml:"dialect"`
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
	c.LLM.Chat.URL = strings.TrimSpace(c.LLM.Chat.URL)
	c.LLM.Chat.Model = strings.TrimSpace(c.LLM.Chat.Model)
	c.LLM.Chat.Effort = strings.TrimSpace(c.LLM.Chat.Effort)
	if (c.LLM.Chat.URL == "") != (c.LLM.Chat.Model == "") {
		return fmt.Errorf("config: llm.chat.url and llm.chat.model must be set together")
	}
	c.LLM.Chat.Headers = normalizeHeaders(c.LLM.Chat.Headers)
	c.LLM.Embeddings.URL = strings.TrimSpace(c.LLM.Embeddings.URL)
	c.LLM.Embeddings.Model = strings.TrimSpace(c.LLM.Embeddings.Model)
	c.LLM.Embeddings.Headers = normalizeHeaders(c.LLM.Embeddings.Headers)
	c.LLM.Embeddings.Dialect = strings.TrimSpace(c.LLM.Embeddings.Dialect)
	if (c.LLM.Embeddings.URL == "") != (c.LLM.Embeddings.Model == "") {
		return fmt.Errorf("config: llm.embeddings.url and llm.embeddings.model must be set together")
	}
	if c.LLM.Embeddings.Dimensions < 0 {
		return fmt.Errorf("config: llm.embeddings.dimensions must not be negative")
	}
	if c.LLM.Embeddings.Dialect == "" {
		c.LLM.Embeddings.Dialect = defaultEmbeddingsDialect
	} else if c.LLM.Embeddings.Dialect != defaultEmbeddingsDialect {
		return fmt.Errorf("config: llm.embeddings.dialect %q is not supported", c.LLM.Embeddings.Dialect)
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
