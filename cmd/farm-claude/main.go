package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/SnowballSH/snowfarm/internal/claude"
)

var version = "dev"

func main() {
	if len(os.Args) == 2 && (os.Args[1] == "version" || os.Args[1] == "--version") {
		fmt.Println(version)
		return
	}
	config, err := config()
	if err != nil {
		fmt.Fprintf(os.Stderr, "farm-claude: %v\n", err)
		os.Exit(claude.ExitError)
	}
	wrapper := claude.Wrapper{
		Config: config,
		Env:    os.Environ(),
		Stdout: os.Stdout,
		Stderr: os.Stderr,
		Now:    time.Now,
	}
	os.Exit(wrapper.Run(os.Args[1:]))
}

func config() (claude.Config, error) {
	agent, err := claude.CallerAgent()
	if err != nil {
		return claude.Config{}, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return claude.Config{}, err
	}
	return claude.Config{
		Dir:         claude.DefaultDir,
		Bin:         claude.DefaultBin,
		ConfigDir:   filepath.Join(home, ".claude"),
		Agent:       agent,
		SlotTimeout: claude.DefaultSlotTimeout,
		SlotPoll:    claude.DefaultSlotPoll,
	}, nil
}
