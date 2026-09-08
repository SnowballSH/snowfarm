package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/SnowballSH/snowfarm/internal/guard"
	"github.com/SnowballSH/snowfarm/internal/roster"
	"github.com/SnowballSH/snowfarm/internal/secrets"
	"github.com/SnowballSH/snowfarm/internal/sysops"
)

var version = "dev"

const (
	usage         = "usage: snowfarm plan|apply|guard|reload|secret-env|version"
	defaultConfig = "/etc/snowfarm/farm.yaml"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "snowfarm: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	switch args[0] {
	case "plan":
		return plan(args[1:])
	case "apply":
		return apply(args[1:])
	case "guard":
		return runGuard(args[1:])
	case "reload":
		return reload(args[1:])
	case "secret-env":
		return secretEnv(args[1:])
	case "version":
		fmt.Println(version)
		return nil
	default:
		return fmt.Errorf("unknown command %q\n%s", args[0], usage)
	}
}

type applyOptions struct {
	config string
	only   []string
	dryRun bool
	start  bool
}

func parseApplyOptions(name string, args []string) (applyOptions, error) {
	set := flag.NewFlagSet(name, flag.ContinueOnError)
	var opts applyOptions
	var only string
	set.StringVar(&opts.config, "config", defaultConfig, "roster to read")
	set.StringVar(&only, "only", "", "restrict the change to these agents")
	set.BoolVar(&opts.dryRun, "dry-run", false, "print the plan and change nothing")
	set.BoolVar(&opts.start, "start", false, "start each manager gateway whose channel ids are resolved")
	if err := set.Parse(args); err != nil {
		return opts, err
	}
	if set.NArg() > 0 {
		return opts, fmt.Errorf("unexpected argument %q", set.Arg(0))
	}
	if only != "" {
		opts.only = strings.Split(only, ",")
	}
	return opts, nil
}

func plan(args []string) error {
	opts, err := parseApplyOptions("plan", args)
	if err == nil && opts.start {
		err = errors.New("--start belongs to apply")
	}
	if err == nil {
		opts.dryRun = true
		err = converge(os.Stdout, opts)
	}
	if err != nil {
		return fmt.Errorf("plan: %w", err)
	}
	return nil
}

func apply(args []string) error {
	opts, err := parseApplyOptions("apply", args)
	if err == nil {
		err = converge(os.Stdout, opts)
	}
	if err != nil {
		return fmt.Errorf("apply: %w", err)
	}
	return nil
}

func converge(out io.Writer, opts applyOptions) error {
	r, err := roster.Load(opts.config)
	if err != nil {
		return err
	}
	if err := roster.CheckOnly(r, opts.only); err != nil {
		return err
	}
	applier := &sysops.Applier{Only: opts.only, Start: opts.start, Warn: os.Stderr}
	channels, err := applier.Channels()
	if err != nil {
		return err
	}
	p, err := applier.Plan(r, channels)
	if err != nil {
		return err
	}
	if p.Empty() {
		_, _ = fmt.Fprintln(out, "no changes")
	} else {
		_, _ = fmt.Fprint(out, p)
	}
	if opts.dryRun {
		return nil
	}
	if err := applier.Apply(r, p); err != nil {
		return err
	}
	pending, err := applier.PendingGateways(r)
	if err != nil {
		return err
	}
	for _, name := range pending {
		_, _ = fmt.Fprintf(out, "%s: gateway installed but not started, its channel ids are unresolved\n", name)
	}
	return nil
}

// runGuard serves until the process is asked to stop. SIGHUP is a reload, not
// a stop, so it is delivered to the guard rather than ending the context.
func runGuard(args []string) error {
	set := flag.NewFlagSet("guard", flag.ContinueOnError)
	var cfg guard.Config
	set.StringVar(&cfg.ConfigPath, "config", defaultConfig, "roster to read")
	set.StringVar(&cfg.StateDir, "state", guard.DefaultStateDir, "directory holding the ledger, the event log and the channel map")
	set.StringVar(&cfg.SecretsDir, "secrets", guard.DefaultSecretsDir, "directory of the per-agent age files")
	set.StringVar(&cfg.IdentityPath, "age-identity", guard.DefaultIdentityPath, "age identity that decrypts them")
	set.StringVar(&cfg.SocketPath, "socket", secrets.DefaultSocket, "socket each agent asks for its own variables on")
	set.StringVar(&cfg.PinsPath, "pins", guard.DefaultPinsPath, "pins file the Hermes drift probe reads")
	set.StringVar(&cfg.PIDPath, "pidfile", guard.DefaultPIDPath, "file `snowfarm reload` finds this guard through")
	err := set.Parse(args)
	if err == nil && set.NArg() > 0 {
		err = fmt.Errorf("unexpected argument %q", set.Arg(0))
	}
	if err != nil {
		return fmt.Errorf("guard: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	cfg.Signals = hup

	if err := guard.Run(ctx, cfg); err != nil {
		return fmt.Errorf("guard: %w", err)
	}
	return nil
}

// reload delivers SIGHUP to the running guard, which re-reads the roster and
// the age files without dropping the ledger.
func reload(args []string) error {
	set := flag.NewFlagSet("reload", flag.ContinueOnError)
	pidfile := set.String("pidfile", guard.DefaultPIDPath, "file the guard wrote its pid to")
	err := set.Parse(args)
	if err == nil && set.NArg() > 0 {
		err = fmt.Errorf("unexpected argument %q", set.Arg(0))
	}
	if err == nil {
		err = signalGuard(*pidfile)
	}
	if err != nil {
		return fmt.Errorf("reload: %w", err)
	}
	return nil
}

func signalGuard(pidfile string) error {
	data, err := os.ReadFile(pidfile)
	if err != nil {
		return err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return fmt.Errorf("%s does not hold a pid: %w", pidfile, err)
	}
	if pid <= 0 {
		return fmt.Errorf("%s holds %d, which is not a process", pidfile, pid)
	}
	if err := syscall.Kill(pid, syscall.SIGHUP); err != nil {
		return fmt.Errorf("signal %d: %w", pid, err)
	}
	return nil
}

func secretEnv(args []string) error {
	set := flag.NewFlagSet("secret-env", flag.ContinueOnError)
	socket := set.String("socket", secrets.DefaultSocket, "guard socket to ask")
	err := set.Parse(args)
	if err == nil && set.NArg() > 0 {
		err = fmt.Errorf("unexpected argument %q", set.Arg(0))
	}
	if err == nil {
		err = secrets.PrintEnv(os.Stdout, *socket, secrets.DefaultTimeout)
	}
	if err != nil {
		return fmt.Errorf("secret-env: %w", err)
	}
	return nil
}
