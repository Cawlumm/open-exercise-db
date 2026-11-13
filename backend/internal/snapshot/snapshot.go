// Package snapshot builds the static catalog published on the catalog branch.
//
// It reads only the public HTTP API, so the publishing workflow needs no
// database credentials. Output is deterministic: an unchanged catalog yields
// byte-identical files and no new manifest.
package snapshot

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// heartbeatAfter is how stale published_at may get before the manifest is
// rewritten with a fresh one, so consumers can tell a quiet catalog from a
// dead publisher.
const heartbeatAfter = 24 * time.Hour

const (
	UserAgent         = "open-exercise-db-snapshot/1 (+https://github.com/Cawlumm/open-exercise-db)"
	DefaultAPI        = "https://oedb-api-o4dqddih5q-uk.a.run.app"
	SchemaVersion     = 1
	maxRetryAfter     = 60 * time.Second
	maxTries          = 3
	pageSize          = 100
	maxPages          = 1000
	maxBody           = 64 << 20
	manifestRelPath   = "v1/manifest.json"
	requestTimeout    = 60 * time.Second
	attemptsPerBuild  = 2
	shrinkNumerator   = 1
	shrinkDenominator = 2
)

// Locale is one manifest entry.
type Locale struct {
	Code   string `json:"code"`
	File   string `json:"file"`
	Total  int    `json:"total"`
	SHA256 string `json:"sha256"`
}

// Manifest is v1/manifest.json.
type Manifest struct {
	SchemaVersion int      `json:"schema_version"`
	GeneratedAt   string   `json:"generated_at"`
	PublishedAt   string   `json:"published_at"`
	Source        string   `json:"source"`
	Locales       []Locale `json:"locales"`
}

// Config drives Build.
type Config struct {
	API             string
	Out             string
	ConfirmRemovals bool
	Client          *http.Client
	Now             func() time.Time
	Sleep           func(context.Context, time.Duration) error
	Log             io.Writer

	Requests int // count of HTTP requests issued, filled in by Build
}

// Result reports what Build did.
type Result struct {
	Changed     bool
	GeneratedAt string
	PublishedAt string
	Locales     []Locale
	Bytes       map[string]int
}

type validationError struct{ msg string }

func (e *validationError) Error() string { return e.msg }

func invalid(format string, a ...any) error {
	return &validationError{fmt.Sprintf(format, a...)}
}

type collection struct {
	code  string
	items []item
}

type item struct {
	id  string
	raw json.RawMessage
}

// Build collects the catalog, validates it and writes any changes under Out.
func Build(ctx context.Context, cfg *Config) (*Result, error) {
	cfg.API = strings.TrimRight(cfg.API, "/")
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: requestTimeout}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Sleep == nil {
		cfg.Sleep = sleepCtx
	}
	if cfg.Log == nil {
		cfg.Log = io.Discard
	}

	old, err := readManifest(cfg.Out)
	if err != nil {
		return nil, fmt.Errorf("existing manifest is unreadable, refusing to skip the shrink guard: %w", err)
	}

	var cols []collection
	for attempt := 1; attempt <= attemptsPerBuild; attempt++ {
		cols, err = collect(ctx, cfg)
		if err == nil {
			err = validate(cols)
		}
		var ve *validationError
		if err == nil || !errors.As(err, &ve) {
			break
		}
		fmt.Fprintf(cfg.Log, "validation failed (attempt %d/%d): %v\n", attempt, attemptsPerBuild, err)
	}
	if err != nil {
		return nil, err
	}

	if err := checkShrink(cfg, old, cols); err != nil {
		return nil, err
	}

	res := &Result{Bytes: map[string]int{}}
	files := map[string][]byte{}
	for _, c := range cols {
		body := encode(c.items)
		sum := sha256.Sum256(body)
		hexSum := hex.EncodeToString(sum[:])
		name := fmt.Sprintf("exercises.%s.%s.json", c.code, hexSum[:16])
		files[name] = body
		res.Bytes[name] = len(body)
		res.Locales = append(res.Locales, Locale{Code: c.code, File: name, Total: len(c.items), SHA256: hexSum})
	}

	now := cfg.Now().UTC()
	res.GeneratedAt = now.Format(time.RFC3339)
	res.PublishedAt = res.GeneratedAt
	if old != nil && sameShas(old.Locales, res.Locales) {
		res.GeneratedAt = old.GeneratedAt
		res.Locales = old.Locales
		pub, perr := time.Parse(time.RFC3339, old.PublishedAt)
		if perr == nil && now.Sub(pub) <= heartbeatAfter {
			res.PublishedAt = old.PublishedAt
			return res, nil
		}
		files = nil // data files already published
	}
	res.Changed = true
	m := Manifest{SchemaVersion: SchemaVersion, GeneratedAt: res.GeneratedAt, PublishedAt: res.PublishedAt, Source: cfg.API, Locales: res.Locales}
	mb, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode manifest: %w", err)
	}
	mb = append(mb, '\n')

	dir := filepath.Join(cfg.Out, "v1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create %s: %w", dir, err)
	}
	if err := writeAtomic(filepath.Join(cfg.Out, ".nojekyll"), nil); err != nil {
		return nil, err
	}
	for name, body := range files {
		p := filepath.Join(dir, name)
		if existing, err := os.ReadFile(p); err == nil && bytes.Equal(existing, body) {
			continue
		}
		if err := writeAtomic(p, body); err != nil {
			return nil, err
		}
	}
	if err := writeAtomic(filepath.Join(cfg.Out, manifestRelPath), mb); err != nil {
		return nil, err
	}
	return res, nil
}

