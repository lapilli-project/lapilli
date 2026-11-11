package content

// messages are what each check says, in English: lapilli serve's catalogs
// translate them.
var messages = map[string]string{
	NoText:          "no summary or description",
	NoRunbook:       "no runbook link",
	RunbookNotURL:   "a runbook link that isn't an http or https URL",
	TemplateError:   "a template that doesn't parse",
	NoSeverity:      "no severity",
	UnknownSeverity: "a severity not in --severities",
	Duplicate:       "the same condition as another rule",
	LabelNeverSeen:  "a label no firing had",
	NoisyNoFor:      "noisy, with no for",
	LabelNotGiven:   "a label its expression may not give",
	RareSeverity:    "a severity fewer than two rules use",
	ShortRange:      "a range shorter than two scrapes",
	AbsentNoFor:     "absent() with no for: a restart pages",

	NotifyNoText:         "a notification template shows an annotation the rule hasn't",
	NotifyLabelNeverSeen: "a notification template names a label no firing had",
	NotifyLabelNotGiven:  "a notification template names a label its expression may not give",
}

// whys are why each heuristic is only a guess.
var whys = map[string]string{
	LabelNotGiven: "read from its aggregation; it hasn't fired to tell",
	RareSeverity:  "it may be a typo, or a severity only it needs",
	ShortRange:    "the interval is its job's where the selector names one, and Prometheus's global one otherwise",
	AbsentNoFor:   "a series gone for a moment may be what it is there for",

	NotifyLabelNotGiven: "read from its aggregation; it hasn't fired to tell",
}

// Message is what a check says, in English.
func Message(check string) string { return messages[check] }

// Why is why a heuristic is only a guess, in English; "" for a check that
// isn't one.
func Why(check string) string { return whys[check] }
