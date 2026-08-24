package main

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Admin  AdminConfig  `yaml:"admin"`
	Aliyun AliyunConfig `yaml:"aliyun"`
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
		return fmt.Errorf("config: admin.username is required")
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
