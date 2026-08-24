package main

import (
	"strconv"
	"strings"
	"time"
)

type crumb struct {
	Name   string
	Prefix string
}

type dirEntry struct {
	Name   string
	Prefix string
}

type fileEntry struct {
	Name         string
	Key          string
	Size         string
	LastModified string
}

type browseData struct {
	Nav        string
	Crumbs     []crumb
	Parent     string
	HasParent  bool
	Dirs       []dirEntry
	Files      []fileEntry
	NextMarker string
	Prefix     string
}

func normalizePrefix(p string) string {
	p = strings.TrimLeft(p, "/")
	if p == "" {
		return ""
	}
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return p
}

func parentPrefix(prefix string) (string, bool) {
	prefix = strings.TrimSuffix(prefix, "/")
	if prefix == "" {
		return "", false
	}
	i := strings.LastIndex(prefix, "/")
	if i < 0 {
		return "", true
	}
	return prefix[:i+1], true
}

func breadcrumbs(prefix string) []crumb {
	out := []crumb{{Name: "Home", Prefix: ""}}
	prefix = strings.Trim(prefix, "/")
	if prefix == "" {
		return out
	}
	acc := ""
	for _, part := range strings.Split(prefix, "/") {
		if part == "" {
			continue
		}
		acc += part + "/"
		out = append(out, crumb{Name: part, Prefix: acc})
	}
	return out
}

func entryName(full, prefix string) string {
	name := strings.TrimPrefix(full, prefix)
	return strings.TrimSuffix(name, "/")
}

func buildBrowseData(prefix string, page ListPage) browseData {
	prefix = normalizePrefix(prefix)
	parent, hasParent := parentPrefix(prefix)
	data := browseData{
		Nav:        "browse",
		Crumbs:     breadcrumbs(prefix),
		Parent:     parent,
		HasParent:  hasParent,
		Prefix:     prefix,
		NextMarker: page.NextMarker,
	}
	if page.IsTruncated && data.NextMarker == "" {
		data.NextMarker = ""
	}
	if !page.IsTruncated {
		data.NextMarker = ""
	}
	for _, p := range page.Prefixes {
		name := entryName(p, prefix)
		if name == "" {
			continue
		}
		data.Dirs = append(data.Dirs, dirEntry{Name: name, Prefix: p})
	}
	for _, obj := range page.Objects {
		name := entryName(obj.Key, prefix)
		if name == "" {
			continue
		}
		data.Files = append(data.Files, fileEntry{
			Name:         name,
			Key:          obj.Key,
			Size:         formatSize(obj.Size),
			LastModified: formatTime(obj.LastModified),
		})
	}
	return data
}

func formatSize(n int64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	v := float64(n) / float64(div)
	suffix := "KMGTPE"[exp]
	return strconv.FormatFloat(v, 'f', 1, 64) + " " + string(suffix) + "B"
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2006-01-02 15:04")
}
