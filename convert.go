package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// convertTimeout bounds one image conversion; documents and videos get
	// convertTimeoutLong per file (multi-page rendering is slower).
	convertTimeout     = 45 * time.Second
	convertTimeoutLong = 120 * time.Second

	// convertTextMaxBytes caps a converted full-text form on disk.
	convertTextMaxBytes = 4 << 20
	// analyzePageMaxBytes caps one rendered document page.
	analyzePageMaxBytes = 4 << 20
	// analyzeMaxPages / analyzeMaxFrames bound the page images rendered from
	// a document and the frames extracted from a video.
	analyzeMaxPages  = 6
	analyzeMaxFrames = 3
	// otherConvertMaxBytes bounds the best-effort text conversion attempted
	// on unknown binaries; larger files are judged by name only.
	otherConvertMaxBytes = 64 << 20
)

var errConvertUnavailable = errors.New("conversion tools not installed")

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
	// Point HOME at the temp dir (soffice writes a profile there) without
	// duplicating keys: an appended HOME= would be shadowed by the inherited
	// one, since getenv returns the first match.
	env := os.Environ()
	env = slices.DeleteFunc(env, func(kv string) bool {
		return strings.HasPrefix(kv, "HOME=") || strings.HasPrefix(kv, "SAL_USE_VCLPLUGIN=")
	})
	cmd.Env = append(env, "HOME="+os.TempDir(), "SAL_USE_VCLPLUGIN=svp")
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

func withConvertTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(ctx, d)
}

var imageExts = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true,
	".webp": true, ".heic": true, ".heif": true, ".bmp": true,
	".tif": true, ".tiff": true, ".svg": true, ".ico": true,
	".avif": true,
}

var documentExts = map[string]bool{
	".pdf": true,
	".doc": true, ".docx": true, ".odt": true, ".rtf": true,
	".ppt": true, ".pptx": true, ".odp": true,
	".xls": true, ".xlsx": true, ".ods": true,
}

