package web

import (
	"net/http"
	"net/http/httptest"
	"testing"

	webui "zerobha/web"
)

// The dashboard used to be served from http.Dir("web"), which resolves against
// the process working directory — so the container, whose WORKDIR is /app and
// which never copied the assets, returned "404 page not found" for every
// dashboard URL while /api answered normally.
//
// This test runs from internal/web/, where a relative "web" directory does not
// exist either, so it fails against the old http.Dir handler and passes against
// the embedded one.
func TestStaticAssetsAreServedRegardlessOfWorkingDirectory(t *testing.T) {
	fs := http.FileServer(http.FS(webui.Files))

	for _, path := range []string{"/", "/app.js", "/style.css"} {
		rec := httptest.NewRecorder()
		fs.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, rec.Code)
		}
		if rec.Body.Len() == 0 {
			t.Errorf("GET %s served an empty body", path)
		}
	}
}
