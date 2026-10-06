// lapilli-case freezes an incident into a case, replays it for an agent, and grades what the agent did.
//
// It is reached as `lapilli case …` once the two binaries are installed side by side, and works
// alone as `lapilli-case …`. The design is docs/design-case.md.
package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/lapilli-project/lapilli/internal/agent"
	"github.com/lapilli-project/lapilli/internal/casefile"
	"github.com/lapilli-project/lapilli/internal/freeze"
	"github.com/lapilli-project/lapilli/internal/grade"
	"github.com/lapilli-project/lapilli/internal/guard"
	"github.com/lapilli-project/lapilli/internal/metrics"
	"github.com/lapilli-project/lapilli/internal/replay"
)

// version is what --version prints. Nothing sets it yet: lapilli-case is in no release, and a build
// that is will pass -ldflags "-X main.version=…".
var version = "0.0.0-dev"

type command struct {
	name, args, help string
	run              func(ctx context.Context, args []string) (int, error)
}

var commands []command

func init() {
	commands = []command{
		{"verify", "<case>...", "check that a case is exactly what was sealed", cmdVerify},
		{"seal", "<case>", "(re)write a case's manifest", cmdSeal},
		{"freeze", "<case.yaml> -o <dir> --kubeconfig <file> [--metrics-url <url>] [--node-logs]", "freeze a live incident into a case", cmdFreeze},
		{"pack", "<case.yaml> --snapshot <dir> --freeze-time <unix seconds> -o <dir> [--metrics <file>]", "build a case from a snapshot that was already collected", cmdPack},
		{"export-metrics", "--url <prometheus> -o <file> [--at <unix seconds>] [--window 1h] [--match <selector>]...", "copy an incident window out of a Prometheus", cmdExportMetrics},
		{"serve", "<case>", "serve a case for manual investigation", cmdServe},
		{"run", "<case>... [--agent <name>] [--model <m>] [--runs 3] [-o results] [--pass-env <NAME>]...", "let an agent investigate one or more cases", cmdRun},
		{"packets", "<results> <case>... [-o packets.json] [--key key.json]", "write blind packets for an outcome judge", cmdPackets},
		{"report", "<results> [--verdicts <file> --key <file>] [--json]", "summarise runs: the outcome, where judged, beside the process checks", cmdReport},
		{"promq", "'<PromQL>' [--range 30m] [--step 15s]", "query the metrics endpoint in $PROM_URL (the helper an agent is given)", cmdPromq},
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, "lapilli-case %s — replayable incident cases for agents that investigate\n\n", version)
	for _, c := range commands {
		fmt.Fprintf(os.Stderr, "  lapilli-case %s %s\n      %s\n", c.name, c.args, c.help)
	}
}

func main() {
	if len(os.Args) < 2 || os.Args[1] == "-h" || os.Args[1] == "--help" || os.Args[1] == "help" {
		usage()
		os.Exit(2)
	}
	if os.Args[1] == "--version" || os.Args[1] == "version" {
		fmt.Println(version)
		return
	}
	if os.Args[1] == "guard-kubectl" { // not a command for people: bin/kubectl on an agent's PATH is this
		os.Exit(guardKubectl(os.Args[2:]))
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	for _, c := range commands {
		if c.name == os.Args[1] {
			code, err := c.run(ctx, os.Args[2:])
			if err != nil {
				fmt.Fprintln(os.Stderr, "lapilli-case "+c.name+":", err)
				if code == 0 {
					code = 1
				}
			}
			stop()
			os.Exit(code)
		}
	}
	fmt.Fprintf(os.Stderr, "lapilli-case: unknown command %q\n\n", os.Args[1])
	usage()
	os.Exit(2)
}

// parse reads flags that may come before, between or after the positional arguments, which the
// standard flag package does not do on its own. Everything after `--` is positional.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	fs.SetOutput(os.Stderr)
	var positional, rest []string
	for i, a := range args {
		if a == "--" {
			args, rest = args[:i], args[i+1:]
			break
		}
	}
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if args = fs.Args(); len(args) == 0 {
			return append(positional, rest...), nil
		}
		positional, args = append(positional, args[0]), args[1:]
	}
}

