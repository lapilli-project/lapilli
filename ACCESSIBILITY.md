# Accessibility

Anyone on call should be able to judge their alerts with Lapilli: on a
screen reader, on the keyboard alone, at 200% zoom, on a phone, in any of
its languages. This says what that covers today, how it is checked, what
is known not to work yet, and how to tell us when something is in your
way.

## What it covers

The pages of `lapilli serve`: the overview, the rules, incidents,
proposals, settings and the rest. `lapilli verdict` prints text, or
`--json`, `--csv` and `--metrics` for other tools, and with `--compare`,
`--markdown` for a pull request. The docs at [lapilli.dev](https://lapilli.dev)
haven't had the same checks as serve's pages.

## What the pages aim for

[WCAG 2.2](https://www.w3.org/TR/WCAG22/) level AA. That is the target the
pages are built and checked against, not a claim that they conform: no one
who uses a screen reader day to day has reviewed them yet.

- No script and no animation: every page is HTML and CSS, and works
  without JavaScript.
- A skip link to the main content, landmarks, one h1, headings in order,
  and a name for every control.
- Every chart's numbers are in text too: in a table below it, or for a
  sparkline, in its label. A state is never told by colour alone.
- Text at 4.5:1 contrast or more; field borders, link underlines and the
  focus ring at 3:1; light and dark.
- Nothing scrolls sideways at 320 CSS pixels, which is 200% zoom on a 640px
  window.
- Each page says its language, and the language picker names each language
  in its own.

## How they are checked

- In CI, on every change: `go test ./internal/serve` parses every page in
  every language and holds its structure (`TestPagesAccessible`,
  `TestPagesAccessibleInMoreStates`), the colours' contrast
  (`TestContrast`), each state to more than a colour (`TestStatusKinds`),
  the overview's and outcomes' charts against their tables
  (`TestChartsInTables`), and the stylesheet's focus ring and lack of
  motion (`TestCSSAccessible`).
- By hand: [`test/a11y/axe.sh`](test/a11y/axe.sh) runs axe-core over every
  page in a headless Chromium, in each language, light and dark, with the
  WCAG A and AA rules, and checks for sideways scrolling at 320px. It needs
  Docker and pulls a browser image, so it is run by hand; the Go tests above
  hold what it found.

[docs/design/access.md](docs/design/access.md) says what the first review
found, and what it fixed.

## Known not to work yet

- The overview's heatmap cuts a long rule name short; the whole name is in
  its link and on the rule's page ([#129](https://github.com/lapilli-project/lapilli/issues/129)).
- The Spanish and Japanese were written with a machine's help and want a
  native speaker's review ([#131](https://github.com/lapilli-project/lapilli/issues/131),
  [#130](https://github.com/lapilli-project/lapilli/issues/130)).
- The pages are checked in Chromium, by tools; not yet with VoiceOver, NVDA,
  JAWS or TalkBack, nor in Firefox or Safari.

## Telling us about a barrier

Open an issue with the form
[An accessibility barrier](https://github.com/lapilli-project/lapilli/issues/new?template=accessibility.yml). Say
what you were trying to do, on which page, and what got in the way; your
browser and any assistive technology help. A screenshot is welcome but
never needed, and you never need to say anything about yourself. A barrier
is a bug, not a feature request.

## Changing the pages

A change to the pages keeps `go test ./internal/serve` passing. With
Docker, run `test/a11y/axe.sh` too, and go through what you changed with
the keyboard alone (Tab, Shift+Tab, Enter, Space): every control reached,
the focus ring always in sight. Fixes are HTML and CSS: the UI has no
script. [CONTRIBUTING.md](CONTRIBUTING.md#the-ui-and-its-languages) has the
rest.

## Who looks after it

The maintainer in [MAINTAINERS.md](MAINTAINERS.md), as part of
`lapilli serve`. Someone who would look after accessibility as an area of
their own is welcome; [ROADMAP.md](ROADMAP.md) lists what's left.
