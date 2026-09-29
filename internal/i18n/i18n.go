// Package i18n loads translation files and provides per-language lookup and
// formatting of dates, byte counts and rates.
//
// Each language is one flat JSON object in <lang>.json mapping dotted keys
// (for example "reminder.dialog.title") to strings. English ("en") is
// required and is the fallback for keys missing from other languages.
package i18n

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"sync"
)

// Default is the fallback language, which must always be present.
const Default = "en"

var (
	keyPattern  = regexp.MustCompile(`^[a-z0-9_]+(\.[a-z0-9_]+)+$`)
	langPattern = regexp.MustCompile(`^[a-z]{2,3}(-[A-Z]{2})?$`)
)

// Catalog holds the translations of all languages.
type Catalog struct {
	langs  map[string]map[string]string
	logger *slog.Logger

	mu       sync.Mutex
	reported map[string]bool // lang + "\x00" + key of missing keys already logged
}

// Load reads every <lang>.json file at the root of fsys.
func Load(fsys fs.FS, logger *slog.Logger) (*Catalog, error) {
	names, err := fs.Glob(fsys, "*.json")
	if err != nil {
		return nil, err
	}
	c := &Catalog{
		langs:    make(map[string]map[string]string, len(names)),
		logger:   logger,
		reported: make(map[string]bool),
	}
	for _, name := range names {
		lang := strings.TrimSuffix(name, ".json")
		if !langPattern.MatchString(lang) {
			return nil, fmt.Errorf("i18n: %s: invalid language code", name)
		}
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, err
		}
		msgs, err := parse(data)
		if err != nil {
			return nil, fmt.Errorf("i18n: %s: %w", name, err)
		}
		c.langs[lang] = msgs
	}
	if _, ok := c.langs[Default]; !ok {
		return nil, fmt.Errorf("i18n: %s.json is missing", Default)
	}
	return c, nil
}

// parse decodes a flat JSON object of string values, rejecting duplicate
// keys, malformed keys and non-string values.
func parse(data []byte) (map[string]string, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, errors.New("top level must be a JSON object")
	}
	msgs := make(map[string]string)
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key := tok.(string) // object keys are always strings
		if !keyPattern.MatchString(key) {
			return nil, fmt.Errorf("invalid key %q", key)
		}
		if _, dup := msgs[key]; dup {
			return nil, fmt.Errorf("duplicate key %q", key)
		}
		tok, err = dec.Token()
		if err != nil {
			return nil, err
		}
		val, ok := tok.(string)
		if !ok {
			return nil, fmt.Errorf("key %q: value must be a string", key)
		}
		msgs[key] = val
	}
	if _, err := dec.Token(); err != nil { // closing brace
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("trailing data after JSON object")
	}
	return msgs, nil
}

// Languages returns the available language codes in sorted order.
func (c *Catalog) Languages() []string {
	out := make([]string, 0, len(c.langs))
	for l := range c.langs {
		out = append(out, l)
	}
	slices.Sort(out)
	return out
}

// Has reports whether lang is available.
func (c *Catalog) Has(lang string) bool {
	_, ok := c.langs[lang]
	return ok
}

// Keys returns every key defined for the default language, sorted.
func (c *Catalog) Keys() []string {
	out := make([]string, 0, len(c.langs[Default]))
	for k := range c.langs[Default] {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// Translator returns a translator for lang, or for the default language if
// lang is not available.
func (c *Catalog) Translator(lang string) *Translator {
	if !c.Has(lang) {
		lang = Default
	}
	return &Translator{cat: c, lang: lang}
}

func (c *Catalog) reportMissing(lang, key string) {
	id := lang + "\x00" + key
	c.mu.Lock()
	seen := c.reported[id]
	c.reported[id] = true
	c.mu.Unlock()
	if !seen && c.logger != nil {
		c.logger.Warn("missing translation", "lang", lang, "key", key)
	}
}

// Translator looks up and formats text for one language.
type Translator struct {
	cat  *Catalog
	lang string
	// dates is the layout dates are written in; empty means
	// DefaultDateFormat.
	dates string
}

// Lang returns the translator's language code.
func (t *Translator) Lang() string { return t.lang }

// T returns the text for key, falling back to the default language. If the
// key is missing from the default language too, it logs once and returns
// the key itself.
func (t *Translator) T(key string) string {
	if s, ok := t.cat.langs[t.lang][key]; ok {
		return s
	}
	if t.lang != Default {
		t.cat.reportMissing(t.lang, key)
		if s, ok := t.cat.langs[Default][key]; ok {
			return s
		}
	}
	t.cat.reportMissing(Default, key)
	return key
}

// WithPrefix returns every key under prefix (for example "js.") with its
// text, including default-language keys missing from this language.
func (t *Translator) WithPrefix(prefix string) map[string]string {
	out := make(map[string]string)
	for k := range t.cat.langs[Default] {
		if strings.HasPrefix(k, prefix) {
			out[k] = t.T(k)
		}
	}
	return out
}