// guardKubectl is what bin/kubectl on an agent's PATH runs: <kubeconfig> <real kubectl> <the agent's
// arguments>. It becomes the real kubectl, pointed at the case, or refuses and says why.
func guardKubectl(args []string) int {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "lapilli-case guard-kubectl: not meant to be run by hand")
		return 2
	}
	pin, err := guard.LoadPin(args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "kubectl refused by lapilli-case:", err)
		return guard.ExitRefused
	}
	argv, err := guard.Kubectl(args[2:], os.Getenv("KUBECONFIG"), pin)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return guard.ExitRefused
	}
	err = syscall.Exec(args[1], append([]string{"kubectl"}, argv...), guard.Environ(os.Environ()))
	fmt.Fprintln(os.Stderr, "lapilli-case guard-kubectl:", args[1]+":", err)
	return 1
}

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

func printJSON(v any) {
	raw, _ := json.MarshalIndent(v, "", " ")
	fmt.Println(string(raw))
}

func cmdVerify(_ context.Context, args []string) (int, error) {
	dirs, err := parse(flag.NewFlagSet("verify", flag.ContinueOnError), args)
	if err != nil || len(dirs) == 0 {
		return 2, errors.Join(err, errors.New("which case?"))
	}
	code := 0
	for _, dir := range dirs {
		problems, err := casefile.Verify(dir)
		if err != nil {
			return 1, err
		}
		if _, err := casefile.Load(dir); err != nil { // sealed but unreadable is not a usable case either
			problems = append(problems, err.Error())
		}
		if len(problems) > 0 {
			fmt.Printf("%s: NOT INTACT\n", dir)
			for _, p := range problems {
				fmt.Println("  " + p)
			}
			code = 1
			continue
		}
		m, err := casefile.LoadManifest(dir)
		if err != nil {
			return 1, err
		}
		fmt.Printf("%s: intact (%s)\n", dir, m.Digest[:16])
	}
	return code, nil
}

func cmdSeal(_ context.Context, args []string) (int, error) {
	dirs, err := parse(flag.NewFlagSet("seal", flag.ContinueOnError), args)
	if err != nil || len(dirs) != 1 {
		return 2, errors.Join(err, errors.New("one case directory is expected"))
	}
	if _, err := casefile.Load(dirs[0]); err != nil {
		return 1, err
	}
	m, err := casefile.Seal(dirs[0])
	if err != nil {
		return 1, err
	}
	fmt.Println(m.Digest)
	return 0, nil
}

// reportFreeze prints what a freeze found and says so when the case cannot be solved from the copy.
func reportFreeze(info *casefile.FreezeInfo, m *casefile.Manifest) int {
	printJSON(struct {
		*casefile.FreezeInfo
		Digest string `json:"manifest_digest"`
	}{info, m.Digest})
	var missing []string
	for pattern, found := range info.EvidenceInSnapshot {
		if !found {
			missing = append(missing, pattern)
		}
	}
	if len(missing) > 0 {
		fmt.Fprintf(os.Stderr, "warning: decisive evidence not found in the frozen copy: %q. An agent cannot solve this case from it.\n", missing)
		return 2
	}
	return 0
}

func cmdFreeze(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("freeze", flag.ContinueOnError)
	opt := freeze.Options{}
	var match multi
	fs.StringVar(&opt.OutDir, "o", "", "case directory to write")
	fs.StringVar(&opt.Kubeconfig, "kubeconfig", "", "the cluster to freeze (required; the default kubeconfig is never used)")
	fs.StringVar(&opt.MetricsURL, "metrics-url", "", "a Prometheus to freeze beside the cluster")
	fs.DurationVar(&opt.MetricsWindow, "metrics-window", time.Hour, "how far back the frozen metrics reach")
	fs.Var(&match, "metrics-match", "series selector to freeze; repeatable (default: every series)")
	fs.BoolVar(&opt.NodeLogs, "node-logs", false, "also read each node's kubelet journal. The collector does that by starting a pod with host access on every node; without this flag freeze only reads the API")
	pos, err := parse(fs, args)
	if err != nil || len(pos) != 1 || opt.OutDir == "" || opt.Kubeconfig == "" {
		return 2, errors.Join(err, errors.New("expected <case.yaml> -o <dir> --kubeconfig <file>"))
	}
	opt.CaseYAML, opt.MetricsSelectors = pos[0], match
	info, m, err := freeze.Freeze(ctx, opt)
	if err != nil {
		return 1, err
	}
	return reportFreeze(info, m), nil
}

