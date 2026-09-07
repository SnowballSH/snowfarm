package main

import (
	"errors"
	"fmt"
	"os"
)

var version = "dev"

const usage = "usage: snowfarm plan|apply|guard|reload|secret-env|version"

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

func plan([]string) error { return fmt.Errorf("plan: %w", errNotImplemented) }

func apply([]string) error { return fmt.Errorf("apply: %w", errNotImplemented) }

func guard([]string) error { return fmt.Errorf("guard: %w", errNotImplemented) }

func reload([]string) error { return fmt.Errorf("reload: %w", errNotImplemented) }

func secretEnv([]string) error { return fmt.Errorf("secret-env: %w", errNotImplemented) }
