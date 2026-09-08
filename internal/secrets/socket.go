package secrets

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	socketMode  = fs.FileMode(0o660)
	serveBudget = 5 * time.Second
)

// Server hands each caller the variables of the agent its uid belongs to. The
// socket lives in the setgid /run/snowfarm, so 0660 reaches farm-agents and
// nobody else; the peer uid, not anything the caller says, decides what it
// gets.
type Server struct {
	Path string

	// UIDToAgent maps a caller's uid to a roster agent. A uid it does not
	// know is served nothing.
	UIDToAgent func(uid uint32) (string, bool)

	// Log receives one line per connection, naming the agent and the
	// variables it was sent, never a value. A nil Log discards them.
	Log io.Writer

	store atomic.Pointer[Store]
}

// Swap publishes a store to every connection served from now on. A SIGHUP
// reload calls it while other goroutines are reading the old one.
func (s *Server) Swap(store *Store) { s.store.Store(store) }

// ListenAndServe serves until ctx ends, then waits for the connections in
// flight. Closing the listener removes the socket file.
func (s *Server) ListenAndServe(ctx context.Context) error {
	listener, err := s.listen()
	if err != nil {
		return err
	}
	var closing atomic.Bool
	stopped := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
		case <-stopped:
		}
		closing.Store(true)
		_ = listener.Close()
	}()
	var connections sync.WaitGroup
	err = s.accept(listener, &connections)
	deliberate := closing.Load()
	close(stopped)
	connections.Wait()
	if deliberate {
		return nil
	}
	return err
}

func (s *Server) listen() (*net.UnixListener, error) {
	if s.Path == "" {
		return nil, errors.New("secrets: no socket path")
	}
	if err := os.Remove(s.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("remove stale socket %s: %w", s.Path, err)
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: s.Path, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", s.Path, err)
	}
	if err := os.Chmod(s.Path, socketMode); err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("chmod %s: %w", s.Path, err)
	}
	return listener, nil
}

func (s *Server) accept(listener *net.UnixListener, connections *sync.WaitGroup) error {
	for {
		conn, err := listener.AcceptUnix()
		if err != nil {
			return fmt.Errorf("accept on %s: %w", s.Path, err)
		}
		connections.Add(1)
		go func() {
			defer connections.Done()
			s.serve(conn)
		}()
	}
}

func (s *Server) serve(conn *net.UnixConn) {
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(serveBudget)); err != nil {
		s.logf("secrets: set deadline: %v", err)
		return
	}
	uid, err := peerUID(conn)
	if err != nil {
		s.logf("secrets: peer credentials: %v", err)
		return
	}
	agent, ok := s.agentFor(uid)
	if !ok {
		s.logf("secrets: uid %d belongs to no agent", uid)
		return
	}
	store := s.store.Load()
	if store == nil {
		s.logf("secrets: %s asked before a store was published", agent)
		return
	}
	vars, ok := store.For(agent)
	if !ok {
		s.logf("secrets: no secrets are loaded for %s", agent)
		return
	}
	names, err := writeEnv(conn, vars)
	if err != nil {
		s.logf("secrets: serve %s: %v", agent, err)
		return
	}
	s.logf("secrets: served %s %s", agent, strings.Join(names, " "))
}

func (s *Server) agentFor(uid uint32) (string, bool) {
	if s.UIDToAgent == nil {
		return "", false
	}
	return s.UIDToAgent(uid)
}

func (s *Server) logf(format string, args ...any) {
	if s.Log == nil {
		return
	}
	_, _ = fmt.Fprintf(s.Log, format+"\n", args...)
}