func cmdPack(_ context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("pack", flag.ContinueOnError)
	snapshot := fs.String("snapshot", "", "a directory collected by crust-gather; its Secrets are redacted in place")
	metricsFile := fs.String("metrics", "", "a metrics file written by export-metrics")
	freezeTime := fs.Float64("freeze-time", 0, "the instant the stores were read, in Unix seconds")
	out := fs.String("o", "", "case directory to write")
	pos, err := parse(fs, args)
	if err != nil || len(pos) != 1 || *snapshot == "" || *out == "" || *freezeTime == 0 {
		return 2, errors.Join(err, errors.New("expected <case.yaml> --snapshot <dir> --freeze-time <unix seconds> -o <dir>"))
	}
	var store *metrics.Store
	if *metricsFile != "" {
		if store, err = metrics.Load(*metricsFile); err != nil {
			return 1, err
		}
	}
	info, m, err := freeze.Pack(pos[0], *snapshot, *out, *freezeTime, store)
	if err != nil {
		return 1, err
	}
	return reportFreeze(info, m), nil
}

func cmdExportMetrics(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("export-metrics", flag.ContinueOnError)
	url := fs.String("url", "", "the Prometheus to read")
	at := fs.Float64("at", 0, "end of the window in Unix seconds (default: now)")
	window := fs.Duration("window", time.Hour, "how far back to read")
	out := fs.String("o", "", "file to write, conventionally "+casefile.MetricsName)
	var match multi
	fs.Var(&match, "match", "series selector; repeatable (default: every series)")
	if pos, err := parse(fs, args); err != nil || len(pos) != 0 || *url == "" || *out == "" {
		return 2, errors.Join(err, errors.New("expected --url <prometheus> -o <file>"))
	}
	end := time.Now()
	if *at != 0 {
		end = metrics.FromSeconds(*at)
	}
	store, err := metrics.Export(ctx, nil, *url, end, *window, match...)
	if err != nil {
		return 1, err
	}
	if err := store.Save(*out); err != nil {
		return 1, err
	}
	series, samples := store.Size()
	fmt.Printf("%d series, %d samples, window ending %s -> %s\n", series, samples, end.UTC().Format(time.RFC3339), *out)
	return 0, nil
}

func self() string {
	if p, err := os.Executable(); err == nil {
		return p
	}
	return os.Args[0]
}

func cmdServe(ctx context.Context, args []string) (int, error) {
	dirs, err := parse(flag.NewFlagSet("serve", flag.ContinueOnError), args)
	if err != nil || len(dirs) != 1 {
		return 2, errors.Join(err, errors.New("one case directory is expected"))
	}
	s, err := replay.Serve(ctx, dirs[0], "", self())
	if err != nil {
		return 1, err
	}
	defer s.Close()
	fmt.Printf("# case %s, frozen at %s, is being served from %s; Ctrl-C to stop\n", s.Case.ID, s.Info.FrozenAt, s.Workdir)
	for _, k := range []string{"KUBECONFIG", "PROM_URL"} {
		if v, ok := s.Env[k]; ok {
			fmt.Printf("export %s=%s\n", k, v)
		}
	}
	fmt.Printf("export PATH=%s:$PATH\n", filepath.Join(s.Workdir, "bin"))
	<-ctx.Done()
	return 0, nil
}

var slugUnsafe = regexp.MustCompile(`[^a-z0-9.]+`)

type runOptions struct {
	agent, out string
	runs       int
	agentOpt   agent.Options
}

// oneRun lets the agent investigate once and records what it did.
func oneRun(ctx context.Context, c *casefile.Case, condition string, env map[string]string, o runOptions) error {
	model := o.agentOpt.Model
	if model == "" {
		model = "default"
	}
	slug := slugUnsafe.ReplaceAllString(strings.ToLower(condition+"-"+o.agent+"-"+model), "-")
	index := 1 // the next free number: a second batch into the same directory adds to the first
	for ; ; index++ {
		if _, err := os.Stat(filepath.Join(o.out, c.ID, fmt.Sprintf("%s-%d.json", slug, index))); os.IsNotExist(err) {
			break
		}
	}
	workdir, err := os.MkdirTemp("", "lapilli-agent-") // an empty directory: nothing for the agent to stumble on
	if err != nil {
		return err
	}
	defer os.RemoveAll(workdir)
	transcript, err := agent.Run(ctx, o.agent, c.Prompt, env, workdir, o.agentOpt)
	if err != nil {
		return err
	}
	run := grade.Run{RunID: fmt.Sprintf("%s/%s-%d", c.ID, slug, index), Case: c.ID, Condition: condition, RuleVersion: grade.RuleVersion,
		Transcript: *transcript, Process: grade.Check(c, transcript)}
	if err := run.Save(o.out, fmt.Sprintf("%s-%d", slug, index)); err != nil {
		return err
	}
	p := run.Process
	word := func(ok bool, yes, no string) string {
		if ok {
			return yes
		}
		return no
	}
	line := fmt.Sprintf("  %s: steps=%d evidence=%s specificity=%s ungrounded=%d", run.RunID, p.Steps, word(p.EvidenceAll, "all", "partial"), word(p.SpecificityNamed, "named", "missing"), len(p.UngroundedEntities))
	if transcript.Error != nil {
		line += " ERROR " + strings.TrimSpace(*transcript.Error)
	}
	fmt.Println(line)
	return nil
}

