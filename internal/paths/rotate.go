package paths

import (
	"fmt"
	"os"
)

// MaxLogBytes is the size at which a log is rotated.
//
// Hook logs grow with every tool call an agent makes, so an unbounded log would quietly
// consume disk over weeks of use. One generation is kept, which is enough to investigate
// something that just happened without keeping history forever.
const MaxLogBytes = 8 << 20

// OpenLog opens a log for appending, rotating it first if it has grown too large.
//
// Rotation is by rename, so a writer that already holds the old file keeps writing to it
// harmlessly rather than failing mid-line.
func OpenLog(path string) (*os.File, error) {
	if err := rotateIfLarge(path); err != nil {
		return nil, err
	}

	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, FileMode)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}

	return file, nil
}

// rotateIfLarge moves a log aside once it exceeds MaxLogBytes.
func rotateIfLarge(path string) error {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("checking %s: %w", path, err)
	}

	if info.Size() < MaxLogBytes {
		return nil
	}

	if err := os.Rename(path, path+".1"); err != nil {
		return fmt.Errorf("rotating %s: %w", path, err)
	}

	return nil
}
