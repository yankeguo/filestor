package main

import (
	"embed"
	"html/template"
)

//go:embed web/*.html
var webFS embed.FS

var webTmpl = template.Must(template.ParseFS(webFS, "web/*.html"))
