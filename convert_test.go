package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// outdirArg extracts the value of --outdir from a soffice command line.
func outdirArg(t *testing.T, args []string) string {
	t.Helper()
	for i, a := range args {
		if a == "--outdir" && i+1 < len(args) {
			return args[i+1]
		}
	}
	t.Fatal("no --outdir in soffice args")
	return ""
}

// fakePdftoppm writes n one-page jpegs at the pdftoppm output prefix (the
// last argument).
func fakePdftoppm(t *testing.T, args []string, n int) {
	t.Helper()
	prefix := args[len(args)-1]
	for i := 1; i <= n; i++ {
		require.NoError(t, os.WriteFile(fmt.Sprintf("%s-%d.jpg", prefix, i), []byte{0xff, 0xd8, 0xff, byte(i)}, 0o644))
	}
}

func TestSniffImageMIME(t *testing.T) {
	require.Equal(t, "image/png", sniffImageMIME(pngSig))
	require.Equal(t, "image/jpeg", sniffImageMIME([]byte{0xff, 0xd8, 0xff, 0xe0}))
	require.Equal(t, "image/gif", sniffImageMIME([]byte("GIF89a....")))
	require.Empty(t, sniffImageMIME([]byte("RIFF")))
}

func TestConvertFileToFullTextDocx(t *testing.T) {
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
		return nil, os.WriteFile(filepath.Join(outdirArg(t, args), "report.txt"), []byte("monthly invoice body"), 0o644)
	})

	text, err := convertFileToFullText(context.Background(), src)
	require.NoError(t, err)
	require.Equal(t, "monthly invoice body", text)
}

func TestConvertFileToFullTextUnavailable(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "report.docx")
	require.NoError(t, os.WriteFile(src, []byte("PK"), 0o644))
	stubConvert(t, noBins, func(ctx context.Context, name string, args ...string) ([]byte, error) {
		t.Fatal("runCmd should not be called")
		return nil, nil
	})
	_, err := convertFileToFullText(context.Background(), src)
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

	data, err := convertFileToLLMImage(context.Background(), src)
	require.NoError(t, err)
	require.Equal(t, []byte{0xff, 0xd8, 0xff, 0xd9}, data)
}

func TestConvertLargeJPEGUsesMagick(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "huge.jpg")
	raw := append([]byte{0xff, 0xd8, 0xff, 0xe0}, make([]byte, 64)...)
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

	data, err := convertFileToLLMImage(context.Background(), src)
	require.NoError(t, err)
	require.Equal(t, []byte{0xff, 0xd8, 0xff, 0xd9}, data)
	require.Equal(t, 1, magickCalls)
}

func TestConvertImageUnavailable(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "pic.webp")
	require.NoError(t, os.WriteFile(src, []byte("RIFF"), 0o644))
	stubConvert(t, noBins, func(ctx context.Context, name string, args ...string) ([]byte, error) {
		t.Fatal("runCmd should not be called")
		return nil, nil
	})
	_, err := convertFileToLLMImage(context.Background(), src)
	require.ErrorIs(t, err, errConvertUnavailable)
}

func TestConvertFileToPageImagesPDF(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "doc.pdf")
	require.NoError(t, os.WriteFile(src, []byte("%PDF-1.7 fake"), 0o644))

	stubConvert(t, func(name string) (string, error) {
		if name == "pdftoppm" {
			return "/usr/bin/pdftoppm", nil
		}
		return "", errConvertUnavailable
	}, func(ctx context.Context, name string, args ...string) ([]byte, error) {
		require.Contains(t, name, "pdftoppm")
		require.Contains(t, args, "-jpeg")
		fakePdftoppm(t, args, 3)
		return nil, nil
	})

	pages, err := convertFileToPageImages(context.Background(), src)
	require.NoError(t, err)
	require.Len(t, pages, 3)
	require.Equal(t, []byte{0xff, 0xd8, 0xff, 0x1}, pages[0])
}

func TestConvertFileToPageImagesOffice(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "report.docx")
	require.NoError(t, os.WriteFile(src, []byte("PK\x03\x04binary"), 0o644))

	// Office documents render through a soffice PDF export, then pdftoppm.
	stubConvert(t, func(name string) (string, error) {
		switch name {
		case "soffice", "pdftoppm":
			return "/usr/bin/" + name, nil
		}
		return "", errConvertUnavailable
	}, func(ctx context.Context, name string, args ...string) ([]byte, error) {
		switch {
		case strings.Contains(name, "soffice"):
			require.Contains(t, args, "pdf")
			return nil, os.WriteFile(filepath.Join(outdirArg(t, args), "report.pdf"), []byte("%PDF-1.7"), 0o644)
		case strings.Contains(name, "pdftoppm"):
			fakePdftoppm(t, args, 2)
			return nil, nil
		}
		t.Fatalf("unexpected command %s", name)
		return nil, nil
	})

	pages, err := convertFileToPageImages(context.Background(), src)
	require.NoError(t, err)
	require.Len(t, pages, 2)
}

