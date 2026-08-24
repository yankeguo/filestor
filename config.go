package main

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

const defaultUploadWorkspace = "upload-workspace"

type Config struct {
	Admin  AdminConfig  `yaml:"admin"`
	Aliyun AliyunConfig `yaml:"aliyun"`
	Upload UploadConfig `yaml:"upload"`
	LLM    LLMConfig    `yaml:"llm"`
}

type AdminConfig struct {
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

type AliyunConfig struct {
	OSS OSSConfig `yaml:"oss"`
}

type OSSConfig struct {
	Endpoint        string `yaml:"endpoint"`
	Bucket          string `yaml:"bucket"`
	AccessKeyID     string `yaml:"access_key_id"`
	AccessKeySecret string `yaml:"access_key_secret"`
}

type UploadConfig struct {
	Workspace string `yaml:"workspace"`
}

// LLMConfig describes an optional OpenAI-compatible endpoint used by
// POST /upload/suggest. url and model must be set together.
type LLMConfig struct {
	URL     string            `yaml:"url"`
	Model   string            `yaml:"model"`
	Effort  string            `yaml:"effort"`
	Headers map[string]string `yaml:"headers"`
}

func loadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
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
	c.Aliyun.OSS.Endpoint = normalizeOSSEndpoint(c.Aliyun.OSS.Endpoint)
	c.Aliyun.OSS.Bucket = strings.TrimSpace(c.Aliyun.OSS.Bucket)
	c.Aliyun.OSS.AccessKeyID = strings.TrimSpace(c.Aliyun.OSS.AccessKeyID)
	c.Aliyun.OSS.AccessKeySecret = strings.TrimSpace(c.Aliyun.OSS.AccessKeySecret)
	if c.Aliyun.OSS.Endpoint == "" {
		return fmt.Errorf("config: aliyun.oss.endpoint is required")
	}
	if c.Aliyun.OSS.Bucket == "" {
		return fmt.Errorf("config: aliyun.oss.bucket is required")
	}
	if c.Aliyun.OSS.AccessKeyID == "" {
		return fmt.Errorf("config: aliyun.oss.access_key_id is required")
	}
	if c.Aliyun.OSS.AccessKeySecret == "" {
		return fmt.Errorf("config: aliyun.oss.access_key_secret is required")
	}
	c.Upload.Workspace = strings.TrimSpace(c.Upload.Workspace)
	if c.Upload.Workspace == "" {
		c.Upload.Workspace = defaultUploadWorkspace
	}
	c.LLM.URL = strings.TrimSpace(c.LLM.URL)
	c.LLM.Model = strings.TrimSpace(c.LLM.Model)
	c.LLM.Effort = strings.TrimSpace(c.LLM.Effort)
	if (c.LLM.URL == "") != (c.LLM.Model == "") {
		return fmt.Errorf("config: llm.url and llm.model must be set together")
	}
	for k, v := range c.LLM.Headers {
		nk, nv := strings.TrimSpace(k), strings.TrimSpace(v)
		if nk == "" {
			delete(c.LLM.Headers, k)
			continue
		}
		if nk != k || nv != v {
			delete(c.LLM.Headers, k)
			c.LLM.Headers[nk] = nv
		}
	}
	return nil
}

func normalizeOSSEndpoint(ep string) string {
	ep = strings.TrimSpace(ep)
	if ep == "" {
		return ""
	}
	if strings.HasPrefix(ep, "http://") || strings.HasPrefix(ep, "https://") {
		return ep
	}
	return "https://" + ep
}
