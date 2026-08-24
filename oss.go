package main

import (
	"fmt"
	"io"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/aliyun/aliyun-oss-go-sdk/oss"
)

const (
	listPageSize  = 200
	signURLTTL    = 5 * time.Minute
	listDelimiter = "/"
)

type ObjectInfo struct {
	Key          string
	Size         int64
	LastModified time.Time
}

type ListPage struct {
	Prefixes    []string
	Objects     []ObjectInfo
	IsTruncated bool
	NextMarker  string
}

type ObjectStore interface {
	List(prefix, marker string) (ListPage, error)
	SignGetURL(key string, ttl time.Duration) (string, error)
	Put(key string, r io.Reader) error
}

type ossStore struct {
	bucket *oss.Bucket
}

func newOSSStore(cfg OSSConfig) (ObjectStore, error) {
	client, err := oss.New(cfg.Endpoint, cfg.AccessKeyID, cfg.AccessKeySecret)
	if err != nil {
		return nil, fmt.Errorf("oss client: %w", err)
	}
	bucket, err := client.Bucket(cfg.Bucket)
	if err != nil {
		return nil, fmt.Errorf("oss bucket: %w", err)
	}
	return &ossStore{bucket: bucket}, nil
}

func (s *ossStore) List(prefix, marker string) (ListPage, error) {
	opts := []oss.Option{
		oss.Delimiter(listDelimiter),
		oss.MaxKeys(listPageSize),
	}
	if prefix != "" {
		opts = append(opts, oss.Prefix(prefix))
	}
	if marker != "" {
		opts = append(opts, oss.Marker(marker))
	}
	res, err := s.bucket.ListObjects(opts...)
	if err != nil {
		return ListPage{}, err
	}
	page := ListPage{
		Prefixes:    res.CommonPrefixes,
		IsTruncated: res.IsTruncated,
		NextMarker:  res.NextMarker,
	}
	for _, obj := range res.Objects {
		if obj.Key == "" || obj.Key == prefix {
			continue
		}
		page.Objects = append(page.Objects, ObjectInfo{
			Key:          obj.Key,
			Size:         obj.Size,
			LastModified: obj.LastModified,
		})
	}
	return page, nil
}

func (s *ossStore) Put(key string, r io.Reader) error {
	return s.bucket.PutObject(key, r)
}

func (s *ossStore) SignGetURL(key string, ttl time.Duration) (string, error) {
	if ttl <= 0 {
		ttl = signURLTTL
	}
	return s.bucket.SignURL(key, oss.HTTPGet, int64(ttl.Seconds()), oss.ResponseContentDisposition(attachmentDisposition(key)))
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
