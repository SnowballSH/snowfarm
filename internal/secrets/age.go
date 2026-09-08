// Package secrets decrypts the farm's age files and hands each agent its own
// variables over a peer-credentialed Unix socket. Nothing here writes a value
// anywhere but the calling process's own connection.
package secrets

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"filippo.io/age"
)

// SupervisorName owns the guard's own credentials. It is not a roster agent,
// so Load asks for it whatever the caller passes.
const SupervisorName = "supervisor"

const fileSuffix = ".age"

// Store holds one set of variables per agent. It is read-only once loaded, so
// a SIGHUP builds a new one and publishes it in place of this one.
type Store struct {
	byAgent map[string]map[string]string
}

// Load decrypts <dir>/<agent>.age for each named agent and for the
// supervisor. The caller passes the agents the roster has enabled: a disabled
// agent has no farm-<agent> user yet and need not have an age file, and the
// SIGHUP reload picks it up on the pass that enables it.
func Load(identityPath, dir string, agents []string) (*Store, error) {
	identities, err := readIdentities(identityPath)
	if err != nil {
		return nil, err
	}
	store := &Store{byAgent: make(map[string]map[string]string, len(agents)+1)}
	for _, agent := range withSupervisor(agents) {
		vars, err := decrypt(filepath.Join(dir, agent+fileSuffix), identities)
		if err != nil {
			return nil, fmt.Errorf("secrets for %s: %w", agent, err)
		}
		store.byAgent[agent] = vars
	}
	return store, nil
}

// For reports the variables loaded for an agent. The copy it returns is what
// keeps a caller from editing a store other connections are reading.
func (s *Store) For(agent string) (map[string]string, bool) {
	vars, ok := s.byAgent[agent]
	if !ok {
		return nil, false
	}
	return maps.Clone(vars), true
}

func withSupervisor(agents []string) []string {
	named := make(map[string]struct{}, len(agents)+1)
	named[SupervisorName] = struct{}{}
	for _, agent := range agents {
		named[agent] = struct{}{}
	}
	return slices.Sorted(maps.Keys(named))
}

func readIdentities(path string) ([]age.Identity, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open age identity: %w", err)
	}
	defer func() { _ = file.Close() }()
	identities, err := age.ParseIdentities(file)
	if err != nil {
		return nil, fmt.Errorf("parse age identity: %w", err)
	}
	return identities, nil
}

func decrypt(path string, identities []age.Identity) (map[string]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	decrypted, err := age.Decrypt(file, identities...)
	if err != nil {
		return nil, fmt.Errorf("decrypt %s: %w", filepath.Base(path), err)
	}
	plain, err := io.ReadAll(decrypted)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}
	return parseEnv(filepath.Base(path), plain)
}

// parseEnv reads KEY=VALUE lines, skipping blank ones and comments. Its errors
// place the fault by line number and never quote the line, because the line is
// the secret.
func parseEnv(source string, data []byte) (map[string]string, error) {
	vars := make(map[string]string)
	lines := bufio.NewScanner(bytes.NewReader(data))
	for number := 1; lines.Scan(); number++ {
		line := strings.TrimSpace(lines.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, found := strings.Cut(line, "=")
		if !found || name == "" {
			return nil, fmt.Errorf("%s line %d is not KEY=VALUE", source, number)
		}
		if _, duplicate := vars[name]; duplicate {
			return nil, fmt.Errorf("%s line %d repeats %s", source, number, name)
		}
		vars[name] = value
	}
	if err := lines.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", source, err)
	}
	return vars, nil
}

// writeEnv prints the variables as sorted KEY=VALUE lines and reports the
// names it wrote, which is all a log may carry.
func writeEnv(w io.Writer, vars map[string]string) ([]string, error) {
	names := slices.Sorted(maps.Keys(vars))
	buffered := bufio.NewWriter(w)
	for _, name := range names {
		if _, err := fmt.Fprintf(buffered, "%s=%s\n", name, vars[name]); err != nil {
			return nil, err
		}
	}
	if err := buffered.Flush(); err != nil {
		return nil, err
	}
	return names, nil
}
