package main

import (
	"context"
	"fmt"
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
