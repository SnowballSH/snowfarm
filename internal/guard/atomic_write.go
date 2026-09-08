package guard

import (
	"errors"
	"io/fs"
	"os"
)

// writeAtomic installs a file under a name a reader only ever finds whole:
// a reader that caught a half-written file would read no content at all and
// fail on a file the writer had every intention of completing.
func writeAtomic(path string, data []byte, mode fs.FileMode) error {
	temp := path + ".tmp"
	if err := os.WriteFile(temp, data, mode); err != nil {
		return err
	}
	if err := os.Rename(temp, path); err != nil {
		return errors.Join(err, os.Remove(temp))
	}
	return nil
}
