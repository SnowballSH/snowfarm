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
	// Every file apply renders into a profile is the agent's to read and
	// nobody's but root's to write: config.yaml and SOUL.md carry chattr +i
	// on top, and the skills are held by the ownership of the directories
	// they sit in.
	rootOwnedMode fs.FileMode = 0o640
	rootOwner                 = "root"
)

// IsSymlink reports whether the applier must create Path as a symlink to
// Content rather than write Content into it.
func (f File) IsSymlink() bool { return f.Mode&fs.ModeSymlink != 0 }