func TestConvertFileToPageImagesSofficePNGFallback(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "report.docx")
	require.NoError(t, os.WriteFile(src, []byte("PK"), 0o644))

	// No pdftoppm and no magick: the soffice png export is the last resort
	// and its single page passes through as png.
	stubConvert(t, func(name string) (string, error) {
		if name == "soffice" {
			return "/usr/bin/soffice", nil
		}
		return "", errConvertUnavailable
	}, func(ctx context.Context, name string, args ...string) ([]byte, error) {
		require.Contains(t, name, "soffice")
		return nil, os.WriteFile(filepath.Join(outdirArg(t, args), "report.png"), append(append([]byte{}, pngSig...), 'x'), 0o644)
	})

	pages, err := convertFileToPageImages(context.Background(), src)
	require.NoError(t, err)
	require.Len(t, pages, 1)
	require.Equal(t, "image/png", sniffImageMIME(pages[0]))
}

func TestConvertFileToPageImagesUnavailable(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "doc.pdf")
	require.NoError(t, os.WriteFile(src, []byte("%PDF"), 0o644))
	stubConvert(t, noBins, func(ctx context.Context, name string, args ...string) ([]byte, error) {
		t.Fatal("runCmd should not be called")
		return nil, nil
	})
	_, err := convertFileToPageImages(context.Background(), src)
	require.ErrorIs(t, err, errConvertUnavailable)
}

func TestConvertFileToFrameImages(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "clip.mp4")
	require.NoError(t, os.WriteFile(src, []byte("fake mp4"), 0o644))

	stubConvert(t, func(name string) (string, error) {
		if name == "ffmpeg" {
			return "/usr/bin/ffmpeg", nil
		}
		return "", errConvertUnavailable
	}, func(ctx context.Context, name string, args ...string) ([]byte, error) {
		require.Contains(t, name, "ffmpeg")
		require.Contains(t, args, "fps=1/5,scale=1280:-2")
		pattern := args[len(args)-1]
		for i := 1; i <= 2; i++ {
			require.NoError(t, os.WriteFile(fmt.Sprintf(pattern, i), []byte{0xff, 0xd8, 0xff, byte(i)}, 0o644))
		}
		return nil, nil
	})

	frames, err := convertFileToFrameImages(context.Background(), src)
	require.NoError(t, err)
	require.Len(t, frames, 2)
}

func TestConvertFileToFrameImagesUnavailable(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "clip.mp4")
	require.NoError(t, os.WriteFile(src, []byte("fake"), 0o644))
	stubConvert(t, noBins, func(ctx context.Context, name string, args ...string) ([]byte, error) {
		t.Fatal("runCmd should not be called")
		return nil, nil
	})
	_, err := convertFileToFrameImages(context.Background(), src)
	require.ErrorIs(t, err, errConvertUnavailable)
}

func TestConvertToTextFileCached(t *testing.T) {
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
		return nil, os.WriteFile(filepath.Join(outdirArg(t, args), "report.txt"), []byte("monthly invoice body"), 0o644)
	})

	p, err := convertToTextFile(context.Background(), dir, src)
	require.NoError(t, err)
	require.Equal(t, ".txt", filepath.Ext(p))
	data, err := os.ReadFile(p)
	require.NoError(t, err)
	require.Equal(t, "monthly invoice body", string(data))
	require.Equal(t, 1, calls)

	// Second call hits the content-hash cache: no converter runs.
	stubConvert(t, noBins, func(ctx context.Context, name string, args ...string) ([]byte, error) {
		t.Fatal("runCmd should not be called")
		return nil, nil
	})
	p2, err := convertToTextFile(context.Background(), dir, src)
	require.NoError(t, err)
	require.Equal(t, p, p2)
	require.Equal(t, 1, calls)
}

func TestConvertToTextFileEmptyNotCached(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "scan.pdf")
	require.NoError(t, os.WriteFile(src, []byte("%PDF"), 0o644))

	// A scanned PDF yields no text; that is an error, not a cache entry.
	stubConvert(t, func(name string) (string, error) {
		if name == "pdftotext" {
			return "/usr/bin/pdftotext", nil
		}
		return "", errConvertUnavailable
	}, func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return []byte("  \n "), nil
	})
	_, err := convertToTextFile(context.Background(), dir, src)
	require.Error(t, err)
}

