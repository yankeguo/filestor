package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

var pngSig = []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}

func stubConvert(t *testing.T, pathFn func(string) (string, error), runFn func(ctx context.Context, name string, args ...string) ([]byte, error)) {
	t.Helper()
	oldPath, oldRun := lookPath, runCmd
	lookPath = pathFn
	runCmd = runFn
	t.Cleanup(func() {
		lookPath = oldPath
		runCmd = oldRun
	})
}

func noBins(_ string) (string, error) {
	return "", errConvertUnavailable
}

func TestSniffImageMIME(t *testing.T) {
	require.Equal(t, "image/png", sniffImageMIME(pngSig))
	require.Equal(t, "image/jpeg", sniffImageMIME([]byte{0xff, 0xd8, 0xff, 0xe0}))
	require.Equal(t, "image/gif", sniffImageMIME([]byte("GIF89a....")))
	require.Empty(t, sniffImageMIME([]byte("RIFF")))
}

func TestConvertFileToTextDocx(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "report.docx")
	require.NoError(t, os.WriteFile(src, []byte("PK\x03\x04binary"), 0o644))

	stubConvert(t, func(name string) (string, error) {
		if name == "soffice" {
			return "/usr/bin/soffice", nil
		}
		return "", errConvertUnavailable
	}, func(ctx context.Context, name string, args ...string) ([]byte, error) {
		require.Contains(t, name, "soffice")
		outdir := ""
		for i, a := range args {
			if a == "--outdir" && i+1 < len(args) {
				outdir = args[i+1]
			}
		}
		require.NotEmpty(t, outdir)
		return nil, os.WriteFile(filepath.Join(outdir, "report.txt"), []byte("monthly invoice body"), 0o644)
	})

	text, err := convertFileToText(context.Background(), src)
	require.NoError(t, err)
	require.Equal(t, "monthly invoice body", text)
}

func TestConvertFileToTextUnavailable(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "report.docx")
	require.NoError(t, os.WriteFile(src, []byte("PK"), 0o644))
	stubConvert(t, noBins, func(ctx context.Context, name string, args ...string) ([]byte, error) {
		t.Fatal("runCmd should not be called")
		return nil, nil
	})
	_, err := convertFileToText(context.Background(), src)
	require.ErrorIs(t, err, errConvertUnavailable)
}

func TestConvertWebpToJPEG(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "pic.webp")
	require.NoError(t, os.WriteFile(src, []byte("RIFF....WEBP"), 0o644))

	stubConvert(t, func(name string) (string, error) {
		if name == "magick" {
			return "/usr/bin/magick", nil
		}
		return "", errConvertUnavailable
	}, func(ctx context.Context, name string, args ...string) ([]byte, error) {
		require.Contains(t, name, "magick")
		out := args[len(args)-1]
		return nil, os.WriteFile(out, []byte{0xff, 0xd8, 0xff, 0xd9}, 0o644)
	})

	mime, data, err := convertFileToLLMImage(context.Background(), src)
	require.NoError(t, err)
	require.Equal(t, "image/jpeg", mime)
	require.Equal(t, []byte{0xff, 0xd8, 0xff, 0xd9}, data)
}

func TestConvertLargeJPEGUsesMagick(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "huge.jpg")
	raw := append([]byte{0xff, 0xd8, 0xff, 0xe0}, bytes.Repeat([]byte{0x00}, 64)...)
	require.NoError(t, os.WriteFile(src, raw, 0o644))

	var magickCalls int
	stubConvert(t, func(name string) (string, error) {
		if name == "magick" {
			return "/usr/bin/magick", nil
		}
		return "", errConvertUnavailable
	}, func(ctx context.Context, name string, args ...string) ([]byte, error) {
		magickCalls++
		require.Contains(t, strings.Join(args, " "), "2048x2048>")
		out := args[len(args)-1]
		return nil, os.WriteFile(out, []byte{0xff, 0xd8, 0xff, 0xd9}, 0o644)
	})

	mime, data, err := convertFileToLLMImage(context.Background(), src)
	require.NoError(t, err)
	require.Equal(t, "image/jpeg", mime)
	require.Equal(t, []byte{0xff, 0xd8, 0xff, 0xd9}, data)
	require.Equal(t, 1, magickCalls)
}

