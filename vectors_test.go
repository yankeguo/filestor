package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestParseVectorsEndpoint(t *testing.T) {
	bucket, region, err := parseVectorsEndpoint("https://examplebucket-123456.cn-hangzhou.oss-vectors.aliyuncs.com")
	require.NoError(t, err)
	require.Equal(t, "examplebucket-123456", bucket)
	require.Equal(t, "cn-hangzhou", region)

	bucket, region, err = parseVectorsEndpoint("https://bkt.cn-hangzhou-internal.oss-vectors.aliyuncs.com")
	require.NoError(t, err)
	require.Equal(t, "bkt", bucket)
	require.Equal(t, "cn-hangzhou", region)

	for _, bad := range []string{
		"https://oss-cn-hangzhou.aliyuncs.com",
		"https://bkt.oss-cn-hangzhou.aliyuncs.com",
		"not a url",
	} {
		_, _, err := parseVectorsEndpoint(bad)
		require.Error(t, err, bad)
	}
}

func TestOSSV4SignDocExample(t *testing.T) {
	// The worked example from the OSS V4 signature documentation. The
	// canonical request matches the doc byte for byte (its SHA-256 is the
	// documented c46d9639…); the doc's printed signature is illustrative
	// only, so the expectation below is the value independently derived
	// from the documented key chain with SK=yourAccessKeySecret.
	req, err := http.NewRequest(http.MethodPut, "https://examplebucket.oss-cn-hangzhou.aliyuncs.com/exampleobject", strings.NewReader("hi"))
	require.NoError(t, err)
	req.Header.Set("Content-Disposition", "attachment")
	req.Header.Set("Content-Length", "3")
	req.Header.Set("Content-MD5", "ICy5YqxZB1uWSwcVLSNLcA==")
	req.Header.Set("Content-Type", "text/plain")
	now := time.Date(2025, 4, 11, 6, 41, 24, 0, time.UTC)
	ossV4Sign(req, "/examplebucket/exampleobject", "cn-hangzhou", "LTAI********************", "yourAccessKeySecret", []string{"content-disposition", "content-length"}, now)

	auth := req.Header.Get("Authorization")
	require.Contains(t, auth, "OSS4-HMAC-SHA256 Credential=")
	require.Contains(t, auth, "/20250411/cn-hangzhou/oss/aliyun_v4_request")
	require.Contains(t, auth, "AdditionalHeaders=content-disposition;content-length")
	require.Contains(t, auth, "Signature=d3694c2dfc5371ee6acd35e88c4871ac95a7ba01d3a2f476768fe61218590097")
	require.Equal(t, "20250411T064124Z", req.Header.Get("x-oss-date"))
	require.Equal(t, "UNSIGNED-PAYLOAD", req.Header.Get("x-oss-content-sha256"))
}

func TestPutVectors(t *testing.T) {
	var gotMethod, gotQuery, gotAuth, gotContentType string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotQuery = r.URL.RawQuery
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := &vectorsClient{
		cfg:    AliyunOSSVectorsConfig{URL: srv.URL, AccessKeyID: "ak", AccessKeySecret: "sk", Index: "idx"},
		http:   srv.Client(),
		bucket: "bkt",
		region: "cn-hangzhou",
	}
	meta := bundleMeta{ID: "uuid-1", Title: "weekly", Time: "2026-08-24T06:59"}
	err := c.putVectors(context.Background(), "uuid-1", meta,
		[]string{"image-01-pic.png", "text-01.txt"},
		[][]float32{{0.1, 0.2}, {0.3, 0.4}})
	require.NoError(t, err)

	require.Equal(t, http.MethodPost, gotMethod)
	require.Equal(t, "putVectors", gotQuery)
	require.Equal(t, "application/json", gotContentType)
	require.True(t, strings.HasPrefix(gotAuth, "OSS4-HMAC-SHA256 Credential=ak/"), gotAuth)
	require.Contains(t, gotAuth, "/cn-hangzhou/oss/aliyun_v4_request")

	require.Equal(t, "idx", gotBody["indexName"])
	vs, ok := gotBody["vectors"].([]any)
	require.True(t, ok)
	require.Len(t, vs, 2)
	v0 := vs[0].(map[string]any)
	require.Equal(t, "uuid-1/image-01-pic.png", v0["key"])
	require.Equal(t, map[string]any{"float32": []any{0.1, 0.2}}, v0["data"])
	md := v0["metadata"].(map[string]any)
	require.Equal(t, "uuid-1", md["bundle"])
	require.Equal(t, "image", md["kind"])
	require.Equal(t, "image-01-pic.png", md["name"])
	require.Equal(t, "weekly", md["title"])
	require.Equal(t, "2026-08-24T06:59", md["time"])
	v1 := vs[1].(map[string]any)
	require.Equal(t, "uuid-1/text-01.txt", v1["key"])
	require.Equal(t, "text", v1["metadata"].(map[string]any)["kind"])
}

func TestPutVectorsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"code":"AccessDenied"}`))
	}))
	t.Cleanup(srv.Close)
	c := &vectorsClient{
		cfg:    AliyunOSSVectorsConfig{URL: srv.URL, AccessKeyID: "ak", AccessKeySecret: "sk", Index: "idx"},
		http:   srv.Client(),
		bucket: "bkt",
		region: "cn-hangzhou",
	}
	err := c.putVectors(context.Background(), "uuid-1", bundleMeta{ID: "uuid-1", Time: "2026-08-24T06:59"},
		[]string{"text-01.txt"}, [][]float32{{0.1}})
	require.Error(t, err)
	require.Contains(t, err.Error(), "HTTP 403")
	require.Contains(t, err.Error(), "AccessDenied")
}
