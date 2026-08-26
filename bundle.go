package main

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	contentRoot    = "content"
	indexRoot      = "index"
	bundleMetaName = ".meta.json"
)

// bundleMeta is stored as content/.../<uuid>/.meta.json and listed in the
// monthly index index/YYYY/YYYY-MM.json.
type bundleMeta struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Time  string `json:"time"` // YYYY-MM-DDTHH:mm wall clock
}

func newUUIDv4() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f'
}

// isUUIDv4 reports a canonical lowercase UUID v4 (8-4-4-4-12, version 4,
// RFC 4122 variant).
func isUUIDv4(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		switch i {
		case 8, 13, 18, 23:
			if s[i] != '-' {
				return false
			}
		default:
			if !isHex(s[i]) {
				return false
			}
		}
	}
	if s[14] != '4' {
		return false
	}
	switch s[19] {
	case '8', '9', 'a', 'b':
		return true
	default:
		return false
	}
}

// bundlePrefix is content/<id[:2]>/<id[2:4]>/<id> with no trailing slash.
func bundlePrefix(id string) string {
	id = strings.ToLower(id)
	if len(id) < 4 {
		return contentRoot + "/" + id
	}
	return contentRoot + "/" + id[:2] + "/" + id[2:4] + "/" + id
}

func bundleMetaKey(id string) string {
	return bundlePrefix(id) + "/" + bundleMetaName
}

// parseBundleID matches content/<aa>/<bb>/<uuid>/ (exactly 4 segments) and
// requires the shard folders to match the id.
func parseBundleID(prefix string) (id string, ok bool) {
	parts := strings.Split(strings.Trim(prefix, "/"), "/")
	if len(parts) != 4 || parts[0] != contentRoot {
		return "", false
	}
	id = strings.ToLower(parts[3])
	if !isUUIDv4(id) || parts[1] != id[:2] || parts[2] != id[2:4] {
		return "", false
	}
	return id, true
}

func indexKey(year int, month time.Month) string {
	return fmt.Sprintf("%s/%04d/%04d-%02d.json", indexRoot, year, year, int(month))
}

func indexKeyForTime(t string) (string, error) {
	when, err := time.Parse(pushTimeLayout, t)
	if err != nil {
		return "", err
	}
	return indexKey(when.Year(), when.Month()), nil
}

func loadMonthIndex(store ObjectStore, year int, month time.Month) ([]bundleMeta, error) {
	data, err := store.Get(indexKey(year, month))
	if errors.Is(err, errNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []bundleMeta
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func appendMonthIndex(store ObjectStore, meta bundleMeta) error {
	key, err := indexKeyForTime(meta.Time)
	if err != nil {
		return err
	}
	when, err := time.Parse(pushTimeLayout, meta.Time)
	if err != nil {
		return err
	}
	list, err := loadMonthIndex(store, when.Year(), when.Month())
	if err != nil {
		return err
	}
	list = append(list, meta)
	data, err := json.Marshal(list)
	if err != nil {
		return err
	}
	return store.Put(key, bytes.NewReader(data), int64(len(data)))
}

func loadBundleMeta(store ObjectStore, id string) (bundleMeta, error) {
	data, err := store.Get(bundleMetaKey(id))
	if err != nil {
		return bundleMeta{}, err
	}
	var meta bundleMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return bundleMeta{}, err
	}
	if strings.ToLower(meta.ID) != id || meta.Time == "" {
		return bundleMeta{}, fmt.Errorf("invalid bundle meta")
	}
	return meta, nil
}