func sameShas(a, b []Locale) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Code != b[i].Code || a[i].SHA256 != b[i].SHA256 {
			return false
		}
	}
	return true
}

func readManifest(out string) (*Manifest, error) {
	b, err := os.ReadFile(filepath.Join(out, manifestRelPath))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", manifestRelPath, err)
	}
	return &m, nil
}

func checkShrink(cfg *Config, old *Manifest, cols []collection) error {
	if old == nil || cfg.ConfirmRemovals {
		return nil
	}
	have := map[string]bool{}
	for _, c := range cols {
		have[c.code] = true
	}
	for _, l := range old.Locales {
		if !have[l.Code] {
			return fmt.Errorf("refusing to publish: locale %s is in the published manifest but not in this run (consumers purge dropped locales); re-run with -confirm-removals if this is intended", l.Code)
		}
		if l.Code == "en" && len(cols[0].items)*shrinkDenominator < l.Total*shrinkNumerator {
			return fmt.Errorf("refusing to publish: en has %d exercises, the published catalog has %d (less than half); re-run with -confirm-removals if this is intended", len(cols[0].items), l.Total)
		}
	}
	return nil
}

func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp for %s: %w", path, err)
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	if err := os.Chmod(name, 0o644); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("rename %s: %w", path, err)
	}
	return nil
}

func encode(items []item) []byte {
	var b bytes.Buffer
	b.WriteString(`{"exercises":[`)
	for i, it := range items {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(it.raw)
	}
	b.WriteString(`],"total":`)
	b.WriteString(strconv.Itoa(len(items)))
	b.WriteString("}\n")
	return b.Bytes()
}

func validate(cols []collection) error {
	if len(cols) == 0 || cols[0].code != "en" {
		return invalid("en locale missing")
	}
	enIDs := map[string]bool{}
	for _, c := range cols {
		if len(c.items) == 0 {
			return invalid("locale %s: zero exercises", c.code)
		}
		seen := map[string]bool{}
		for _, it := range c.items {
			if seen[it.id] {
				return invalid("locale %s: duplicate id %s", c.code, it.id)
			}
			seen[it.id] = true
		}
		if c.code == "en" {
			enIDs = seen
			continue
		}
		if len(seen) != len(enIDs) {
			return invalid("locale %s has %d ids, en has %d", c.code, len(seen), len(enIDs))
		}
		for id := range seen {
			if !enIDs[id] {
				return invalid("locale %s has id %s that en lacks", c.code, id)
			}
		}
	}
	return nil
}

type listBody struct {
	Exercises []json.RawMessage `json:"exercises"`
	Locales   []struct {
		Code string `json:"code"`
	} `json:"locales"`
	Total *int `json:"total"`
}

func collect(ctx context.Context, cfg *Config) ([]collection, error) {
	codes, err := fetchLocales(ctx, cfg)
	if err != nil {
		return nil, err
	}
	var cols []collection
	en, err := fetchExport(ctx, cfg)
	if err != nil {
		return nil, err
	}
	cols = append(cols, en)
	for _, code := range codes {
		if code == "en" {
			continue
		}
		c, err := fetchLocale(ctx, cfg, code)
		if err != nil {
			return nil, err
		}
		cols = append(cols, c)
	}
	return cols, nil
}

func fetchLocales(ctx context.Context, cfg *Config) ([]string, error) {
	set := map[string]bool{"en": true}
	got := 0
	for page := 1; page <= maxPages; page++ {
		var lb listBody
		if err := getJSON(ctx, cfg, fmt.Sprintf("/api/v1/meta/locales?per_page=50&page=%d", page), &lb); err != nil {
			return nil, err
		}
		for _, l := range lb.Locales {
			set[l.Code] = true
		}
		got += len(lb.Locales)
		if lb.Total == nil && len(lb.Locales) > 0 {
			return nil, fmt.Errorf("GET /api/v1/meta/locales: response has locales but no total")
		}
		if len(lb.Locales) == 0 || lb.Total == nil || got >= *lb.Total {
			break
		}
	}
	var others []string
	for c := range set {
		if c != "en" {
			others = append(others, c)
		}
	}
	sort.Strings(others)
	return append([]string{"en"}, others...), nil
}

