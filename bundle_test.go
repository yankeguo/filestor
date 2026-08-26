package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNewUUIDv4(t *testing.T) {
	a, err := newUUIDv4()
	require.NoError(t, err)
	require.True(t, isUUIDv4(a), a)
	b, err := newUUIDv4()
	require.NoError(t, err)
	require.NotEqual(t, a, b)
}

func TestBundlePrefix(t *testing.T) {
	require.Equal(t, "content/55/0e/"+testBundleID1, bundlePrefix(testBundleID1))
	require.Equal(t, "content/55/0e/"+testBundleID1, bundlePrefix("550E8400-E29B-41D4-A716-446655440000"))
}

func TestIndexKey(t *testing.T) {
	require.Equal(t, "index/2026/2026-08.json", indexKey(2026, time.August))
}

func TestParseIndexKey(t *testing.T) {
	m, ok := parseIndexKey("index/2026/2026-08.json")
	require.True(t, ok)
	require.Equal(t, "2026-08", m)

	for _, k := range []string{
		"",
		"index/",
		"index/2026-08.json",
		"index/2026/2026-08",
		"index/2026/2026-13.json",
		"index/2026/2027-01.json",
		"index/2026/2026-08.json/extra",
		"content/55/0e/" + testBundleID1,
	} {
		_, ok := parseIndexKey(k)
		require.False(t, ok, k)
	}
}

func TestBundleIndexLoadAndAppend(t *testing.T) {
	store := &fakeStore{}
	idx := newBundleIndex()
	require.NoError(t, idx.load(store))
	require.Empty(t, idx.year(2026))

	meta := bundleMeta{ID: testBundleID1, Title: "weekly-report", Time: "2026-08-24T06:59"}
	require.NoError(t, idx.append(store, meta))
	require.Equal(t, []string{"index/2026/2026-08.json"}, store.putKeys())
	require.Equal(t, []bundleMeta{meta}, idx.year(2026))
	got, ok := idx.get(testBundleID1)
	require.True(t, ok)
	require.Equal(t, meta, got)
	// Lookup is case-insensitive.
	_, ok = idx.get(strings.ToUpper(testBundleID1))
	require.True(t, ok)

	// A fresh index loads what append wrote to the bucket.
	idx2 := newBundleIndex()
	require.NoError(t, idx2.load(store))
	require.Equal(t, []bundleMeta{meta}, idx2.year(2026))

	meta2 := bundleMeta{ID: testBundleID2, Title: "next", Time: "2026-08-25T08:00"}
	require.NoError(t, idx.append(store, meta2))
	require.Equal(t, []bundleMeta{meta, meta2}, idx.year(2026))

	raw, err := store.Get("index/2026/2026-08.json")
	require.NoError(t, err)
	var listed []bundleMeta
	require.NoError(t, json.Unmarshal(raw, &listed))
	require.Equal(t, []bundleMeta{meta, meta2}, listed)
}

func TestBundleIndexAppendFailureKeepsMemory(t *testing.T) {
	store := &fakeStore{putErr: errors.New("boom")}
	idx := newBundleIndex()
	meta := bundleMeta{ID: testBundleID1, Title: "x", Time: "2026-08-24T06:59"}
	require.Error(t, idx.append(store, meta))
	require.Empty(t, idx.year(2026))
	_, ok := idx.get(testBundleID1)
	require.False(t, ok)
}

func TestBundleIndexLoadSkipsBadFiles(t *testing.T) {
	store := &fakeStore{objects: map[string][]byte{
		"index/2026/2026-08.json": mustIndexJSON(t,
			bundleMeta{ID: testBundleID3, Title: "a", Time: "2026-08-01T12:00"},
		),
		"index/2026/2026-09.json": []byte("not json"),
		"index/2026/notes.txt":    []byte("ignored"),
		"index/2026/2027-01.json": mustIndexJSON(t,
			bundleMeta{ID: testBundleID4, Title: "wrong year folder", Time: "2027-01-01T00:00"},
		),
	}}
	idx := newBundleIndex()
	require.NoError(t, idx.load(store))
	require.Equal(t, []bundleMeta{
		{ID: testBundleID3, Title: "a", Time: "2026-08-01T12:00"},
	}, idx.year(2026))
	require.Empty(t, idx.year(2027))

	// Only the key listing itself fails the load.
	broken := &fakeStore{err: errors.New("boom")}
	require.Error(t, newBundleIndex().load(broken))
}

func TestLoadBundleMeta(t *testing.T) {
	meta := bundleMeta{ID: testBundleID1, Title: "weekly-report", Time: "2026-08-24T06:59"}
	store := &fakeStore{objects: map[string][]byte{
		bundleMetaKey(testBundleID1): mustMetaJSON(t, meta),
	}}
	got, err := loadBundleMeta(store, testBundleID1)
	require.NoError(t, err)
	require.Equal(t, meta, got)

	_, err = loadBundleMeta(store, testBundleID2)
	require.ErrorIs(t, err, errNotFound)
}

func TestBundleIndexAppendRefusesBrokenMonth(t *testing.T) {
	store := &fakeStore{objects: map[string][]byte{
		"index/2026/2026-08.json": []byte("not json"),
	}}
	idx := newBundleIndex()
	require.NoError(t, idx.load(store))

	// The corrupt month failed to load: appending into it is refused instead
	// of rewriting the bucket file from the empty in-memory copy.
	err := idx.append(store, bundleMeta{ID: testBundleID1, Title: "x", Time: "2026-08-24T06:59"})
	require.Error(t, err)
	require.Empty(t, store.putKeys())

	// A healthy month still appends.
	require.NoError(t, idx.append(store, bundleMeta{ID: testBundleID2, Title: "y", Time: "2026-09-01T10:00"}))
	require.Equal(t, []string{"index/2026/2026-09.json"}, store.putKeys())
}
