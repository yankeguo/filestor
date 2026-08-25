package main

import (
	"embed"
	"html/template"
)

//go:embed templates/*.html
var webFS embed.FS

var webTmpl = template.Must(template.New("").Funcs(template.FuncMap{
	"sub": func(a, b int) int { return a - b },
}).ParseFS(webFS, "templates/*.html"))