func fetchExport(ctx context.Context, cfg *Config) (collection, error) {
	var lb listBody
	if err := getJSON(ctx, cfg, "/api/v1/exercises/export", &lb); err != nil {
		return collection{}, err
	}
	return toCollection("en", lb.Exercises, lb.Total)
}

func fetchLocale(ctx context.Context, cfg *Config, code string) (collection, error) {
	var all []json.RawMessage
	var total *int
	for page := 1; page <= maxPages; page++ {
		var lb listBody
		path := fmt.Sprintf("/api/v1/exercises?locale=%s&per_page=%d&page=%d", url.QueryEscape(code), pageSize, page)
		if err := getJSON(ctx, cfg, path, &lb); err != nil {
			return collection{}, err
		}
		total = lb.Total
		all = append(all, lb.Exercises...)
		if len(lb.Exercises) == 0 || (total != nil && len(all) >= *total) {
			break
		}
	}
	return toCollection(code, all, total)
}

func toCollection(code string, raws []json.RawMessage, total *int) (collection, error) {
	if total == nil {
		return collection{}, invalid("locale %s: response has no total", code)
	}
	if len(raws) != *total {
		return collection{}, invalid("locale %s: collected %d exercises, API reports total %d", code, len(raws), *total)
	}
	c := collection{code: code}
	for _, r := range raws {
		var head struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		}
		if err := json.Unmarshal(r, &head); err != nil || head.ID == "" {
			return collection{}, invalid("locale %s: exercise without a usable id", code)
		}
		if head.Status != "published" {
			return collection{}, invalid("locale %s: exercise %s has status %q", code, head.ID, head.Status)
		}
		var buf bytes.Buffer
		if err := json.Compact(&buf, r); err != nil {
			return collection{}, invalid("locale %s: exercise %s: %v", code, head.ID, err)
		}
		c.items = append(c.items, item{id: head.ID, raw: buf.Bytes()})
	}
	sort.Slice(c.items, func(i, j int) bool { return c.items[i].id < c.items[j].id })
	return c, nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

const transientBackoff = 2 * time.Second

// getJSON retries a 429 up to maxTries times (honouring Retry-After) and a 5xx
// or network error once after a short backoff.
func getJSON(ctx context.Context, cfg *Config, path string, dst any) error {
	rateLimited, transient := 0, 0
	for {
		status, hdr, body, err := doGet(ctx, cfg, path)
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if transient++; transient > 1 {
				return fmt.Errorf("GET %s: %w", path, err)
			}
			fmt.Fprintf(cfg.Log, "%v on %s, retrying\n", err, path)
			if err := cfg.Sleep(ctx, transientBackoff); err != nil {
				return err
			}
		case status == http.StatusTooManyRequests:
			wait := retryAfter(hdr.Get("Retry-After"), cfg.Now())
			if rateLimited++; rateLimited >= maxTries {
				return fmt.Errorf("GET %s: 429 after %d tries (Retry-After %s)", path, maxTries, wait)
			}
			fmt.Fprintf(cfg.Log, "429 on %s, waiting %s\n", path, wait)
			if err := cfg.Sleep(ctx, wait); err != nil {
				return err
			}
		case status >= 500:
			if transient++; transient > 1 {
				return fmt.Errorf("GET %s: HTTP %d", path, status)
			}
			fmt.Fprintf(cfg.Log, "HTTP %d on %s, retrying\n", status, path)
			if err := cfg.Sleep(ctx, transientBackoff); err != nil {
				return err
			}
		case status < 200 || status > 299:
			return fmt.Errorf("GET %s: HTTP %d", path, status)
		default:
			if err := json.Unmarshal(body, dst); err != nil {
				return fmt.Errorf("GET %s: decode: %w", path, err)
			}
			return nil
		}
	}
}

func doGet(ctx context.Context, cfg *Config, path string) (int, http.Header, []byte, error) {
	cctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, cfg.API+path, nil)
	if err != nil {
		return 0, nil, nil, err
	}
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept", "application/json")
	cfg.Requests++
	resp, err := cfg.Client.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return 0, nil, nil, fmt.Errorf("read body: %w", err)
	}
	return resp.StatusCode, resp.Header, body, nil
}

func retryAfter(h string, now time.Time) time.Duration {
	h = strings.TrimSpace(h)
	d := time.Second
	if secs, err := strconv.Atoi(h); err == nil {
		d = time.Duration(secs) * time.Second
	} else if t, err := http.ParseTime(h); err == nil {
		d = t.Sub(now)
	}
	if d < time.Second {
		d = time.Second
	}
	if d > maxRetryAfter {
		d = maxRetryAfter
	}
	return d
}
