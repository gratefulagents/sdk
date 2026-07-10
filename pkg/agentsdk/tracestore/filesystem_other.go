//go:build !linux

package tracestore

import (
	"fmt"
)

func initializeFilesystemRoot(string) (string, int, error) {
	return "", -1, fmt.Errorf("filesystem trace store mutations require Linux openat2 confinement")
}

func (s *FilesystemTraceStore) createRunDir(string, []byte) error {
	return fmt.Errorf("filesystem trace store mutations require Linux openat2 confinement")
}

func (s *FilesystemTraceStore) appendTrace(string, string, []byte) error {
	return fmt.Errorf("filesystem trace store mutations require Linux openat2 confinement")
}

func (s *FilesystemTraceStore) writeFile(string, string, []byte) error {
	return fmt.Errorf("filesystem trace store mutations require Linux openat2 confinement")
}

func (s *FilesystemTraceStore) readMetadata(string) ([]byte, error) {
	return nil, fmt.Errorf("filesystem trace store requires Linux openat2 confinement")
}

func (s *FilesystemTraceStore) writeMetadata(string, []byte) error {
	return fmt.Errorf("filesystem trace store mutations require Linux openat2 confinement")
}

func (s *FilesystemTraceStore) verifyRunDir(string, string) error {
	return fmt.Errorf("filesystem trace store requires Linux openat2 confinement")
}

func closeFilesystemRoot(int) error { return nil }

func (s *FilesystemTraceStore) listRuns(RunFilter) ([]RunMetadata, error) {
	return nil, fmt.Errorf("filesystem trace store requires Linux openat2 confinement")
}
