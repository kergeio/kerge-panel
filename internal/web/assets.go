package web

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"path"
)

// FuncNames lists the template functions, for tooling that parses templates
// without executing them.
var FuncNames = []string{"t", "msg", "lang", "asset", "bytes", "bitrate", "percent"}

// parseTemplates builds one template set per language so that "t" is bound
// to that language without cloning at request time.
func (s *Server) parseTemplates() (map[string]map[string]*template.Template, error) {
	files, err := fs.Sub(s.opts.Files, "templates")
	if err != nil {
		return nil, err
	}
	out := make(map[string]map[string]*template.Template)
	for _, lang := range s.opts.Catalog.Languages() {
		tr := s.opts.Catalog.Translator(lang)
		funcs := template.FuncMap{
			"t":        tr.T,
			"msg":      func(k msgKey) string { return tr.T(string(k)) },
			"lang":     tr.Lang,
			"asset":    s.assetURL,
			"bytes":    tr.Bytes,
			"byterate": tr.ByteRate,
			"percent":  tr.Percent,
		}
		out[lang] = make(map[string]*template.Template, len(pages))
		for _, p := range pages {
			tpl, err := template.New(p).Funcs(funcs).ParseFS(files, "layout.html", "partials.html", p)
			if err != nil {
				return nil, fmt.Errorf("web: parse %s: %w", p, err)
			}
			out[lang][p] = tpl
		}
	}
	return out, nil
}

// Static assets.

// hashAssets returns a short content hash for every static file, used as a
// cache-busting query parameter.
func hashAssets(static fs.FS) (map[string]string, error) {
	out := make(map[string]string)
	err := fs.WalkDir(static, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(static, name)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		out[name] = hex.EncodeToString(sum[:])[:16]
		return nil
	})
	return out, err
}

// assetURL returns the versioned URL of a static file. Unknown names are an
// error so that a typo fails at render time instead of producing a 404.
func (s *Server) assetURL(name string) (string, error) {
	h, ok := s.assets[name]
	if !ok {
		return "", fmt.Errorf("unknown asset %q", name)
	}
	return "/static/" + name + "?v=" + h, nil
}

// staticHandler serves files from static via http.FileServerFS. Directories
// are reported as not found, so there are no listings. Versioned requests
// (?v=) are cacheable indefinitely because the URL changes with the content.
func staticHandler(static fs.FS) http.Handler {
	files := http.FileServerFS(noDirFS{static})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("v") != "" {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	})
}

// faviconHandler serves the icon that browsers request at /favicon.ico on
// their own. Pages link the SVG icon as well; this file is for browsers
// that ask for the ICO regardless.
func faviconHandler(static fs.FS) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=86400")
		http.ServeFileFS(w, r, static, "favicon.ico")
	})
}

// noDirFS hides directories: opening one fails with fs.ErrNotExist.
type noDirFS struct{ fsys fs.FS }

func (n noDirFS) Open(name string) (fs.File, error) {
	f, err := n.fsys.Open(name)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if info.IsDir() {
		f.Close()
		return nil, &fs.PathError{Op: "open", Path: path.Clean(name), Err: fs.ErrNotExist}
	}
	return f, nil
}
