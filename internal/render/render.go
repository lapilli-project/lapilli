// Package render prints a report for people (text) or programs (JSON).
package render

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/lapilli-project/lapilli/internal/diagnose"
)

const width = 80

type style struct{ on bool }

func (s style) wrap(code, text string) string {
	if !s.on {
		return text
	}
	return "\x1b[" + code + "m" + text + "\x1b[0m"
}
func (s style) bold(t string) string   { return s.wrap("1", t) }
func (s style) dim(t string) string    { return s.wrap("2", t) }
func (s style) red(t string) string    { return s.wrap("31", t) }
func (s style) green(t string) string  { return s.wrap("32", t) }
func (s style) yellow(t string) string { return s.wrap("33", t) }
func (s style) cyan(t string) string   { return s.wrap("36", t) }

// Clean makes text from a log or a server safe to print on a terminal. Control
// characters (every escape sequence starts with one) and the Unicode format
// characters that reorder or hide text are shown as \x1b or \u202e instead.
// Newlines and tabs stay.
func Clean(s string) string {
	if utf8.ValidString(s) && strings.IndexFunc(s, unsafe) < 0 {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case !unsafe(r):
			b.WriteRune(r)
		case r < 0x100:
			fmt.Fprintf(&b, "\\x%02x", r)
		default:
			fmt.Fprintf(&b, "\\u%04x", r)
		}
	}
	return b.String()
}

func unsafe(r rune) bool {
	return r != '\n' && r != '\t' && (unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == '\u2028' || r == '\u2029')
}

// cleaned is a copy of r with every string passed through Clean.
func cleaned(r *diagnose.Report) *diagnose.Report {
	c := *r
	c.Headline = Clean(r.Headline)
	c.Facts = nil
	for _, f := range r.Facts {
		c.Facts = append(c.Facts, diagnose.Fact{Label: Clean(f.Label), Value: Clean(f.Value)})
	}
	c.Keys = nil
	for _, k := range r.Keys {
		c.Keys = append(c.Keys, diagnose.KeyRow{Name: Clean(k.Name), Type: Clean(k.Type), Fingerprint: Clean(k.Fingerprint), Source: Clean(k.Source), Result: Clean(k.Result)})
	}
	c.Why = nil
	for _, w := range r.Why {
		c.Why = append(c.Why, Clean(w))
	}
	c.Fix = nil
	for _, st := range r.Fix {
		c.Fix = append(c.Fix, diagnose.Step{Text: Clean(st.Text), Cmd: Clean(st.Cmd)})
	}
	c.Also = nil
	for _, n := range r.Also {
		c.Also = append(c.Also, diagnose.Note{Level: n.Level, Text: Clean(n.Text), Cmd: Clean(n.Cmd)})
	}
	return &c
}

// Text prints the report for a terminal.
func Text(w io.Writer, r *diagnose.Report, color bool) {
	r = cleaned(r)
	s := style{color}
	mark := s.red("✗")
	if r.OK {
		mark = s.green("✓")
	}
	for i, line := range wrap(r.Headline, width-2) {
		if i == 0 {
			fmt.Fprintf(w, "%s %s\n", mark, s.bold(line))
		} else {
			fmt.Fprintf(w, "  %s\n", s.bold(line))
		}
	}
	if len(r.Facts) > 0 || len(r.Keys) > 0 {
		fmt.Fprintln(w)
	}

	if len(r.Facts) > 0 {
		lw := 0
		for _, f := range r.Facts {
			lw = max(lw, len(f.Label))
		}
		for _, f := range r.Facts {
			fmt.Fprintf(w, "  %s  %s\n", s.dim(pad(f.Label, lw)), f.Value)
		}
	}
	if len(r.Keys) > 0 {
		if len(r.Facts) > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "  %s\n", s.dim("Keys, in the order ssh tried them:"))
		nw, tw := 0, 0
		for _, k := range r.Keys {
			nw = max(nw, utf8.RuneCountInString(k.Name))
			tw = max(tw, len(k.Type))
		}
		nw = min(nw, 34)
		for i, k := range r.Keys {
			res := k.Result
			switch {
			case strings.HasPrefix(res, "accepted") && !strings.Contains(res, ","):
				res = s.green(res)
			case res == "rejected" || strings.HasPrefix(res, "cut off") || strings.Contains(res, "failed") || strings.Contains(res, "can't"):
				res = s.red(res)
			default:
				res = s.yellow(res)
			}
			fmt.Fprintf(w, "    %d  %s  %s  %s  %s  %s\n", i+1, pad(trimLeft(k.Name, nw), nw), pad(k.Type, tw), pad(shortFP(k.Fingerprint), 18), pad(k.Source, 5), res)
		}
	}

	if len(r.Why) > 0 {
		fmt.Fprintf(w, "\n%s\n", s.bold("Why"))
		for _, p := range r.Why {
			for _, line := range wrap(p, width-2) {
				fmt.Fprintf(w, "  %s\n", line)
			}
		}
	}
	if len(r.Fix) > 0 {
		fmt.Fprintf(w, "\n%s\n", s.bold("Fix"))
		for i, st := range r.Fix {
			num := strconv.Itoa(i+1) + ". "
			for j, line := range wrap(st.Text, width-2-len(num)) {
				if j == 0 {
					fmt.Fprintf(w, "  %s%s\n", num, line)
				} else {
					fmt.Fprintf(w, "  %s%s\n", strings.Repeat(" ", len(num)), line)
				}
			}
			printCmd(w, s, st.Cmd, 2+len(num)+2)
		}
	}
	if len(r.Also) > 0 {
		fmt.Fprintf(w, "\n%s\n", s.bold("Also"))
		for _, n := range r.Also {
			sym := s.dim("·")
			if n.Level == "warn" {
				sym = s.yellow("!")
			}
			for j, line := range wrap(n.Text, width-4) {
				if j == 0 {
					fmt.Fprintf(w, "  %s %s\n", sym, line)
				} else {
					fmt.Fprintf(w, "    %s\n", line)
				}
			}
			printCmd(w, s, n.Cmd, 6)
		}
	}
}

func printCmd(w io.Writer, s style, cmd string, indent int) {
	if cmd == "" {
		return
	}
	for _, line := range strings.Split(cmd, "\n") {
		fmt.Fprintf(w, "%s%s\n", strings.Repeat(" ", indent), s.cyan(line))
	}
}

// JSON prints the report and the parsed trace.
func JSON(w io.Writer, r *diagnose.Report, trace any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(struct {
		*diagnose.Report
		Trace any `json:"trace,omitempty"`
	}{r, trace})
}

func shortFP(fp string) string {
	if len(fp) > 18 {
		return fp[:17] + "…"
	}
	return fp
}

func trimLeft(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return "…" + string(r[len(r)-n+1:])
}

func pad(s string, n int) string {
	if c := utf8.RuneCountInString(s); c < n {
		return s + strings.Repeat(" ", n-c)
	}
	return s
}

// wrap breaks text into lines of at most n characters, at spaces.
func wrap(text string, n int) []string {
	var lines []string
	var cur strings.Builder
	for _, word := range strings.Fields(text) {
		if cur.Len() > 0 && utf8.RuneCountInString(cur.String())+1+utf8.RuneCountInString(word) > n {
			lines = append(lines, cur.String())
			cur.Reset()
		}
		if cur.Len() > 0 {
			cur.WriteByte(' ')
		}
		cur.WriteString(word)
	}
	if cur.Len() > 0 {
		lines = append(lines, cur.String())
	}
	return lines
}
