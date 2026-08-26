package main

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
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

// bundleIndex is the in-memory copy of the monthly indexes
// (index/YYYY/YYYY-MM.json). The server is single-instance, so it loads every
// index file once at startup and is the only writer afterwards: a successful
// push appends here and rewrites the month file.
type bundleIndex struct {
	mu     sync.RWMutex
	months map[string][]bundleMeta // "2006-01" -> entries
	byID   map[string]bundleMeta   // lowercase uuid -> meta
}

func newBundleIndex() *bundleIndex {
	return &bundleIndex{months: map[string][]bundleMeta{}, byID: map[string]bundleMeta{}}
}

// parseIndexKey matches index/YYYY/YYYY-MM.json and returns the "YYYY-MM"
// month key; the year folder must match the file name.
func parseIndexKey(key string) (string, bool) {
	parts := strings.Split(key, "/")
	if len(parts) != 3 || parts[0] != indexRoot {
		return "", false
	}
	monthKey, ok := strings.CutSuffix(parts[2], ".json")
	if !ok || len(monthKey) != len("2006-01") || parts[1] != monthKey[:4] {
		return "", false
	}
	if _, err := time.Parse(browseMonthLayout, monthKey); err != nil {
		return "", false
	}
	return monthKey, true
}

// load fetches every monthly index file concurrently. A file that cannot be
// read or parsed is logged and skipped so one corrupt month does not empty
// the calendar; only the key listing itself can fail the load.
func (x *bundleIndex) load(store ObjectStore) error {
	keys, err := store.ListKeys(indexRoot + "/")
	if err != nil {
		return err
	}
	months := map[string][]bundleMeta{}
	byID := map[string]bundleMeta{}
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, key := range keys {
		monthKey, ok := parseIndexKey(key)
		if !ok {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			data, err := store.Get(key)
			if err != nil {
				log.Println("load bundle index:", key, err)
				return
			}
			var list []bundleMeta
			if err := json.Unmarshal(data, &list); err != nil {
				log.Println("parse bundle index:", key, err)
				return
			}
			mu.Lock()
			months[monthKey] = list
			for _, m := range list {
				byID[strings.ToLower(m.ID)] = m
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	x.mu.Lock()
	x.months = months
	x.byID = byID
	x.mu.Unlock()
	return nil
}

// get returns the indexed meta for a bundle id.
func (x *bundleIndex) get(id string) (bundleMeta, bool) {
	x.mu.RLock()
	defer x.mu.RUnlock()
	m, ok := x.byID[strings.ToLower(id)]
	return m, ok
}

// year returns every bundle of the year, oldest months first.
func (x *bundleIndex) year(year int) []bundleMeta {
	x.mu.RLock()
	defer x.mu.RUnlock()
	var out []bundleMeta
	for m := 1; m <= 12; m++ {
		out = append(out, x.months[fmt.Sprintf("%04d-%02d", year, m)]...)
	}
	return out
}

// append records a pushed bundle: the month file is rewritten first and the
// in-memory index is only updated once the write lands, so a failed Put
// leaves both untouched. Pushes are single-flight, so appends never race.
func (x *bundleIndex) append(store ObjectStore, meta bundleMeta) error {
	when, err := time.Parse(pushTimeLayout, meta.Time)
	if err != nil {
		return err
	}
	monthKey := when.Format(browseMonthLayout)
	x.mu.RLock()
	list := append(append([]bundleMeta(nil), x.months[monthKey]...), meta)
	x.mu.RUnlock()
	data, err := json.Marshal(list)
	if err != nil {
		return err
	}
	if err := store.Put(indexKey(when.Year(), when.Month()), bytes.NewReader(data), int64(len(data))); err != nil {
		return err
	}
	x.mu.Lock()
	x.months[monthKey] = list
	x.byID[strings.ToLower(meta.ID)] = meta
	x.mu.Unlock()
	return nil
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
