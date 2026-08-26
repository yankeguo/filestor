package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/url"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

const (
	listPageSize  = 200
	signURLTTL    = 5 * time.Minute
	listDelimiter = "/"
	// getMaxBytes caps metadata reads (monthly indexes and .meta.json).
	getMaxBytes = 1 << 20
)

var errNotFound = errors.New("not found")

type ObjectInfo struct {
	Key          string
	Size         int64
	LastModified time.Time
}

type ListPage struct {
	Prefixes    []string
	Objects     []ObjectInfo
	IsTruncated bool
}

type ObjectStore interface {
	List(prefix, marker string) (ListPage, error)
	// ListKeys returns every object key under prefix (no delimiter), paging
	// internally; used at startup to enumerate the monthly index files.
	ListKeys(prefix string) ([]string, error)
	Get(key string) ([]byte, error)
	SignGetURL(key string, ttl time.Duration) (string, error)
	SignPreviewURL(key string, ttl time.Duration) (string, error)
	Put(key string, r io.Reader, size int64) error
}

type s3Store struct {
	client *s3.Client
	bucket string
}

func newS3Store(cfg S3Config) (ObjectStore, error) {
	client := s3.New(s3.Options{
		Region:       cfg.Region,
		BaseEndpoint: aws.String(cfg.Endpoint),
		Credentials:  credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		UsePathStyle: cfg.ForcePathStyle,
		// Many S3-compatible vendors reject the extra integrity checksums
		// the SDK sends by default since 2025; only use them when required.
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	})
	return &s3Store{client: client, bucket: cfg.Bucket}, nil
}

func (s *s3Store) List(prefix, marker string) (ListPage, error) {
	in := &s3.ListObjectsV2Input{
		Bucket:    aws.String(s.bucket),
		Delimiter: aws.String(listDelimiter),
		MaxKeys:   aws.Int32(listPageSize),
	}
	if prefix != "" {
		in.Prefix = aws.String(prefix)
	}
	if marker != "" {
		in.StartAfter = aws.String(marker)
	}
	out, err := s.client.ListObjectsV2(context.Background(), in)
	if err != nil {
		return ListPage{}, err
	}
	page := ListPage{IsTruncated: aws.ToBool(out.IsTruncated)}
	for _, p := range out.CommonPrefixes {
		page.Prefixes = append(page.Prefixes, aws.ToString(p.Prefix))
	}
	for _, obj := range out.Contents {
		key := aws.ToString(obj.Key)
		if key == "" || key == prefix {
			continue
		}
		page.Objects = append(page.Objects, ObjectInfo{
			Key:          key,
			Size:         aws.ToInt64(obj.Size),
			LastModified: aws.ToTime(obj.LastModified),
		})
	}
	// No ContinuationToken plumbing: StartAfter on the last listed key or
	// common prefix resumes the listing just as well.
	return page, nil
}

func (s *s3Store) Get(key string) ([]byte, error) {
	out, err := s.client.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isS3NotFound(err) {
			return nil, errNotFound
		}
		return nil, err
	}
	defer out.Body.Close()
	if out.ContentLength != nil && *out.ContentLength > getMaxBytes {
		_, _ = io.Copy(io.Discard, out.Body)
		return nil, fmt.Errorf("object too large")
	}
	data, err := io.ReadAll(io.LimitReader(out.Body, getMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > getMaxBytes {
		return nil, fmt.Errorf("object too large")
	}
	return data, nil
}

func (s *s3Store) ListKeys(prefix string) ([]string, error) {
	var keys []string
	marker := ""
	for {
		in := &s3.ListObjectsV2Input{
			Bucket:  aws.String(s.bucket),
			Prefix:  aws.String(prefix),
			MaxKeys: aws.Int32(1000),
		}
		if marker != "" {
			in.StartAfter = aws.String(marker)
		}
		// The interface deliberately takes no ctx; a hung startup load is
		// bounded by a 30s timeout on each ListObjectsV2 call instead.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		out, err := s.client.ListObjectsV2(ctx, in)
		cancel()
		if err != nil {
			return nil, err
		}
		for _, obj := range out.Contents {
			keys = append(keys, aws.ToString(obj.Key))
		}
		if !aws.ToBool(out.IsTruncated) || len(out.Contents) == 0 {
			return keys, nil
		}
		marker = aws.ToString(out.Contents[len(out.Contents)-1].Key)
	}
}

func isS3NotFound(err error) bool {
	var nsk *types.NoSuchKey
	if errors.As(err, &nsk) {
		return true
	}
	var api smithy.APIError
	if errors.As(err, &api) {
		switch api.ErrorCode() {
		case "NoSuchKey", "NotFound":
			return true
		}
	}
	return false
}

func (s *s3Store) Put(key string, r io.Reader, size int64) error {
	in := &s3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(key),
		Body:          r,
		ContentLength: aws.Int64(size),
	}
	// An explicit ContentType lets the signed preview URLs render inline;
	// without a known extension the SDK/server default applies.
	if ct := mime.TypeByExtension(filepath.Ext(key)); ct != "" {
		in.ContentType = aws.String(ct)
	}
	_, err := s.client.PutObject(context.Background(), in)
	return err
}

func (s *s3Store) SignGetURL(key string, ttl time.Duration) (string, error) {
	return s.signGet(key, ttl, attachmentDisposition(key))
}

// SignPreviewURL signs a GET URL without a Content-Disposition override so the
// browser renders the object inline (image/video/audio preview).
func (s *s3Store) SignPreviewURL(key string, ttl time.Duration) (string, error) {
	return s.signGet(key, ttl, "")
}

func (s *s3Store) signGet(key string, ttl time.Duration, disposition string) (string, error) {
	if ttl <= 0 {
		ttl = signURLTTL
	}
	presign := s3.NewPresignClient(s.client, func(o *s3.PresignOptions) {
		o.Expires = ttl
	})
	in := &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	}
	if disposition != "" {
		in.ResponseContentDisposition = aws.String(disposition)
	}
	out, err := presign.PresignGetObject(context.Background(), in)
	if err != nil {
		return "", err
	}
	return out.URL, nil
}

func attachmentDisposition(key string) string {
	name := path.Base(key)
	name = strings.Map(func(r rune) rune {
		switch r {
		case 0, '"', '\\', '\r', '\n':
			return -1
		default:
			return r
		}
	}, name)
	if name == "" || name == "." || name == "/" {
		name = "download"
	}
	return fmt.Sprintf(`attachment; filename="%s"; filename*=UTF-8''%s`, name, url.PathEscape(name))
}
