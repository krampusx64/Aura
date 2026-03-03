package ui

import "embed"

//go:embed index.html config.html config_help.json shared.css *.png
var Content embed.FS
