//go:build linux

package tracestore

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

const (
	resolveConfinement = unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS
)

func initializeFilesystemRoot(root string) (string, int, error) {
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return "", -1, fmt.Errorf("resolve trace root: %w", err)
	}
	canonicalRoot, rootFD, err := openOrCreateRoot(absoluteRoot)
	if err != nil {
		return "", -1, err
	}
	if err := unix.Mkdirat(rootFD, "traces", 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
		unix.Close(rootFD)
		return "", -1, fmt.Errorf("create traces dir: %w", err)
	}
	tracesFD, err := openBeneath(rootFD, "traces", unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		unix.Close(rootFD)
		return "", -1, fmt.Errorf("open traces dir: %w", err)
	}
	if err := unix.Fchmod(tracesFD, 0o700); err != nil {
		unix.Close(tracesFD)
		unix.Close(rootFD)
		return "", -1, fmt.Errorf("set traces dir permissions: %w", err)
	}
	unix.Close(tracesFD)
	return canonicalRoot, rootFD, nil
}

func openOrCreateRoot(absoluteRoot string) (string, int, error) {
	existing := filepath.Clean(absoluteRoot)
	var missing []string
	for {
		canonical, err := filepath.EvalSymlinks(existing)
		if err == nil {
			anchorFD, openErr := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
			if openErr != nil {
				return "", -1, fmt.Errorf("open filesystem root: %w", openErr)
			}
			canonicalPath := strings.TrimPrefix(canonical, "/")
			if canonicalPath == "" {
				canonicalPath = "."
			}
			fd, openErr := openBeneath(anchorFD, canonicalPath, unix.O_RDONLY|unix.O_DIRECTORY, 0)
			unix.Close(anchorFD)
			if openErr != nil {
				return "", -1, fmt.Errorf("open canonical trace root: %w", openErr)
			}
			for i := len(missing) - 1; i >= 0; i-- {
				part := missing[i]
				if mkdirErr := unix.Mkdirat(fd, part, 0o700); mkdirErr != nil && !errors.Is(mkdirErr, unix.EEXIST) {
					unix.Close(fd)
					return "", -1, fmt.Errorf("create trace root: %w", mkdirErr)
				}
				nextFD, nextErr := openBeneath(fd, part, unix.O_RDONLY|unix.O_DIRECTORY, 0)
				unix.Close(fd)
				if nextErr != nil {
					return "", -1, fmt.Errorf("open trace root component: %w", nextErr)
				}
				fd = nextFD
				canonical = filepath.Join(canonical, part)
			}
			return canonical, fd, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", -1, fmt.Errorf("resolve trace root symlinks: %w", err)
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			return "", -1, fmt.Errorf("resolve trace root: no existing ancestor")
		}
		missing = append(missing, filepath.Base(existing))
		existing = parent
	}
}

func openBeneath(dirFD int, path string, flags int, mode uint32) (int, error) {
	if flags&unix.O_CREAT == 0 {
		mode = 0
	}
	fd, err := unix.Openat2(dirFD, path, &unix.OpenHow{
		Flags:   uint64(flags | unix.O_CLOEXEC),
		Mode:    uint64(mode),
		Resolve: resolveConfinement,
	})
	if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EINVAL) {
		return -1, fmt.Errorf("secure filesystem confinement requires openat2: %w", err)
	}
	return fd, err
}

func (s *FilesystemTraceStore) openTraces() (int, error) {
	return openBeneath(s.rootFD, "traces", unix.O_RDONLY|unix.O_DIRECTORY, 0)
}

func (s *FilesystemTraceStore) openRun(runID string) (int, error) {
	return openBeneath(s.rootFD, filepath.Join("traces", runID), unix.O_RDONLY|unix.O_DIRECTORY, 0)
}

func (s *FilesystemTraceStore) createRunDir(runID string, metadata []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureOpen(); err != nil {
		return err
	}

	tracesFD, err := s.openTraces()
	if err != nil {
		return fmt.Errorf("open traces dir: %w", err)
	}
	defer unix.Close(tracesFD)
	if err := unix.Mkdirat(tracesFD, runID, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
		return fmt.Errorf("create run dir: %w", err)
	}
	runFD, err := openBeneath(tracesFD, runID, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return fmt.Errorf("open run dir: %w", err)
	}
	defer unix.Close(runFD)
	if err := unix.Fchmod(runFD, 0o700); err != nil {
		return fmt.Errorf("set run dir permissions: %w", err)
	}
	if err := atomicWriteAt(runFD, "metadata.json", metadata); err != nil {
		return fmt.Errorf("write metadata: %w", err)
	}
	return nil
}

