package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	convertTimeout  = 45 * time.Second
	convertCacheTTL = 7 * 24 * time.Hour
)

var (
	errConvertUnavailable = errors.New("conversion tools not installed")
	errUseImageTool       = errors.New("not text; use read_file_as_image")
)

// lookPath and runCmd are vars so tests can stub converters without
// installing ImageMagick or LibreOffice.
var (
	lookPath  = exec.LookPath
	runCmd    = defaultRunCmd
	sofficeMu sync.Mutex
)

func defaultRunCmd(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	// soffice forks a grandchild that inherits the pipes; without WaitDelay
	// cmd.Run() would block on the pipe copy well past the ctx timeout.
	cmd.WaitDelay = 5 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Env = append(os.Environ(), "HOME="+os.TempDir(), "SAL_USE_VCLPLUGIN=svp")
	err := cmd.Run()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return stdout.Bytes(), fmt.Errorf("%s: %s", filepath.Base(name), msg)
	}
	return stdout.Bytes(), nil
}

func findBin(names ...string) (string, error) {
	var last error
	for _, n := range names {
		p, err := lookPath(n)
		if err == nil {
			return p, nil
		}
		last = err
	}
	if last == nil {
		last = errConvertUnavailable
	}
	return "", last
}

func withConvertTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(ctx, convertTimeout)
}

var imageAsTextExts = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true,
	".webp": true, ".heic": true, ".heif": true, ".bmp": true,
	".tif": true, ".tiff": true, ".svg": true, ".ico": true,
	".avif": true,
}

var forceTextConvertExts = map[string]bool{
	".pdf": true,
	".doc": true, ".docx": true, ".odt": true, ".rtf": true,
	".ppt": true, ".pptx": true, ".odp": true,
	".xls": true, ".xlsx": true, ".ods": true,
}

var sheetExts = map[string]bool{
	".xls": true, ".xlsx": true, ".ods": true,
}

func sniffImageMIME(head []byte) string {
	if len(head) >= 3 && head[0] == 0xff && head[1] == 0xd8 && head[2] == 0xff {
		return "image/jpeg"
	}
	if len(head) >= 8 && bytes.Equal(head[:8], []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}) {
		return "image/png"
	}
	if len(head) >= 6 && (bytes.HasPrefix(head, []byte("GIF87a")) || bytes.HasPrefix(head, []byte("GIF89a"))) {
		return "image/gif"
	}
	return ""
}

func capConvertedText(data []byte) string {
	truncated := len(data) > analyzeTextMaxBytes
	if truncated {
		data = data[:analyzeTextMaxBytes]
	}
	out := string(data)
	if strings.TrimSpace(out) == "" {
		return "(empty file)"
	}
	if truncated {
		out += "\n... (truncated)"
	}
	return out
}

func readConvertedOutput(dir, ext string) ([]byte, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if ext != "" && !strings.EqualFold(filepath.Ext(e.Name()), ext) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		if len(data) == 0 {
			continue
		}
		return data, nil
	}
	return nil, errors.New("converter produced no output")
}

func sofficeConvert(ctx context.Context, src, outdir, format string) error {
	bin, err := findBin("soffice", "libreoffice")
	if err != nil {
		return errConvertUnavailable
	}
	sofficeMu.Lock()
	defer sofficeMu.Unlock()
	_, err = runCmd(ctx, bin, "--headless", "--norestore", "--nologo", "--nolockcheck", "--convert-to", format, "--outdir", outdir, src)
	return err
}

func magickBin() (string, error) {
	return findBin("magick", "convert")
}

func magickConvert(ctx context.Context, src, dst string, maxEdge, quality int) error {
	bin, err := magickBin()
	if err != nil {
		return errConvertUnavailable
	}
	resize := fmt.Sprintf("%dx%d>", maxEdge, maxEdge)
	_, err = runCmd(ctx, bin, src+"[0]", "-auto-orient", "-resize", resize, "-strip", "-quality", fmt.Sprintf("%d", quality), dst)
	if err != nil {
		_, err = runCmd(ctx, bin, src, "-auto-orient", "-resize", resize, "-strip", "-quality", fmt.Sprintf("%d", quality), dst)
	}
	return err
}

