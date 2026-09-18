// Package web holds the single page served at "/". It is embedded in the
// binary, so deploying the service is one file with nothing beside it.
package web

import _ "embed"

//go:embed index.html
var Page string
