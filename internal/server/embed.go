package server

import "embed"

//go:embed templates/*.tmpl
var templatesFS embed.FS

//go:embed static/*
var staticFS embed.FS
