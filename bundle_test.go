package main

import (
	"encoding/json"
	"errors"
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
	key, err := indexKeyForTime("2026-08-24T06:59")
	require.NoError(t, err)
	require.Equal(t, "index/2026/2026-08.json", key)
}

func TestLoadAndAppendMonthIndex(t *testing.T) {
	store := &fakeStore{}
	list, err := loadMonthIndex(store, 2026, time.August)
	require.NoError(t, err)
	require.Empty(t, list)

	meta := bundleMeta{ID: testBundleID1, Title: "weekly-report", Time: "2026-08-24T06:59"}
	require.NoError(t, appendMonthIndex(store, meta))
	require.Equal(t, []string{"index/2026/2026-08.json"}, store.putKeys())

	list, err = loadMonthIndex(store, 2026, time.August)
	require.NoError(t, err)
	require.Equal(t, []bundleMeta{meta}, list)

	meta2 := bundleMeta{ID: testBundleID2, Title: "next", Time: "2026-08-25T08:00"}
	require.NoError(t, appendMonthIndex(store, meta2))
	list, err = loadMonthIndex(store, 2026, time.August)
	require.NoError(t, err)
	require.Equal(t, []bundleMeta{meta, meta2}, list)

	raw, err := store.Get("index/2026/2026-08.json")
	require.NoError(t, err)
	var got []bundleMeta
	require.NoError(t, json.Unmarshal(raw, &got))
	require.Equal(t, list, got)
}

func TestLoadMonthIndexError(t *testing.T) {
	store := &fakeStore{err: errors.New("boom")}
	_, err := loadMonthIndex(store, 2026, time.August)
	require.Error(t, err)
	require.False(t, errors.Is(err, errNotFound))
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
