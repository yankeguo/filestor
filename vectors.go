package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// vectorsHTTPTimeout bounds one PutVectors request.
const vectorsHTTPTimeout = 60 * time.Second

// vectorsResourcePath is the unescaped OSS Vectors V4 canonical resource:
// /acs:ossvector:{region}:{account_id}:{bucket}/
func vectorsResourcePath(region, accountID, bucket string) string {
	return "/acs:ossvector:" + region + ":" + accountID + ":" + bucket + "/"
}

// parseVectorsEndpoint splits the configured vectors URL into bucket and
// region: the bucket lives in the host,
// <bucket>.<region>[-internal].oss-vectors.aliyuncs.com.
func parseVectorsEndpoint(rawURL string) (bucket, region string, err error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" {
		return "", "", fmt.Errorf("vectors: invalid url %q", rawURL)
	}
	labels := strings.Split(u.Hostname(), ".")
	if len(labels) >= 3 && labels[2] == "oss-vectors" {
		return labels[0], strings.TrimSuffix(labels[1], "-internal"), nil
	}
	return "", "", fmt.Errorf("vectors: url host must be <bucket>.<region>.oss-vectors.aliyuncs.com, got %q", u.Hostname())
}

// ossV4Escape URI-encodes a resource path segment-wise: unreserved characters
// and the slash stay literal (per the OSS V4 signature spec).
func ossV4Escape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' ||
			c == '-' || c == '.' || c == '_' || c == '~' || c == '/' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// ossV4SignedHeader reports whether a lowercased header name participates in
// the signature by default: x-oss-*, Content-Type, Content-MD5.
func ossV4SignedHeader(low string) bool {
	return strings.HasPrefix(low, "x-oss-") || low == "content-type" || low == "content-md5"
}

// ossV4CanonicalQuery renders the sorted, encoded query parameters; a
// name-only parameter (e.g. ?putVectors) stays bare.
func ossV4CanonicalQuery(rawQuery string) string {
	values := map[string]string{}
	var keys []string
	for q := strings.ReplaceAll(rawQuery, "+", "%20"); q != ""; {
		var pair string
		pair, q, _ = strings.Cut(q, "&")
		if pair == "" {
			continue
		}
		k, v, _ := strings.Cut(pair, "=")
		values[k] = v
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		if b.Len() > 0 {
			b.WriteByte('&')
		}
		b.WriteString(k)
		if values[k] != "" {
			b.WriteByte('=')
			b.WriteString(values[k])
		}
	}
	return b.String()
}

