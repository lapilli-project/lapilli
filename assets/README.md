# Brand assets

The Lapilli mark and wordmark, in the sizes this repository actually references. Nothing here is
decorative inventory: every file is used by something, and a file nothing references should be
deleted rather than kept.

| file | used by |
|---|---|
| `lapilli-lockup.png` | `README.md`, light |
| `lapilli-lockup-dark.png` | `README.md`, `prefers-color-scheme: dark` |
| `lapilli-icon.png` | `charts/lapilli/Chart.yaml`'s `icon`, which is what Artifact Hub shows |
| `favicon-32.png` | the site's tab icon |
| `apple-touch-icon.png` | the site, added to a home screen |
| `og.png` | the site's Open Graph card (1200×630) |

**One copy, two destinations.** `.github/workflows/pages.yml` copies this directory into `site/` at
deploy time, the same way it copies `spec/IEB-SPEC.md` into `/ieb/v1` — so the site and this
repository cannot drift. `README.md` references the files by their `raw.githubusercontent.com` URL
rather than by `lapilli.dev`, because the README is also rendered on crates.io, where a relative
path does not resolve, and because a domain can lapse while the repository is the repository.

**There is no vector master yet.** Every file here is a raster, derived from the full-size set the
maintainer keeps outside this repository and then resized and recompressed (`sips`, `pngquant`,
`oxipng`) to the sizes above. The next step for the mark is an SVG of the same shapes, which would
replace `lapilli-lockup*.png` and `lapilli-icon.png`; `Chart.yaml`'s `icon` and the site's
`<link rel="icon">` both accept SVG, so that swap is three references and no new plumbing.

**Replacing a file means checking three places**, because nothing here is referenced from only one:
`README.md` (by `raw.githubusercontent.com` URL, so a change is live as soon as it merges),
`charts/lapilli/Chart.yaml` (by `lapilli.dev` URL, so it is live after the next Pages deploy), and
`site/_layouts/default.html`. The table above is the index; keep it true.
