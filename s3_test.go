package main

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/stretchr/testify/require"
)

func testS3Store(t *testing.T) ObjectStore {
	t.Helper()
	store, err := newS3Store(S3Config{
		Endpoint:        "https://s3.example.com",
		Region:          "test",
		Bucket:          "b",
		AccessKeyID:     "x",
		SecretAccessKey: "y",
	})
	require.NoError(t, err)
	return store
}

func TestSignGetURLAttachmentDisposition(t *testing.T) {
	// Presigning is local: static credentials, no network traffic.
	u, err := testS3Store(t).SignGetURL("dir/a.txt", 5*time.Minute)
	require.NoError(t, err)
	require.Contains(t, u, "response-content-disposition=attachment")
	require.Contains(t, u, "X-Amz-Expires=300")
}

func TestSignPreviewURLNoDisposition(t *testing.T) {
	// Preview signs without the attachment override so the browser renders
	// the object inline.
	u, err := testS3Store(t).SignPreviewURL("dir/a.txt", 5*time.Minute)
	require.NoError(t, err)
	require.NotContains(t, u, "response-content-disposition")
	require.Contains(t, u, "X-Amz-Expires=300")
}

func TestIsS3NotFound(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"NoSuchKey type", &types.NoSuchKey{}, true},
		{"generic NoSuchKey", &smithy.GenericAPIError{Code: "NoSuchKey", Message: "no"}, true},
		{"generic NotFound", &smithy.GenericAPIError{Code: "NotFound", Message: "no"}, true},
		{"wrapped NoSuchKey", fmt.Errorf("wrap: %w", &types.NoSuchKey{}), true},
		{"AccessDenied", &smithy.GenericAPIError{Code: "AccessDenied", Message: "no"}, false},
		{"plain error", errors.New("x"), false},
	}
	for _, tc := range cases {
		require.Equal(t, tc.want, isS3NotFound(tc.err), tc.name)
	}
}

func TestAttachmentDisposition(t *testing.T) {
	d := attachmentDisposition(`dir/"q".txt`)
	require.Contains(t, d, `filename="q.txt"`)
	require.Contains(t, d, "filename*=UTF-8''")

	d = attachmentDisposition("dir/evil\r\nX.txt")
	require.NotContains(t, d, "\r")
	require.NotContains(t, d, "\n")
	require.Contains(t, d, `filename="evilX.txt"`)
}
