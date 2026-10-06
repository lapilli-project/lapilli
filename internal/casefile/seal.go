package casefile

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	ManifestName   = "MANIFEST.json"
	ManifestFormat = "lapilli.dev/case/v0"
)

// Manifest records a digest per file, so a case that was edited after it was sealed is detectable.
// It is an integrity check, not a signature: it says the case is what was sealed, not who sealed it.
type Manifest struct {
	Format string            `json:"format"`
	Files  map[string]string `json:"files"`
	// Digest is the SHA-256 of the file list in `sha256sum` form — "<hex>  <path>\n", sorted by path —
	// so it can be recomputed with nothing but a shell.
	Digest string `json:"digest"`
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func listFiles(dir string) (map[string]string, error) {
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == ManifestName {
			return nil
		}
		sum, err := hashFile(p)
		files[rel] = sum
		return err
	})
	return files, err
}

func listDigest(files map[string]string) string {
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var b strings.Builder
	for _, p := range paths {
		fmt.Fprintf(&b, "%s  %s\n", files[p], p)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// Seal writes the manifest of a case directory and returns it.
func Seal(dir string) (*Manifest, error) {
	files, err := listFiles(dir)
	if err != nil {
		return nil, err
	}
	m := &Manifest{Format: ManifestFormat, Files: files, Digest: listDigest(files)}
	raw, err := json.MarshalIndent(m, "", " ")
	if err != nil {
		return nil, err
	}
	return m, os.WriteFile(filepath.Join(dir, ManifestName), append(raw, '\n'), 0o644)
}

// LoadManifest reads the manifest of a sealed case without checking it.
func LoadManifest(dir string) (*Manifest, error) {
	raw, err := os.ReadFile(filepath.Join(dir, ManifestName))
	if err != nil {
		return nil, err
	}
	var m Manifest
	return &m, json.Unmarshal(raw, &m)
}

// Verify returns what is wrong with a sealed case; an empty list means it is exactly what was sealed.
func Verify(dir string) ([]string, error) {
	raw, err := os.ReadFile(filepath.Join(dir, ManifestName))
	if os.IsNotExist(err) {
		return []string{ErrNotSealed.Error()}, nil
	}
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return []string{"manifest is not valid JSON: " + err.Error()}, nil
	}
	var problems []string
	if m.Format != ManifestFormat {
		problems = append(problems, fmt.Sprintf("unknown manifest format %q (this build reads %s)", m.Format, ManifestFormat))
	}
	present, err := listFiles(dir)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(m.Files))
	for p := range m.Files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		switch got, ok := present[p]; {
		case !ok:
			problems = append(problems, "missing: "+p)
		case got != m.Files[p]:
			problems = append(problems, "altered: "+p)
		}
	}
	extra := make([]string, 0)
	for p := range present {
		if _, ok := m.Files[p]; !ok {
			extra = append(extra, p)
		}
	}
	sort.Strings(extra)
	for _, p := range extra {
		problems = append(problems, "not in manifest: "+p)
	}
	if listDigest(m.Files) != m.Digest {
		problems = append(problems, "manifest digest does not match its file list")
	}
	return problems, nil
}
