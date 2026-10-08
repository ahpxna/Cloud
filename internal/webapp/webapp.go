// Package webapp serves the family web app: sign-in, library, uploads and
// Shortcut key management. It is static and same-origin with the API.
package webapp

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed static
var static embed.FS

// BasePath is where the web app is mounted; "/" redirects here.
const BasePath = "/app/"

// contentSecurityPolicy allows only same-origin scripts and styles, images and
// video from the API (including blob: for downloads), and nothing framed.
const contentSecurityPolicy = "default-src 'none'; script-src 'self'; style-src 'self'; " +
	"img-src 'self' blob: data:; media-src 'self' blob:; connect-src 'self'; manifest-src 'self'; " +
	"base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// Handler serves the embedded files under BasePath.
func Handler() http.Handler {
	files, err := fs.Sub(static, "static")
	if err != nil {
		panic(err)
	}
	// Embedded files carry no modification time, so give each a content ETag;
	// with no-cache, phones revalidate with a cheap 304 on every launch.
	etags := make(map[string]string)
	if err := fs.WalkDir(files, ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		contents, err := fs.ReadFile(files, path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(contents)
		etags["/"+path] = `"` + hex.EncodeToString(sum[:12]) + `"`
		return nil
	}); err != nil {
		panic(err)
	}
	etags["/"] = etags["/index.html"]
	fileServer := http.StripPrefix(strings.TrimSuffix(BasePath, "/"), http.FileServer(http.FS(files)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Path == strings.TrimSuffix(BasePath, "/") {
			http.Redirect(w, r, BasePath, http.StatusMovedPermanently)
			return
		}
		header := w.Header()
		header.Set("Content-Security-Policy", contentSecurityPolicy)
		// Revalidate on every launch so a fixed app version reaches phones
		// immediately; the files are small.
		header.Set("Cache-Control", "no-cache")
		if etag, ok := etags[strings.TrimPrefix(r.URL.Path, strings.TrimSuffix(BasePath, "/"))]; ok {
			header.Set("ETag", etag)
		}
		if strings.HasSuffix(r.URL.Path, ".webmanifest") {
			header.Set("Content-Type", "application/manifest+json")
		}
		fileServer.ServeHTTP(w, r)
	})
}

// RedirectRoot sends "/" to the web app and 404s every other unknown path.
func RedirectRoot() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, BasePath, http.StatusFound)
	})
}