// ossV4Sign signs req with the OSS4-HMAC-SHA256 header signature: it sets the
// x-oss-date, Date and x-oss-content-sha256 headers and finally the
// Authorization header. resourcePath is the unescaped canonical resource
// (vectorsResourcePath for the vectors API; "/<bucket>/<key>" for object
// OSS). additional lists extra signed header names (empty for PutVectors).
// now is the signing time (UTC).
func ossV4Sign(req *http.Request, resourcePath, region, accessKeyID, accessKeySecret string, additional []string, now time.Time) {
	now = now.UTC()
	datetime := now.Format("20060102T150405Z")
	date := now.Format("20060102")
	req.Header.Set("x-oss-date", datetime)
	req.Header.Set("Date", now.Format(http.TimeFormat))
	req.Header.Set("x-oss-content-sha256", "UNSIGNED-PAYLOAD")

	// Canonical headers: the default signed set plus the additional names,
	// sorted, one "name:value\n" line each.
	additionalSet := map[string]bool{}
	for _, h := range additional {
		additionalSet[strings.ToLower(h)] = true
	}
	var headerNames []string
	for k := range req.Header {
		low := strings.ToLower(k)
		if ossV4SignedHeader(low) || additionalSet[low] {
			headerNames = append(headerNames, low)
		}
	}
	sort.Strings(headerNames)
	var canonicalHeaders strings.Builder
	for _, k := range headerNames {
		values := req.Header.Values(k)
		for i, v := range values {
			values[i] = strings.TrimSpace(v)
		}
		canonicalHeaders.WriteString(k + ":" + strings.Join(values, ",") + "\n")
	}

	canonicalRequest := strings.Join([]string{
		req.Method,
		ossV4Escape(resourcePath),
		ossV4CanonicalQuery(req.URL.RawQuery),
		canonicalHeaders.String(),
		strings.Join(additional, ";"),
		"UNSIGNED-PAYLOAD",
	}, "\n")

	scope := date + "/" + region + "/oss/aliyun_v4_request"
	stringToSign := "OSS4-HMAC-SHA256\n" + datetime + "\n" + scope + "\n" +
		hex.EncodeToString(sha256Sum([]byte(canonicalRequest)))

	sha256New := func() hash.Hash { return sha256.New() }
	h1 := hmac.New(sha256New, []byte("aliyun_v4"+accessKeySecret))
	io.WriteString(h1, date)
	h2 := hmac.New(sha256New, h1.Sum(nil))
	io.WriteString(h2, region)
	h3 := hmac.New(sha256New, h2.Sum(nil))
	io.WriteString(h3, "oss")
	h4 := hmac.New(sha256New, h3.Sum(nil))
	io.WriteString(h4, "aliyun_v4_request")
	h := hmac.New(sha256New, h4.Sum(nil))
	io.WriteString(h, stringToSign)
	signature := hex.EncodeToString(h.Sum(nil))

	auth := "OSS4-HMAC-SHA256 Credential=" + accessKeyID + "/" + scope
	if len(additional) > 0 {
		auth += ",AdditionalHeaders=" + strings.Join(additional, ";")
	}
	auth += ",Signature=" + signature
	req.Header.Set("Authorization", auth)
}

func sha256Sum(data []byte) []byte {
	sum := sha256.Sum256(data)
	return sum[:]
}

// vectorsHTTPClient makes PutVectors requests; it is a var so tests can
// redirect the OSS Vectors endpoint to a local server.
var vectorsHTTPClient = &http.Client{Timeout: vectorsHTTPTimeout}

// vectorsClient writes embedding vectors into an Aliyun OSS Vectors index.
type vectorsClient struct {
	cfg       AliyunOSSVectorsConfig
	http      *http.Client
	bucket    string
	region    string
	accountID string
}

func newVectorsClient(cfg AliyunOSSVectorsConfig) (*vectorsClient, error) {
	bucket, region, err := parseVectorsEndpoint(cfg.URL)
	if err != nil {
		return nil, err
	}
	return &vectorsClient{cfg: cfg, http: vectorsHTTPClient, bucket: bucket, region: region, accountID: cfg.AccountID}, nil
}

type vectorItem struct {
	Key      string               `json:"key"`
	Data     map[string][]float32 `json:"data"`
	Metadata map[string]string    `json:"metadata"`
}

// putVectors writes one vector per digest file into the configured index,
// keyed by bundle id so the batch can be found and overwritten as a unit.
func (c *vectorsClient) putVectors(ctx context.Context, bundleID string, meta bundleMeta, names []string, vecs [][]float32) error {
	items := make([]vectorItem, 0, len(names))
	for i, name := range names {
		kind := "image"
		if isDigestText(name) {
			kind = "text"
		}
		items = append(items, vectorItem{
			Key:  bundleID + "/" + name,
			Data: map[string][]float32{"float32": vecs[i]},
			Metadata: map[string]string{
				"bundle": bundleID,
				"kind":   kind,
				"name":   name,
				"title":  meta.Title,
				"time":   meta.Time,
			},
		})
	}
	body, err := json.Marshal(map[string]any{"indexName": c.cfg.Index, "vectors": items})
	if err != nil {
		return err
	}
	log.Printf("vectors: writing %d vectors to index %s", len(items), c.cfg.Index)
	endpoint := strings.TrimRight(c.cfg.URL, "/") + "/?putVectors"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	ossV4Sign(req, vectorsResourcePath(c.region, c.accountID, c.bucket), c.region, c.cfg.AccessKeyID, c.cfg.AccessKeySecret, nil, time.Now())
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("vectors: HTTP %d: %s", resp.StatusCode, truncateBody(data))
	}
	return nil
}