// appendTrace appends data to a category file in O(len(data)) using an
// O_APPEND descriptor, instead of rewriting the whole file per record. When
// the active chunk would exceed the per-file limit it is rotated to
// "<name>.NNN" and a fresh chunk is started; once every rotation slot is
// used, appends fail with ErrTraceCategoryFull.
func (s *FilesystemTraceStore) appendTrace(runID, name string, data []byte) error {
	if int64(len(data)) > s.maxAppendFileBytes {
		return fmt.Errorf("append %s (%d bytes): %w", name, len(data), ErrTraceEventTooLarge)
	}
	runFD, err := s.openRun(runID)
	if err != nil {
		return fmt.Errorf("open run dir: %w", err)
	}
	defer unix.Close(runFD)

	for {
		fd, err := openBeneath(runFD, name,
			unix.O_WRONLY|unix.O_APPEND|unix.O_CREAT|unix.O_NOFOLLOW, 0o600)
		if err != nil {
			return fmt.Errorf("open trace file: %w", err)
		}
		var st unix.Stat_t
		if err := unix.Fstat(fd, &st); err != nil {
			unix.Close(fd)
			return fmt.Errorf("stat trace file: %w", err)
		}
		if st.Mode&unix.S_IFMT != unix.S_IFREG {
			unix.Close(fd)
			return fmt.Errorf("trace file must be a regular file")
		}
		if st.Nlink != 1 {
			// Never write through a hardlinked target: rotate it aside and
			// continue in a fresh, single-link chunk.
			unix.Close(fd)
			if err := s.rotateTraceFile(runFD, name); err != nil {
				return err
			}
			continue
		}
		if st.Size+int64(len(data)) > s.maxAppendFileBytes {
			unix.Close(fd)
			if err := s.rotateTraceFile(runFD, name); err != nil {
				return err
			}
			continue
		}
		created := st.Size == 0
		remaining := data
		for len(remaining) > 0 {
			n, writeErr := unix.Write(fd, remaining)
			if writeErr != nil {
				unix.Close(fd)
				return fmt.Errorf("append trace: %w", writeErr)
			}
			remaining = remaining[n:]
		}
		if err := unix.Fdatasync(fd); err != nil {
			unix.Close(fd)
			return fmt.Errorf("sync trace file: %w", err)
		}
		unix.Close(fd)
		if created {
			if err := unix.Fsync(runFD); err != nil {
				return fmt.Errorf("sync run directory: %w", err)
			}
		}
		return nil
	}
}

// rotateTraceFile renames the active chunk to the first free "<name>.NNN"
// slot. Rotation slots bound total per-category storage to
// (maxRotations+1) x maxAppendFileBytes.
func (s *FilesystemTraceStore) rotateTraceFile(runFD int, name string) error {
	for i := 1; i <= s.maxRotations; i++ {
		rotated := fmt.Sprintf("%s.%03d", name, i)
		fd, err := openBeneath(runFD, rotated, unix.O_RDONLY|unix.O_NOFOLLOW, 0)
		if err == nil {
			unix.Close(fd)
			continue
		}
		if !errors.Is(err, unix.ENOENT) {
			return fmt.Errorf("probe rotated trace file: %w", err)
		}
		if err := unix.Renameat(runFD, name, runFD, rotated); err != nil {
			return fmt.Errorf("rotate trace file: %w", err)
		}
		if err := unix.Fsync(runFD); err != nil {
			return fmt.Errorf("sync run directory: %w", err)
		}
		return nil
	}
	return fmt.Errorf("append %s: %w", name, ErrTraceCategoryFull)
}

func (s *FilesystemTraceStore) writeFile(runID, relPath string, data []byte) error {
	runFD, err := s.openRun(runID)
	if err != nil {
		return fmt.Errorf("open run dir: %w", err)
	}
	defer unix.Close(runFD)
	parts := splitTracePath(relPath)
	parentFD := runFD
	for _, part := range parts[:len(parts)-1] {
		if err := unix.Mkdirat(parentFD, part, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
			if parentFD != runFD {
				unix.Close(parentFD)
			}
			return fmt.Errorf("create trace directory: %w", err)
		}
		nextFD, err := openBeneath(parentFD, part, unix.O_RDONLY|unix.O_DIRECTORY, 0)
		if parentFD != runFD {
			unix.Close(parentFD)
		}
		if err != nil {
			return fmt.Errorf("open trace directory: %w", err)
		}
		if err := unix.Fchmod(nextFD, 0o700); err != nil {
			unix.Close(nextFD)
			return fmt.Errorf("set trace directory permissions: %w", err)
		}
		parentFD = nextFD
	}
	if parentFD != runFD {
		defer unix.Close(parentFD)
	}
	return atomicWriteAt(parentFD, parts[len(parts)-1], data)
}