var videoExts = map[string]bool{
	".mp4": true, ".mov": true, ".mkv": true, ".avi": true,
	".webm": true, ".m4v": true, ".mpg": true, ".mpeg": true,
	".wmv": true, ".flv": true, ".ts": true, ".3gp": true,
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

// nativeImageMIME returns the sniffed mime when the file is a browser- and
// model-native jpeg/png/gif within the image size cap, so it can be loaded
// straight from staging without conversion.
func nativeImageMIME(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	head := make([]byte, 16)
	n, _ := f.Read(head)
	mime := sniffImageMIME(head[:n])
	if mime == "" {
		return ""
	}
	info, err := f.Stat()
	if err != nil || info.Size() > analyzeImageMaxBytes {
		return ""
	}
	return mime
}

// capFullText truncates a converted full-text form at convertTextMaxBytes.
func capFullText(data []byte) string {
	if len(data) > convertTextMaxBytes {
		data = data[:convertTextMaxBytes]
	}
	return string(data)
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

// convertedFilePath returns the first regular file with the given extension
// under dir (e.g. the PDF soffice produced).
func convertedFilePath(dir, ext string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ext) {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if info, err := e.Info(); err == nil && info.Size() > 0 {
			return p, nil
		}
	}
	return "", errors.New("converter produced no output")
}

func sofficeConvert(ctx context.Context, src, outdir, format string) error {
	bin, err := findBin("soffice", "libreoffice")
	if err != nil {
		return errConvertUnavailable
	}
	if err := os.MkdirAll(outdir, 0o755); err != nil {
		return err
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

// convertFileToFullText converts a document (or, best-effort, any binary) to
// its full text form, capped at convertTextMaxBytes. An empty result means no
// readable text was found (e.g. a scanned, all-image document).
func convertFileToFullText(ctx context.Context, src string) (string, error) {
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
		return capFullText(data), nil
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
		return capFullText(out), nil
	}

	switch {
	case ext == ".pdf":
		if bin, err := findBin("pdftotext"); err == nil {
			out, err := runCmd(ctx, bin, "-layout", "-nopgbrk", "-enc", "UTF-8", src, "-")
			if err == nil {
				return capFullText(out), nil
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
	case documentExts[ext]:
		if ext == ".doc" {
			if bin, err := findBin("catdoc"); err == nil {
				out, err := runCmd(ctx, bin, "-w", src)
				if err == nil {
					return capFullText(out), nil
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

// convertFileToLLMImage normalizes any image to a jpeg within
// analyzeImageMaxBytes, stepping down size and quality until it fits; the
// soffice png export is the fallback for formats ImageMagick cannot read.
func convertFileToLLMImage(ctx context.Context, src string) ([]byte, error) {
	tmp, err := os.MkdirTemp("", "filestor-img-*")
	if err != nil {
		return nil, err
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
		return data, nil
	}

	pngDir := filepath.Join(tmp, "lo")
	if err := sofficeConvert(ctx, src, pngDir, "png"); err == nil {
		png, err := readConvertedOutput(pngDir, ".png")
		if err == nil {
			pngPath := filepath.Join(pngDir, "page.png")
			if err := os.WriteFile(pngPath, png, 0o644); err == nil {
				if data, err := tryMagick(pngPath); err == nil {
					return data, nil
				}
			}
		}
	}

	if _, err := magickBin(); err != nil {
		if _, err2 := findBin("soffice", "libreoffice"); err2 != nil {
			return nil, fmt.Errorf("%w (unsupported or oversized image)", errConvertUnavailable)
		}
	}
	return nil, errors.New("could not convert file to jpeg")
}

// renderPDFPages renders up to analyzeMaxPages pages of a PDF as images into
// a fresh subdirectory of tmp and returns their paths. pdftoppm is the
// primary path: the Debian image ships ImageMagick 6 with a policy that
// rejects PDFs (no ghostscript), so magick is only a fallback.
func renderPDFPages(ctx context.Context, pdf, tmp string) ([]string, error) {
	out := filepath.Join(tmp, "pages")
	if err := os.MkdirAll(out, 0o755); err != nil {
		return nil, err
	}
	if bin, err := findBin("pdftoppm"); err == nil {
		prefix := filepath.Join(out, "pg")
		if _, err := runCmd(ctx, bin, "-jpeg", "-r", "150", "-f", "1", "-l", strconv.Itoa(analyzeMaxPages), pdf, prefix); err == nil {
			if pages, _ := filepath.Glob(prefix + "-*.jpg"); len(pages) > 0 {
				return pages, nil
			}
		}
	}
	if bin, err := magickBin(); err == nil {
		// ImageMagick writes one file per page next to the target name.
		base := filepath.Join(out, "mg.jpg")
		if _, err := runCmd(ctx, bin, pdf+"[0-5]", "-auto-orient", "-resize", "1600x1600>", "-strip", "-quality", "80", base); err == nil {
			pages, _ := filepath.Glob(filepath.Join(out, "mg-*.jpg"))
			if len(pages) == 0 {
				if info, err := os.Stat(base); err == nil && info.Size() > 0 {
					pages = []string{base}
				}
			}
			if len(pages) > 0 {
				return pages, nil
			}
		}
	}
	return nil, errConvertUnavailable
}

// shrinkPageImage brings one rendered page under analyzePageMaxBytes.
// pdftoppm pages at 150 dpi are already small enough and pass through
// untouched; anything larger walks the magick step-down ladder.
func shrinkPageImage(ctx context.Context, path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) > 0 && int64(len(data)) <= analyzePageMaxBytes {
		return data, nil
	}
	for _, step := range []struct {
		edge, quality int
	}{{1600, 80}, {1024, 60}} {
		out := fmt.Sprintf("%s.s%d.jpg", path, step.edge)
		if err := magickConvert(ctx, path, out, step.edge, step.quality); err != nil {
			return nil, err
		}
		data, err := os.ReadFile(out)
		if err != nil {
			return nil, err
		}
		if len(data) > 0 && int64(len(data)) <= analyzePageMaxBytes {
			return data, nil
		}
	}
	return nil, fmt.Errorf("page image still exceeds %s", formatSize(analyzePageMaxBytes))
}

// convertFileToPageImages renders up to analyzeMaxPages page images of a
// document: PDFs go straight to renderPDFPages, office documents through a
// soffice PDF export first; the last resort is a single soffice png page.
func convertFileToPageImages(ctx context.Context, src string) ([][]byte, error) {
	tmp, err := os.MkdirTemp("", "filestor-pgs-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)

	var raw []string
	if strings.EqualFold(filepath.Ext(src), ".pdf") {
		raw, _ = renderPDFPages(ctx, src, tmp)
	}
	if len(raw) == 0 {
		// Office documents (and stubborn PDFs): export to PDF, then render.
		pdfDir := filepath.Join(tmp, "pdf")
		if err := sofficeConvert(ctx, src, pdfDir, "pdf"); err == nil {
			if pdf, err := convertedFilePath(pdfDir, ".pdf"); err == nil {
				raw, _ = renderPDFPages(ctx, pdf, tmp)
			}
		}
	}
	if len(raw) == 0 {
		// Last resort: a single soffice png page (may stay png; the mime is
		// sniffed again when the page is loaded for the model).
		pngDir := filepath.Join(tmp, "png")
		if err := sofficeConvert(ctx, src, pngDir, "png"); err == nil {
			if png, err := convertedFilePath(pngDir, ".png"); err == nil {
				raw = []string{png}
			}
		}
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("%w (cannot render pages of %s)", errConvertUnavailable, filepath.Ext(src))
	}
	var out [][]byte
	for _, p := range raw[:min(len(raw), analyzeMaxPages)] {
		data, err := shrinkPageImage(ctx, p)
		if err != nil {
			continue // drop unshrinkable pages, keep the rest
		}
		out = append(out, data)
	}
	if len(out) == 0 {
		return nil, errors.New("could not render any page image")
	}
	return out, nil
}

// convertFileToFrameImages extracts up to analyzeMaxFrames jpeg frames from
// a video with ffmpeg (one frame per ~5 seconds, 1280 px wide).
func convertFileToFrameImages(ctx context.Context, src string) ([][]byte, error) {
	bin, err := findBin("ffmpeg")
	if err != nil {
		return nil, errConvertUnavailable
	}
	tmp, err := os.MkdirTemp("", "filestor-frm-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)

	pattern := filepath.Join(tmp, "f%02d.jpg")
	if _, err := runCmd(ctx, bin, "-i", src, "-vf", "fps=1/5,scale=1280:-2", "-frames:v", strconv.Itoa(analyzeMaxFrames), pattern); err != nil {
		return nil, err
	}
	paths, _ := filepath.Glob(filepath.Join(tmp, "f*.jpg"))
	var out [][]byte
	for _, p := range paths[:min(len(paths), analyzeMaxFrames)] {
		data, err := os.ReadFile(p)
		if err != nil || len(data) == 0 || int64(len(data)) > analyzeImageMaxBytes {
			continue
		}
		out = append(out, data)
	}
	if len(out) == 0 {
		return nil, errors.New("no frames extracted")
	}
	return out, nil
}
