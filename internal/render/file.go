package render

import "io/fs"

// File is one rendered artefact: the applier writes Content to Path, chowns
// it Owner:Group and chmods it Mode.
type File struct {
	Path    string
	Mode    fs.FileMode
	Owner   string
	Group   string
	Content []byte
}

const (
	// The two files the agent must not own: C4 installs them root:farm-<agent>
	// and then chattr +i, so a compromised agent cannot rewrite its own
	// persona or reach a provider the roster did not give it.
	rootOwnedMode  fs.FileMode = 0o640
	agentOwnedMode fs.FileMode = 0o600
	rootOwner                  = "root"
)

// IsSymlink reports whether the applier must create Path as a symlink to
// Content rather than write Content into it.
func (f File) IsSymlink() bool { return f.Mode&fs.ModeSymlink != 0 }
