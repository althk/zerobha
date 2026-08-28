// Package webui carries the dashboard's static assets, embedded in the binary.
//
// They used to be served from http.Dir("web"), a relative path resolved against
// the process working directory. The container copies only the binary, the
// config and indices.csv, so /app/web never existed and every dashboard URL —
// including "/" — returned "404 page not found" while the /api endpoints
// answered normally. Same trap CLAUDE.md records for the log directory: a
// relative path resolves against WORKDIR, not against anything the deployment
// guarantees. Embedding removes the dependency on where the process was started.
package webui

import "embed"

//go:embed index.html app.js style.css
var Files embed.FS