func splitTracePath(path string) []string {
	return strings.Split(path, string(filepath.Separator))
}

func atomicWriteAt(parentFD int, name string, data []byte) error {
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		return fmt.Errorf("generate temporary file name: %w", err)
	}
	tempName := ".trace.tmp-" + hex.EncodeToString(random)
	fd, err := unix.Openat(parentFD, tempName, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	keep := false
	defer func() {
		unix.Close(fd)
		if !keep {
			unix.Unlinkat(parentFD, tempName, 0)
		}
	}()
	for len(data) > 0 {
		n, writeErr := unix.Write(fd, data)
		if writeErr != nil {
			return fmt.Errorf("write temporary file: %w", writeErr)
		}
		data = data[n:]
	}
	if err := unix.Fsync(fd); err != nil {
		return fmt.Errorf("sync temporary file: %w", err)
	}
	if err := unix.Renameat(parentFD, tempName, parentFD, name); err != nil {
		return fmt.Errorf("replace file: %w", err)
	}
	keep = true
	if err := unix.Fsync(parentFD); err != nil {
		return fmt.Errorf("sync parent directory: %w", err)
	}
	return nil
}

func (s *FilesystemTraceStore) readMetadata(runID string) ([]byte, error) {
	runFD, err := s.openRun(runID)
	if err != nil {
		return nil, err
	}
	defer unix.Close(runFD)
	fd, err := openBeneath(runFD, "metadata.json", unix.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), "metadata.json")
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("metadata is not a regular file")
	}
	return io.ReadAll(file)
}

func (s *FilesystemTraceStore) writeMetadata(runID string, data []byte) error {
	runFD, err := s.openRun(runID)
	if err != nil {
		return err
	}
	defer unix.Close(runFD)
	return atomicWriteAt(runFD, "metadata.json", data)
}

func (s *FilesystemTraceStore) verifyRunDir(runID, namedPath string) error {
	pinnedFD, err := s.openRun(runID)
	if err != nil {
		return err
	}
	defer unix.Close(pinnedFD)
	namedFD, err := unix.Open(namedPath, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open named run dir: %w", err)
	}
	defer unix.Close(namedFD)
	var pinned, named unix.Stat_t
	if err := unix.Fstat(pinnedFD, &pinned); err != nil {
		return fmt.Errorf("stat pinned run dir: %w", err)
	}
	if err := unix.Fstat(namedFD, &named); err != nil {
		return fmt.Errorf("stat named run dir: %w", err)
	}
	if pinned.Dev != named.Dev || pinned.Ino != named.Ino {
		return fmt.Errorf("trace root changed during run directory lookup")
	}
	return nil
}

func closeFilesystemRoot(fd int) error {
	if fd < 0 {
		return nil
	}
	return unix.Close(fd)
}

func (s *FilesystemTraceStore) listRuns(filter RunFilter) ([]RunMetadata, error) {
	tracesFD, err := s.openTraces()
	if err != nil {
		return nil, fmt.Errorf("read traces dir: %w", err)
	}
	file := os.NewFile(uintptr(tracesFD), "traces")
	defer file.Close()
	entries, err := file.Readdirnames(-1)
	if err != nil {
		return nil, fmt.Errorf("read traces dir: %w", err)
	}
	var runs []RunMetadata
	for _, name := range entries {
		if _, err := safeTraceName("run id", name); err != nil {
			continue
		}
		data, err := s.readMetadata(name)
		if err != nil {
			continue
		}
		var meta RunMetadata
		if jsonErr := json.Unmarshal(data, &meta); jsonErr != nil {
			continue
		}
		if filter.CandidateID != "" && meta.CandidateID != filter.CandidateID {
			continue
		}
		if !filter.Since.IsZero() && meta.StartedAt.Before(filter.Since) {
			continue
		}
		runs = append(runs, meta)
	}
	return runs, nil
}