func cmdRun(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	o := runOptions{}
	fs.StringVar(&o.agent, "agent", "claude-code", "one of: "+strings.Join(agent.Names(), ", "))
	fs.StringVar(&o.agentOpt.Model, "model", "", "model, in the agent's own naming")
	fs.IntVar(&o.runs, "runs", 3, "investigations per case; one run of a stochastic agent says little")
	fs.StringVar(&o.out, "o", "results", "directory for run records")
	fs.StringVar(&o.agentOpt.Command, "agent-command", "", "for --agent command")
	fs.StringVar(&o.agentOpt.Temperature, "temperature", "", "for agents that take one")
	fs.Float64Var(&o.agentOpt.BudgetUSD, "budget-usd", 1.0, "spending ceiling per run, where the agent supports one")
	fs.DurationVar(&o.agentOpt.Timeout, "timeout", 0, "time limit per run (default: the adapter's)")
	var passEnv multi
	fs.Var(&passEnv, "pass-env", "a variable of your environment the agent may see, such as the key for its model; repeatable. Nothing else that looks like a credential is passed")
	live := fs.Bool("live", false, "investigate a live cluster instead of the frozen copy, to compare the two")
	kubeconfig := fs.String("kubeconfig", "", "with --live: the one cluster the agent may talk to")
	promURL := fs.String("prom-url", "", "with --live: the metrics endpoint")
	allowUnsealed := fs.Bool("allow-unsealed", false, "run a case that is unsealed or was altered after sealing")
	dirs, err := parse(fs, args)
	o.agentOpt.PassEnv = passEnv
	switch {
	case err != nil || len(dirs) == 0:
		return 2, errors.Join(err, errors.New("which case?"))
	case *live && *kubeconfig == "":
		return 2, errors.New("--live needs --kubeconfig: the default kubeconfig is never used")
	case !*live && (*kubeconfig != "" || *promURL != ""):
		return 2, errors.New("--kubeconfig and --prom-url only make sense with --live")
	}
	for _, dir := range dirs {
		if *live {
			c, err := casefile.Load(dir)
			if err != nil {
				return 1, err
			}
			workdir, err := os.MkdirTemp("", "lapilli-live-")
			if err != nil {
				return 1, err
			}
			defer os.RemoveAll(workdir)
			abs, _ := filepath.Abs(*kubeconfig)
			bin := filepath.Join(workdir, "bin")
			if err := replay.WriteTools(bin, abs, self()); err != nil { // the guard pins the agent to this one kubeconfig
				return 1, err
			}
			env := map[string]string{"KUBECONFIG": abs, "PATH": bin + string(os.PathListSeparator) + os.Getenv("PATH")}
			if *promURL != "" {
				env["PROM_URL"] = *promURL
			}
			fmt.Printf("%s (live)\n", c.ID)
			for i := 0; i < o.runs; i++ {
				if err := oneRun(ctx, c, "live", env, o); err != nil {
					return 1, err
				}
			}
			continue
		}
		problems, err := casefile.Verify(dir)
		if err != nil {
			return 1, err
		}
		if len(problems) > 0 && !*allowUnsealed {
			return 1, fmt.Errorf("%s: refusing to run an altered or unsealed case (%s); --allow-unsealed overrides", dir, problems[0])
		}
		s, err := replay.Serve(ctx, dir, "", self())
		if err != nil {
			return 1, err
		}
		fmt.Printf("%s (frozen)\n", s.Case.ID)
		for i := 0; i < o.runs; i++ {
			if err := oneRun(ctx, s.Case, "frozen", s.Env, o); err != nil {
				s.Close()
				return 1, err
			}
		}
		s.Close()
	}
	return 0, nil
}

