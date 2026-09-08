package claude

import (
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"time"
)

// The wrapper's exit codes, which the farm-claude-code skill tells every
// agent how to read.
const (
	ExitOK          = 0
	ExitError       = 1
	ExitNoSlot      = 3
	ExitLimitWindow = 4
	ExitLimitHit    = 5
)

// The host contract, fixed at compile time: an agent that could point the
// wrapper at another directory would take slots the farm does not count and
// write runs nothing reads.
const (
	DefaultDir         = "/srv/snowfarm/claude"
	DefaultBin         = "/usr/local/bin/claude"
	DefaultSlotTimeout = 20 * time.Minute
	DefaultSlotPoll    = 500 * time.Millisecond
)

const tokenEnv = "CLAUDE_CODE_OAUTH_TOKEN"

// Config is where the farm keeps its Claude Code state and what it runs.
type Config struct {
	Dir         string
	Bin         string
	ConfigDir   string
	Agent       string
	SlotTimeout time.Duration
	SlotPoll    time.Duration
}

func (c Config) SlotsDir() string { return filepath.Join(c.Dir, "shared", "slots") }

func (c Config) MarkerPath() string { return filepath.Join(c.Dir, "shared", "limit-until") }

func (c Config) RunsPath() string { return filepath.Join(c.Dir, "runs", c.Agent+".jsonl") }

// Wrapper is one farm-claude invocation.
type Wrapper struct {
	Config Config
	Env    []string
	Stdout io.Writer
	Stderr io.Writer
	Now    func() time.Time
}

func (w Wrapper) Run(args []string) int {
	opts, err := ParseArgs(args)
	if err != nil {
		return w.fail(err)
	}
	if value, ok := envLookup(w.Env, tokenEnv); !ok || value == "" {
		return w.fail(fmt.Errorf("%s is unset: the run has no subscription to spend", tokenEnv))
	}

	reset, active, err := ReadMarker(w.Config.MarkerPath(), w.Now())
	if err != nil {
		return w.fail(err)
	}
	if active {
		return w.reportLimit(Limit{Kind: KindMarker, ResetAt: reset}, w.newRecord(opts), ExitLimitWindow)
	}

	slot, err := AcquireSlot(w.Config.SlotsDir(), w.Config.SlotTimeout, w.Config.SlotPoll)
	if errors.Is(err, ErrNoSlot) {
		_, _ = fmt.Fprintf(w.Stderr, "farm-claude: %v; every other agent is running one\n", err)
		return ExitNoSlot
	}
	if err != nil {
		return w.fail(err)
	}
	defer func() { _ = slot.Release() }()

	return w.exec(opts)
}

func (w Wrapper) exec(opts Options) int {
	argv, err := Argv(w.Config.Bin, opts)
	if err != nil {
		return w.fail(err)
	}
	cmd := exec.Command(argv[0], argv[1:]...) // #nosec G204 -- the binary is the compiled default and every argument comes from Argv
	cmd.Env = Env(w.Env, w.Config.ConfigDir, opts)
	cmd.Stderr = w.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return w.fail(err)
	}
	started := w.Now()
	if err := cmd.Start(); err != nil {
		return w.fail(err)
	}
	stream, parseErr := ParseStream(stdout)
	if parseErr != nil {
		_, _ = io.Copy(io.Discard, stdout)
	}
	waitErr := cmd.Wait()

	result, complete := stream.Result()
	record := w.newRecord(opts)
	record.TS = started.Format(time.RFC3339)
	record.NumTurns = result.NumTurns
	record.DurationMS = runDuration(result, started, w.Now())
	record.IsError = result.IsError || waitErr != nil || !complete
	record.APIErrorStatus = result.APIErrorStatus
	record.TotalCostUSD = result.TotalCostUSD
	if limit, ok := stream.Limit(w.Now()); ok {
		return w.reportLimit(limit, record, ExitLimitHit)
	}
	w.record(record)

	if result.Result != "" {
		_, _ = fmt.Fprintln(w.Stdout, result.Result)
	}
	if parseErr != nil {
		return w.fail(parseErr)
	}
	if !complete {
		return w.fail(errors.New("claude ended without a result: the run has no account of itself"))
	}
	if record.IsError {
		if waitErr != nil {
			_, _ = fmt.Fprintf(w.Stderr, "farm-claude: %v\n", waitErr)
		}
		return ExitError
	}
	return ExitOK
}

func runDuration(result ResultLine, started, ended time.Time) int64 {
	if result.DurationMS > 0 {
		return result.DurationMS
	}
	return ended.Sub(started).Milliseconds()
}

// reportLimit prints the message the skill tells agents to stop on, and
// records the limit for the guard, which owns the marker file itself.
func (w Wrapper) reportLimit(limit Limit, record Record, code int) int {
	record.LimitKind = limit.Kind
	record.ResetAt = limit.ResetAt.Format(time.RFC3339)
	record.IsError = true
	w.record(record)
	_, _ = fmt.Fprintln(w.Stdout, limit.Message())
	return code
}

func (w Wrapper) newRecord(opts Options) Record {
	return Record{
		TS:     w.Now().Format(time.RFC3339),
		Agent:  w.Config.Agent,
		Model:  opts.Model,
		Effort: opts.Effort,
	}
}

func (w Wrapper) record(record Record) {
	if err := AppendRecord(w.Config.RunsPath(), record); err != nil {
		_, _ = fmt.Fprintf(w.Stderr, "farm-claude: the run was not recorded: %v\n", err)
	}
}

func (w Wrapper) fail(err error) int {
	_, _ = fmt.Fprintf(w.Stderr, "farm-claude: %v\n", err)
	return ExitError
}