func TestReadWorkspaceImageConvertsOversizedJPEG(t *testing.T) {
	dir := t.TempDir()
	// Bigger than 8 MiB so the native jpeg path is skipped.
	huge := make([]byte, suggestImageMaxBytes+1)
	huge[0], huge[1], huge[2] = 0xff, 0xd8, 0xff
	require.NoError(t, os.WriteFile(filepath.Join(dir, "big.jpg"), huge, 0o644))

	stubConvert(t, func(name string) (string, error) {
		if name == "magick" {
			return "/usr/bin/magick", nil
		}
		return "", errConvertUnavailable
	}, func(ctx context.Context, name string, args ...string) ([]byte, error) {
		out := args[len(args)-1]
		return nil, os.WriteFile(out, []byte{0xff, 0xd8, 0xff, 0xd9}, 0o644)
	})

	mime, data, err := readWorkspaceImage(context.Background(), dir, "big.jpg")
	require.NoError(t, err)
	require.Equal(t, "image/jpeg", mime)
	require.Equal(t, []byte{0xff, 0xd8, 0xff, 0xd9}, data)
}

func TestConvertImageUnavailable(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "pic.webp")
	require.NoError(t, os.WriteFile(src, []byte("RIFF"), 0o644))
	stubConvert(t, noBins, func(ctx context.Context, name string, args ...string) ([]byte, error) {
		t.Fatal("runCmd should not be called")
		return nil, nil
	})
	_, _, err := convertFileToLLMImage(context.Background(), src)
	require.ErrorIs(t, err, errConvertUnavailable)
}

func TestConvertFileToTextCached(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "report.docx")
	require.NoError(t, os.WriteFile(src, []byte("PK\x03\x04binary"), 0o644))

	calls := 0
	stubConvert(t, func(name string) (string, error) {
		if name == "soffice" {
			return "/usr/bin/soffice", nil
		}
		return "", errConvertUnavailable
	}, func(ctx context.Context, name string, args ...string) ([]byte, error) {
		calls++
		outdir := ""
		for i, a := range args {
			if a == "--outdir" && i+1 < len(args) {
				outdir = args[i+1]
			}
		}
		require.NotEmpty(t, outdir)
		return nil, os.WriteFile(filepath.Join(outdir, "report.txt"), []byte("monthly invoice body"), 0o644)
	})

	text, err := convertFileToTextCached(context.Background(), dir, src)
	require.NoError(t, err)
	require.Equal(t, "monthly invoice body", text)
	require.Equal(t, 1, calls)

	// Second call hits the content-hash cache: no converter runs.
	stubConvert(t, noBins, func(ctx context.Context, name string, args ...string) ([]byte, error) {
		t.Fatal("runCmd should not be called")
		return nil, nil
	})
	text, err = convertFileToTextCached(context.Background(), dir, src)
	require.NoError(t, err)
	require.Equal(t, "monthly invoice body", text)
	require.Equal(t, 1, calls)
}

func TestConvertFileToLLMImageCached(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "pic.webp")
	require.NoError(t, os.WriteFile(src, []byte("RIFF....WEBP"), 0o644))

	calls := 0
	stubConvert(t, func(name string) (string, error) {
		if name == "magick" {
			return "/usr/bin/magick", nil
		}
		return "", errConvertUnavailable
	}, func(ctx context.Context, name string, args ...string) ([]byte, error) {
		calls++
		out := args[len(args)-1]
		return nil, os.WriteFile(out, []byte{0xff, 0xd8, 0xff, 0xd9}, 0o644)
	})

	mime, data, err := convertFileToLLMImageCached(context.Background(), dir, src)
	require.NoError(t, err)
	require.Equal(t, "image/jpeg", mime)
	require.Equal(t, []byte{0xff, 0xd8, 0xff, 0xd9}, data)
	require.Equal(t, 1, calls)

	// Second call hits the content-hash cache: no converter runs.
	stubConvert(t, noBins, func(ctx context.Context, name string, args ...string) ([]byte, error) {
		t.Fatal("runCmd should not be called")
		return nil, nil
	})
	mime, data, err = convertFileToLLMImageCached(context.Background(), dir, src)
	require.NoError(t, err)
	require.Equal(t, "image/jpeg", mime)
	require.Equal(t, []byte{0xff, 0xd8, 0xff, 0xd9}, data)
	require.Equal(t, 1, calls)
}

func TestConvertCachePutAndGet(t *testing.T) {
	dir := t.TempDir()
	_, ok := convertCacheGet(dir, "deadbeef", ".txt")
	require.False(t, ok)
	convertCachePut(dir, "deadbeef", ".txt", []byte("cached"))
	data, ok := convertCacheGet(dir, "deadbeef", ".txt")
	require.True(t, ok)
	require.Equal(t, "cached", string(data))
	// Empty data is never cached.
	convertCachePut(dir, "empty", ".txt", nil)
	_, ok = convertCacheGet(dir, "empty", ".txt")
	require.False(t, ok)
}