func cmdPackets(_ context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("packets", flag.ContinueOnError)
	out := fs.String("o", "packets.json", "blind packets, for the judge")
	keyFile := fs.String("key", "key.json", "packet id -> run id; not for the judge")
	seed := fs.Int64("seed", 0, "the same seed gives the same ids and order; 0 picks one that cannot be guessed")
	pos, err := parse(fs, args)
	if err != nil || len(pos) < 2 {
		return 2, errors.Join(err, errors.New("expected <results> <case>..."))
	}
	runs, err := grade.LoadRuns(pos[0])
	if err != nil {
		return 1, err
	}
	cases := map[string]*casefile.Case{}
	for _, dir := range pos[1:] {
		c, err := casefile.Load(dir)
		if err != nil {
			return 1, err
		}
		cases[c.ID] = c
	}
	if *seed == 0 { // with the seed and the list of runs anyone can rebuild the key, so the default is not a constant
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			return 1, err
		}
		*seed = int64(binary.LittleEndian.Uint64(b[:]) >> 1)
	}
	packets, key, err := grade.Packets(runs, cases, *seed)
	if err != nil {
		return 1, err
	}
	for path, v := range map[string]any{*out: packets, *keyFile: key} {
		raw, _ := json.MarshalIndent(v, "", " ")
		if err := os.WriteFile(path, append(raw, '\n'), 0o644); err != nil {
			return 1, err
		}
	}
	fmt.Printf("%d blind packets -> %s\nkey (do not show to the judge) -> %s\nseed %d (keep it with the key, not with the packets)\n\nJudge rule, version %d:\n%s\n",
		len(packets), *out, *keyFile, *seed, grade.RuleVersion, grade.JudgeInstructions)
	return 0, nil
}

func readJSON(path string, into any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func cmdReport(_ context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	verdictsFile := fs.String("verdicts", "", "the judge's verdicts, keyed by packet id")
	keyFile := fs.String("key", "", "the key written by packets")
	asJSON := fs.Bool("json", false, "print rows as JSON")
	pos, err := parse(fs, args)
	if err != nil || len(pos) != 1 || (*verdictsFile == "") != (*keyFile == "") {
		return 2, errors.Join(err, errors.New("expected <results>, and --verdicts and --key together or not at all"))
	}
	runs, err := grade.LoadRuns(pos[0])
	if err != nil {
		return 1, err
	}
	var verdicts map[string]grade.Verdict
	var key map[string]string
	if *verdictsFile != "" {
		if err := errors.Join(readJSON(*verdictsFile, &verdicts), readJSON(*keyFile, &key)); err != nil {
			return 1, err
		}
	}
	rows := grade.Summarize(runs, verdicts, key)
	versions := map[int]bool{}
	for _, r := range runs {
		versions[r.RuleVersion] = true
	}
	if len(versions) > 1 {
		return 1, fmt.Errorf("the runs under %s were graded under %d different rule versions; they do not compare", pos[0], len(versions))
	}
	if *asJSON {
		printJSON(struct {
			RuleVersion int         `json:"rule_version"`
			Rows        []grade.Row `json:"rows"`
		}{grade.RuleVersion, rows})
		return 0, nil
	}
	fmt.Printf("Grading rule version %d (docs/case-grading.md).\n\n", grade.RuleVersion)
	fmt.Println("| case | agent | model | condition | outcome | decisive evidence retrieved | specificity named | runs citing unseen entities | ended without an answer | steps | cost |")
	fmt.Println("|---|---|---|---|---|---|---|---|---|---|---|")
	for _, r := range rows {
		outcome := "not judged"
		if r.OutcomePass != nil {
			// Out of the runs that have a verdict: a batch judged in part does not get the rest counted as failures.
			outcome = fmt.Sprintf("%d/%d", *r.OutcomePass, r.Judged)
			if r.Judged != r.Runs {
				outcome += fmt.Sprintf(" (%d not judged)", r.Runs-r.Judged)
			}
		}
		fmt.Printf("| %s | %s | %s | %s | %s | %d/%d | %d/%d | %d/%d | %d/%d | %.1f | $%.3f |\n", r.Case, r.Agent, r.Model, r.Condition, outcome,
			r.EvidenceAll, r.Runs, r.SpecificityNamed, r.Runs, r.UngroundedRuns, r.Runs, r.Errored, r.Runs, r.MeanSteps, r.MeanCostUSD)
	}
	return 0, nil
}

func cmdPromq(_ context.Context, args []string) (int, error) {
	url := os.Getenv("PROM_URL")
	if url == "" {
		fmt.Println("no metrics endpoint is configured for this case")
		return 0, nil
	}
	if len(args) == 0 {
		fmt.Println(metrics.PromqUsage)
		return 1, nil
	}
	// Errors go to standard output: the caller is an agent reading a tool result, and a failed query is
	// something it should see and correct.
	if err := metrics.Promq(os.Stdout, nil, url, args, time.Now()); err != nil {
		fmt.Println(err)
		return 1, nil
	}
	return 0, nil
}
