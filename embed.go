package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// embedHTTPTimeout bounds one embedding request; a multi-MB image can take a
// while on the model side.
const embedHTTPTimeout = 120 * time.Second

// embedClient calls a Bailian (DashScope) multimodal embedding endpoint: one
// HTTP request per content item, because the per-request image limits vary by
// model and per-item requests stay model-agnostic.
type embedClient struct {
	cfg  BailianMultimodalEmbeddingConfig
	http *http.Client
}

func newEmbedClient(cfg BailianMultimodalEmbeddingConfig) *embedClient {
	return &embedClient{cfg: cfg, http: &http.Client{Timeout: embedHTTPTimeout}}
}

type embedParameters struct {
	Dimension int `json:"dimension,omitempty"`
}

type embedRequest struct {
	Model      string           `json:"model"`
	Input      embedInput       `json:"input"`
	Parameters *embedParameters `json:"parameters,omitempty"`
}

type embedInput struct {
	Contents []map[string]string `json:"contents"`
}

type embedResponse struct {
	Output struct {
		Embeddings []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
			Type      string    `json:"type"`
		} `json:"embeddings"`
	} `json:"output"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// embedContent embeds one content item ({"text": …} or {"image": data-URI})
// and returns its vector.
func (c *embedClient) embedContent(ctx context.Context, content map[string]string) ([]float32, error) {
	req := embedRequest{Model: c.cfg.Model}
	req.Input.Contents = []map[string]string{content}
	if c.cfg.Dimensions > 0 {
		req.Parameters = &embedParameters{Dimension: c.cfg.Dimensions}
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	for k, v := range c.cfg.Headers {
		httpReq.Header.Set(k, v)
	}
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embed: HTTP %d: %s", resp.StatusCode, truncateBody(data))
	}
	var out embedResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("embed: bad response: %w", err)
	}
	if out.Code != "" {
		return nil, fmt.Errorf("embed: %s: %s", out.Code, out.Message)
	}
	if len(out.Output.Embeddings) == 0 || len(out.Output.Embeddings[0].Embedding) == 0 {
		return nil, errors.New("embed: no vector in response")
	}
	return out.Output.Embeddings[0].Embedding, nil
}

// truncateBody keeps an upstream error body short for logs and job errors.
func truncateBody(data []byte) string {
	const maxErrBody = 256
	msg := strings.TrimSpace(string(data))
	if len(msg) > maxErrBody {
		msg = msg[:maxErrBody] + "..."
	}
	return msg
}

// isDigestText reports whether a digest file name is a marked text chunk.
func isDigestText(name string) bool {
	return strings.HasPrefix(name, "text-") && strings.HasSuffix(name, ".txt")
}

// digestImageDataURI reads a digest image and returns it as a base64 data URI
// with the sniffed format (jpeg/png/gif).
func digestImageDataURI(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	format := "jpeg"
	if mime := sniffImageMIME(data); mime != "" {
		format = strings.TrimPrefix(mime, "image/")
	}
	return "data:image/" + format + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}

// embedDigest embeds every digest file (text chunks and images) in order, one
// request per file, and returns the vectors aligned with names. onItem is
// called before each request for progress reporting. All vectors must share
// one dimension.
func (c *embedClient) embedDigest(ctx context.Context, dir string, names []string, onItem func(i int, name string)) ([][]float32, error) {
	vecs := make([][]float32, 0, len(names))
	dim := 0
	for i, name := range names {
		if onItem != nil {
			onItem(i, name)
		}
		var content map[string]string
		if isDigestText(name) {
			data, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				return nil, err
			}
			content = map[string]string{"text": string(data)}
		} else {
			uri, err := digestImageDataURI(filepath.Join(dir, name))
			if err != nil {
				return nil, err
			}
			content = map[string]string{"image": uri}
		}
		vec, err := c.embedContent(ctx, content)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if dim == 0 {
			dim = len(vec)
		} else if len(vec) != dim {
			return nil, fmt.Errorf("%s: embedding dimension %d does not match %d", name, len(vec), dim)
		}
		vecs = append(vecs, vec)
	}
	return vecs, nil
}
