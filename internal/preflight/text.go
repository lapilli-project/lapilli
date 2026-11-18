package preflight

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// estimateLabels are how the text names each estimate.
var estimateLabels = map[string]string{
	"estimate.hourly":    "hourly run",
	"estimate.backfill":  "backfill",
	"estimate.nights":    "nights",
	"estimate.beside":    "beside",
	"estimate.backtests": "backtests",
	"estimate.memory":    "memory",
}

// Text writes the report for a person: a line of counts, an item a line,
// then the estimates and what they leave out.
func (r *Report) Text(w io.Writer) error {
	ok, problems, unknown := r.Count()
	var b strings.Builder
	fmt.Fprintf(&b, "lapilli preflight: %s: %d ok, %s, %d unknown; %s asked", plural(len(r.Items), "item"), ok, plural(problems, "problem"), unknown,
		plural(r.Asked.Requests, "request"))
	if r.Asked.Refused > 0 {
		fmt.Fprintf(&b, ", %d refused before it was sent", r.Asked.Refused)
	}
	b.WriteString("\n")
	line := func(indent, head, text string) {
		const width = 100
		words := strings.Fields(text)
		cur := indent + head
		pad := strings.Repeat(" ", len(indent)+len(head))
		first := true
		for _, word := range words {
			if !first && len(cur)+1+len(word) > width {
				b.WriteString(strings.TrimRight(cur, " ") + "\n")
				cur = pad
				first = true
			}
			if first {
				cur += word
				first = false
			} else {
				cur += " " + word
			}
		}
		b.WriteString(strings.TrimRight(cur, " ") + "\n")
	}
	item := func(it Item, name string) {
		head := fmt.Sprintf("%-9s%-20s", it.State, name)
		line("", head, it.What)
		if it.Why != "" {
			line(strings.Repeat(" ", 29), "why: ", it.Why)
		}
		if it.Fix != "" {
			line(strings.Repeat(" ", 29), "fix: ", it.Fix)
		}
	}
	var estimates []Item
	for _, it := range r.Items {
		if _, ok := estimateLabels[it.ID]; ok {
			estimates = append(estimates, it)
			continue
		}
		item(it, it.ID)
	}
	if len(estimates) > 0 {
		b.WriteString("Estimates\n")
		for _, it := range estimates {
			item(it, estimateLabels[it.ID])
		}
	}
	if len(r.LeftOut) > 0 {
		line("", "Left out of the estimates: ", strings.Join(r.LeftOut, "; "))
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// JSON writes the report as JSON, redacted as the text is.
func (r *Report) JSON(w io.Writer) error {
	if r.Items == nil {
		r.Items = []Item{}
	}
	if r.LeftOut == nil {
		r.LeftOut = []string{}
	}
	if r.Asked.By == nil {
		r.Asked.By = map[string]int{}
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}
