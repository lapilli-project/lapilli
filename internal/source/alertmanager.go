package source

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"lapilli.dev/internal/content"
)

// Alertmanager's configuration, for the notification templates a rule is
// sent with (docs/design/content.md, "Alertmanager's templates"): from a
// file, with the template files it names, or from Mimir's or Cortex's
// configuration API. It only reads.

// MaxAlertmanagerConfig is the most an Alertmanager configuration may be,
// as a file or an answer, template files and all in Mimir's.
const MaxAlertmanagerConfig = 8 << 20

// AlertmanagerURL reports whether --alertmanager-config names Mimir's or
// Cortex's configuration API rather than a file.
func AlertmanagerURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

// CheckAlertmanagerURL refuses an --alertmanager-config URL that isn't an
// http or https URL with a host, or that has a user name, a query or an
// anchor.
func CheckAlertmanagerURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("--alertmanager-config %s: want a file, or Mimir's or Cortex's configuration API, such as http://mimir:8080/api/v1/alerts", raw)
	}
	return nil
}

// AlertmanagerFiles reads Alertmanager's configuration from a file, and
// the template files its templates: globs name, resolved against the
// file's directory as Alertmanager resolves them. An absolute glob outside
// that directory, as Alertmanager's own path (/etc/alertmanager/*.tmpl),
// is looked for by its last element beside the file, where a mounted
// Secret or ConfigMap puts the templates. No file outside the directory,
// after its links, is read; nor more than content.MaxTemplateFiles, each
// content.MaxTemplateFile at most. A file in the format of Mimir's,
// Cortex's and Amazon's (alertmanager_config, template_files) is read as
// they hold it, its templates beside it. What is passed over is said in
// passed, by the names the files have relative to the directory.
func AlertmanagerFiles(path string) (config []byte, files []content.TemplateFile, passed []string, err error) {
	config, err = readBounded(path, MaxAlertmanagerConfig)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("--alertmanager-config: %w", err)
	}
	if cfg, fs, ok, err := content.SplitDefinition(config); ok || err != nil {
		if err != nil {
			return nil, nil, nil, fmt.Errorf("--alertmanager-config: %w", err)
		}
		return cfg, fs, nil, nil
	}
	globs, err := content.TemplateGlobs(config)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("--alertmanager-config: %w", err)
	}
	dir, err := filepath.Abs(filepath.Dir(path))
	if err == nil {
		dir, err = filepath.EvalSymlinks(dir)
	}
	if err != nil {
		return nil, nil, nil, fmt.Errorf("--alertmanager-config: %w", err)
	}
	seen := map[string]bool{}
	total := 0
	for _, g := range globs {
		pattern := g
		if !filepath.IsAbs(g) {
			pattern = filepath.Join(dir, g)
		} else if !inDir(dir, filepath.Dir(filepath.Clean(g))) {
			pattern = filepath.Join(dir, filepath.Base(g))
		}
		matches, err := filepath.Glob(pattern)
		if err != nil {
			passed = append(passed, fmt.Sprintf("templates: %q isn't a glob", clip(g)))
			continue
		}
		if len(matches) == 0 {
			passed = append(passed, fmt.Sprintf("templates: %q names no file beside the configuration", clip(g)))
		}
		for _, m := range matches {
			real, err := filepath.EvalSymlinks(m)
			if err != nil || !inDir(dir, real) {
				passed = append(passed, fmt.Sprintf("templates: %q is outside the configuration's directory, and isn't read", clip(rel(dir, m))))
				continue
			}
			if st, err := os.Stat(real); err != nil || !st.Mode().IsRegular() || seen[real] {
				continue
			}
			seen[real] = true
			if len(files) >= content.MaxTemplateFiles {
				passed = append(passed, fmt.Sprintf("templates: more than %d files: those after aren't read", content.MaxTemplateFiles))
				return config, files, passed, nil
			}
			b, err := readBounded(real, content.MaxTemplateFile)
			if err != nil {
				// Said by its name beside the configuration, not where it is.
				var pe *fs.PathError
				if errors.As(err, &pe) {
					err = pe.Err
				}
				passed = append(passed, fmt.Sprintf("templates: %s: %s", clip(rel(dir, m)), clipErr(err)))
				continue
			}
			if total += len(b); total > content.MaxTemplateBytes {
				passed = append(passed, fmt.Sprintf("templates: more than %d MB in all: those after aren't read", content.MaxTemplateBytes>>20))
				return config, files, passed, nil
			}
			files = append(files, content.TemplateFile{Name: rel(dir, m), Text: b})
		}
	}
	return config, files, passed, nil
}

// inDir reports whether path is dir or under it.
func inDir(dir, path string) bool {
	r, err := filepath.Rel(dir, path)
	return err == nil && r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator)) && !filepath.IsAbs(r)
}

// rel is a path as it is said: relative to dir, never where on the disk.
func rel(dir, path string) string {
	if r, err := filepath.Rel(dir, path); err == nil {
		return filepath.ToSlash(r)
	}
	return filepath.Base(path)
}

func clip(s string) string {
	if len(s) > 200 {
		s = s[:200]
	}
	return strings.ToValidUTF8(s, "")
}

// readBounded reads a regular file of limit bytes at most; a larger one is
// an error.
func readBounded(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("larger than %d KB, not read", limit>>10)
	}
	return b, nil
}

// AlertmanagerAPI reads a Mimir's or a Cortex's Alertmanager configuration
// for its tenant (GET /api/v1/alerts): Alertmanager's configuration and
// every template file beside it, as Mimir loads them all. found is false
// when the tenant has none (404). The request goes to the URL's host only,
// follows no redirect, and reads MaxAlertmanagerConfig at most; the tenant
// is sent as X-Scope-OrgID, and the token, read from its file, as a
// bearer token.
func AlertmanagerAPI(ctx context.Context, raw, tenant string, c Creds, agent string, transport http.RoundTripper) (config []byte, files []content.TemplateFile, found bool, err error) {
	if err := CheckAlertmanagerURL(raw); err != nil {
		return nil, nil, false, err
	}
	h := http.Header{}
	h.Set("Accept", "application/yaml")
	if tenant != "" {
		h.Set("X-Scope-OrgID", tenant)
	}
	e := endpoint{base: raw, what: "the Alertmanager", flag: "--alertmanager-config", agent: agent, transport: transport}
	code, _, body, err := e.call(ctx, http.MethodGet, raw, nil, h, c, MaxAlertmanagerConfig)
	if err != nil {
		return nil, nil, false, err
	}
	switch {
	case code == http.StatusNotFound:
		return nil, nil, false, nil
	case code != http.StatusOK:
		return nil, nil, false, fmt.Errorf("the Alertmanager: HTTP %d: %s", code, answered(body, 300))
	}
	cfg, fs, ok, err := content.SplitDefinition(body)
	switch {
	case err != nil:
		return nil, nil, false, fmt.Errorf("the Alertmanager's configuration: %w", err)
	case !ok:
		return nil, nil, false, errors.New("the Alertmanager's answer has no alertmanager_config: not Mimir's or Cortex's configuration API")
	}
	return cfg, fs, true, nil
}
