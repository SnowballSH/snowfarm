package secrets

import (
	"fmt"
	"io"
	"net"
	"time"
)

const (
	// DefaultSocket is where the guard listens, inside the setgid runtime
	// directory the tmpfiles entry owns.
	DefaultSocket = "/run/snowfarm/guard.sock"

	// DefaultTimeout bounds the whole exchange. Hermes gives its secrets
	// helper three seconds, so a helper that hangs must still be a helper
	// that returns.
	DefaultTimeout = 2 * time.Second
)

// Fetch asks the guard for the calling process's own variables. The guard
// reads the caller's uid from the socket, so this asks for nothing and
// carries no credential.
func Fetch(path string, timeout time.Duration) (map[string]string, error) {
	deadline := time.Now().Add(timeout)
	conn, err := net.DialTimeout("unix", path, timeout)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", path, err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, err
	}
	served, err := io.ReadAll(conn)
	if err != nil {
		return nil, fmt.Errorf("read from %s: %w", path, err)
	}
	return parseEnv(path, served)
}

// PrintEnv writes the fetched variables as the sorted KEY=VALUE lines Hermes'
// command secrets helper expects, and nothing else.
func PrintEnv(w io.Writer, path string, timeout time.Duration) error {
	vars, err := Fetch(path, timeout)
	if err != nil {
		return err
	}
	_, err = writeEnv(w, vars)
	return err
}
