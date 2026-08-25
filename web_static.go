package main

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

// static/dist holds the bundles built by the bun project in static/
// (`bun run build`); only .gitkeep is committed, so run the frontend build
// before compiling the binary.
//
//go:embed all:static/dist
var staticFS embed.FS

func staticDir() fs.FS {
	sub, err := fs.Sub(staticFS, "static/dist")
	if err != nil {
		panic(err)
	}
	return sub
}

var staticFiles = func() []string {
	entries, err := fs.ReadDir(staticDir(), ".")
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}()

// matchAsset finds "<name>-<hash>.js" (or a plain "<name>.js") among files.
func matchAsset(files []string, name string) string {
	plain := name + ".js"
	prefix := name + "-"
	for _, f := range files {
		if f == plain {
			return f
		}
		if strings.HasPrefix(f, prefix) && strings.HasSuffix(f, ".js") {
			return f
		}
	}
	return ""
}

// jsAsset resolves a bundle entry name ("upload") to its served path
// ("/static/upload-1a2b3c4d.js"). When the bundle has not been built it falls
// back to the unhashed name, which 404s until `bun run build` has run.
func jsAsset(name string) string {
	if match := matchAsset(staticFiles, name); match != "" {
		return "/static/" + match
	}
	return "/static/" + name + ".js"
}

// staticHandler serves the embedded bundles. Hashed names are immutable, so
// responses are cached aggressively (overriding the global no-store header).
func staticHandler() http.Handler {
	files := http.StripPrefix("/static/", http.FileServerFS(staticDir()))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		files.ServeHTTP(w, r)
	})
}