func convertFileToText(ctx context.Context, src string) (string, error) {
	ctx, cancel := withConvertTimeout(ctx)
	defer cancel()
	ext := strings.ToLower(filepath.Ext(src))

	tmp, err := os.MkdirTemp("", "filestor-txt-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)

	trySoffice := func(format, outExt string) (string, error) {
		if err := sofficeConvert(ctx, src, tmp, format); err != nil {
			return "", err
		}
		data, err := readConvertedOutput(tmp, outExt)
		if err != nil {
			return "", err
		}
		return capConvertedText(data), nil
	}
	tryPandoc := func() (string, error) {
		bin, err := findBin("pandoc")
		if err != nil {
			return "", errConvertUnavailable
		}
		out, err := runCmd(ctx, bin, "-t", "plain", src)
		if err != nil {
			return "", err
		}
		return capConvertedText(out), nil
	}

	switch {
	case ext == ".pdf":
		if bin, err := findBin("pdftotext"); err == nil {
			out, err := runCmd(ctx, bin, "-layout", "-nopgbrk", "-enc", "UTF-8", src, "-")
			if err == nil {
				return capConvertedText(out), nil
			}
		}
		if text, err := trySoffice("txt:Text", ".txt"); err == nil {
			return text, nil
		}
	case sheetExts[ext]:
		if text, err := trySoffice("csv", ".csv"); err == nil {
			return text, nil
		}
		if text, err := trySoffice("txt:Text", ".txt"); err == nil {
			return text, nil
		}
	case forceTextConvertExts[ext]:
		if ext == ".doc" {
			if bin, err := findBin("catdoc"); err == nil {
				out, err := runCmd(ctx, bin, "-w", src)
				if err == nil {
					return capConvertedText(out), nil
				}
			}
		}
		if text, err := trySoffice("txt:Text", ".txt"); err == nil {
			return text, nil
		}
		if text, err := tryPandoc(); err == nil {
			return text, nil
		}
	default:
		if text, err := trySoffice("txt:Text", ".txt"); err == nil {
			return text, nil
		}
		if text, err := tryPandoc(); err == nil {
			return text, nil
		}
	}
	return "", fmt.Errorf("%w (cannot convert %s)", errConvertUnavailable, ext)
}

func convertFileToLLMImage(ctx context.Context, src string) (string, []byte, error) {
	ctx, cancel := withConvertTimeout(ctx)
	defer cancel()

	tmp, err := os.MkdirTemp("", "filestor-img-*")
	if err != nil {
		return "", nil, err
	}
	defer os.RemoveAll(tmp)

	tryMagick := func(in string) ([]byte, error) {
		for _, step := range []struct {
			edge, quality int
		}{{2048, 85}, {1280, 70}, {800, 50}} {
			out := filepath.Join(tmp, fmt.Sprintf("out-%d.jpg", step.edge))
			if err := magickConvert(ctx, in, out, step.edge, step.quality); err != nil {
				return nil, err
			}
			data, err := os.ReadFile(out)
			if err != nil {
				return nil, err
			}
			if int64(len(data)) <= analyzeImageMaxBytes && len(data) > 0 {
				return data, nil
			}
		}
		return nil, fmt.Errorf("converted image still exceeds %s", formatSize(analyzeImageMaxBytes))
	}

	if data, err := tryMagick(src); err == nil {
		return "image/jpeg", data, nil
	}

	pngDir := filepath.Join(tmp, "lo")
	if err := os.MkdirAll(pngDir, 0o755); err != nil {
		return "", nil, err
	}
	if err := sofficeConvert(ctx, src, pngDir, "png"); err == nil {
		png, err := readConvertedOutput(pngDir, ".png")
		if err == nil {
			pngPath := filepath.Join(pngDir, "page.png")
			if err := os.WriteFile(pngPath, png, 0o644); err == nil {
				if data, err := tryMagick(pngPath); err == nil {
					return "image/jpeg", data, nil
				}
			}
		}
	}

	if _, err := magickBin(); err != nil {
		if _, err2 := findBin("soffice", "libreoffice"); err2 != nil {
			return "", nil, fmt.Errorf("%w (unsupported or oversized image)", errConvertUnavailable)
		}
	}
	return "", nil, errors.New("could not convert file to jpeg/png/gif")
}

// hashFileSHA256 returns the hex content hash used as the conversion cache key.
func hashFileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// convertCacheGet returns the cached conversion output for a content hash.
// Entries older than convertCacheTTL count as a miss (left in place for
// pruneConvertCache to remove).
func convertCacheGet(dir, key, ext string) ([]byte, bool) {
	p := filepath.Join(workspaceCachePath(dir), key+ext)
	if info, err := os.Stat(p); err != nil || time.Since(info.ModTime()) > convertCacheTTL {
		return nil, false
	}
	data, err := os.ReadFile(p)
	if err != nil || len(data) == 0 {
		return nil, false
	}
	return data, true
}

// convertCachePut stores a conversion result atomically and prunes expired
// entries on the way out (best-effort).
func convertCachePut(dir, key, ext string, data []byte) {
	if len(data) == 0 {
		return
	}
	if err := os.MkdirAll(workspaceCachePath(dir), 0o755); err != nil {
		return
	}
	tmp, err := os.CreateTemp(workspaceCachePath(dir), "tmp-*")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return
	}
	if err := tmp.Close(); err != nil {
		return
	}
	if err := os.Rename(tmpName, filepath.Join(workspaceCachePath(dir), key+ext)); err != nil {
		return
	}
	pruneConvertCache(dir)
}

// pruneConvertCache removes cache entries older than convertCacheTTL.
func pruneConvertCache(dir string) {
	entries, err := os.ReadDir(workspaceCachePath(dir))
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-convertCacheTTL)
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() || !info.ModTime().Before(cutoff) {
			continue
		}
		_ = os.Remove(filepath.Join(workspaceCachePath(dir), e.Name()))
	}
}

// convertFileToTextCached converts like convertFileToText but memoizes the
// result under the workspace's .filestor/cache, keyed by the source content
// hash, so re-reading a staged file skips the external converters.
func convertFileToTextCached(ctx context.Context, dir, src string) (string, error) {
	key, err := hashFileSHA256(src)
	if err == nil {
		if data, ok := convertCacheGet(dir, key, ".txt"); ok {
			return string(data), nil
		}
	}
	text, err := convertFileToText(ctx, src)
	if err != nil {
		return "", err
	}
	if key != "" {
		convertCachePut(dir, key, ".txt", []byte(text))
	}
	return text, nil
}

// convertFileToLLMImageCached is the cached counterpart of
// convertFileToLLMImage; cached entries are always jpeg.
func convertFileToLLMImageCached(ctx context.Context, dir, src string) (string, []byte, error) {
	key, err := hashFileSHA256(src)
	if err == nil {
		if data, ok := convertCacheGet(dir, key, ".jpg"); ok {
			return "image/jpeg", data, nil
		}
	}
	mime, data, err := convertFileToLLMImage(ctx, src)
	if err != nil {
		return "", nil, err
	}
	if key != "" && mime == "image/jpeg" {
		convertCachePut(dir, key, ".jpg", data)
	}
	return mime, data, nil
}
