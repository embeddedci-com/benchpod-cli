package serialconsole

import "io"

// NewConsole wraps an already-open port (or a test fake answering like the firmware) as a
// Console. The CLI opens real ports with Open/OpenBenchpod; this is for tests outside the package.
func NewConsole(rw io.ReadWriteCloser) *Console { return newConsole(rw) }
