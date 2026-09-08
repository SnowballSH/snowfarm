package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/SnowballSH/snowfarm/internal/roster"
	"github.com/SnowballSH/snowfarm/internal/sysops"
)

var version = "dev"

const (
	usage         = "usage: snowfarm plan|apply|guard|reload|secret-env|version"
	defaultConfig = "/etc/snowfarm/farm.yaml"
)

var errNotImplemented = errors.New("not implemented")

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
		return guard(args[1:])
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
	applier := &sysops.Applier{Only: opts.only, Start: opts.start}
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

func guard([]string) error { return fmt.Errorf("guard: %w", errNotImplemented) }

func reload([]string) error { return fmt.Errorf("reload: %w", errNotImplemented) }

func secretEnv([]string) error { return fmt.Errorf("secret-env: %w", errNotImplemented) }