func TestConvertToImageFileCached(t *testing.T) {
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

	p, err := convertToImageFile(context.Background(), dir, src)
	require.NoError(t, err)
	require.Equal(t, ".jpg", filepath.Ext(p))
	require.Equal(t, 1, calls)

	// Second call hits the content-hash cache: no converter runs.
	stubConvert(t, noBins, func(ctx context.Context, name string, args ...string) ([]byte, error) {
		t.Fatal("runCmd should not be called")
		return nil, nil
	})
	p2, err := convertToImageFile(context.Background(), dir, src)
	require.NoError(t, err)
	require.Equal(t, p, p2)
	require.Equal(t, 1, calls)
}

func TestConvertToPageFilesCached(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "doc.pdf")
	require.NoError(t, os.WriteFile(src, []byte("%PDF-1.7 fake"), 0o644))

	calls := 0
	stubConvert(t, func(name string) (string, error) {
		if name == "pdftoppm" {
			return "/usr/bin/pdftoppm", nil
		}
		return "", errConvertUnavailable
	}, func(ctx context.Context, name string, args ...string) ([]byte, error) {
		calls++
		fakePdftoppm(t, args, 2)
		return nil, nil
	})

	pages, err := convertToPageFiles(context.Background(), dir, src)
	require.NoError(t, err)
	require.Len(t, pages, 2)
	require.Contains(t, filepath.Base(pages[0]), ".p01.jpg")
	require.Contains(t, filepath.Base(pages[1]), ".p02.jpg")
	require.Equal(t, 1, calls)

	// Second call enumerates the multi-file cache by glob: no converter runs.
	stubConvert(t, noBins, func(ctx context.Context, name string, args ...string) ([]byte, error) {
		t.Fatal("runCmd should not be called")
		return nil, nil
	})
	pages2, err := convertToPageFiles(context.Background(), dir, src)
	require.NoError(t, err)
	require.Equal(t, pages, pages2)
	require.Equal(t, 1, calls)
}

func TestConvertCacheGetPathAndList(t *testing.T) {
	dir := t.TempDir()

	_, ok := convertCacheGetPath(dir, "deadbeef.txt")
	require.False(t, ok)
	convertCachePut(dir, "deadbeef", ".txt", []byte("cached"))
	p, ok := convertCacheGetPath(dir, "deadbeef.txt")
	require.True(t, ok)
	data, err := os.ReadFile(p)
	require.NoError(t, err)
	require.Equal(t, "cached", string(data))

	// Multi-file products enumerate by glob, suffix families stay separate.
	convertCachePut(dir, "deadbeef", ".p01.jpg", []byte("p1"))
	convertCachePut(dir, "deadbeef", ".p02.jpg", []byte("p2"))
	convertCachePut(dir, "deadbeef", ".f1.jpg", []byte("f1"))
	pages, ok := convertCacheList(dir, "deadbeef", ".p*.jpg")
	require.True(t, ok)
	require.Len(t, pages, 2)
	frames, ok := convertCacheList(dir, "deadbeef", ".f*.jpg")
	require.True(t, ok)
	require.Len(t, frames, 1)
	_, ok = convertCacheList(dir, "deadbeef", ".x*.jpg")
	require.False(t, ok)

	// Empty data is never cached.
	convertCachePut(dir, "empty", ".txt", nil)
	_, ok = convertCacheGetPath(dir, "empty.txt")
	require.False(t, ok)
}

func TestConvertCacheTTL(t *testing.T) {
	dir := t.TempDir()
	convertCachePut(dir, "old", ".txt", []byte("cached"))
	convertCachePut(dir, "old", ".p01.jpg", []byte("p1"))
	p, ok := convertCacheGetPath(dir, "old.txt")
	require.True(t, ok)

	// Backdate beyond the TTL: single and multi-product lookups both miss,
	// and the prune removes the stale files.
	stale := time.Now().Add(-convertCacheTTL - time.Hour)
	require.NoError(t, os.Chtimes(p, stale, stale))
	pages, ok := convertCacheList(dir, "old", ".p*.jpg")
	require.True(t, ok)
	require.NoError(t, os.Chtimes(pages[0], stale, stale))
	_, ok = convertCacheGetPath(dir, "old.txt")
	require.False(t, ok)
	_, ok = convertCacheList(dir, "old", ".p*.jpg")
	require.False(t, ok)
	pruneConvertCache(dir)
	entries, err := os.ReadDir(workspaceCachePath(dir))
	require.NoError(t, err)
	require.Empty(t, entries)
}
