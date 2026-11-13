package logging

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultMaxSize is the size at which the owning process starts a new log file.
	DefaultMaxSize = 10 << 20
	// DefaultKeepDays and DefaultKeepFiles bound the rotated files that are kept.
	DefaultKeepDays  = 3
	DefaultKeepFiles = 5

	// checkInterval is how often a writer looks at the file on disk to see
	// whether another process rotated it.
	checkInterval = time.Second
	// rotateRetryDelay is how long the owner waits after a failed rotation.
	rotateRetryDelay = time.Minute
)

// FileOptions configures a RotatingFile.
type FileOptions struct {
	// Owner marks the one process that rotates the file (the daemon or the
	// manager service). Every other process only follows rotations.
	Owner bool
	// FileMode is the mode of a newly created log file (Unix only).
	FileMode os.FileMode
	// MaxSize is the size that triggers rotation by the owner; 0 uses DefaultMaxSize.
	MaxSize int64
	// KeepDays and KeepFiles bound the rotated files; 0 uses the defaults.
	KeepDays  int
	KeepFiles int
}

// RotatingFile is an append-only log file that may be shared by several
// processes. The owner renames it to a dated name when it grows past MaxSize
// and every writer notices the rename and reopens the path, so nobody keeps
// writing into a rotated file.
type RotatingFile struct {
	path string
	opts FileOptions
	now  func() time.Time

	mu        sync.Mutex
	f         *os.File
	size      int64
	lastCheck time.Time

	noRotateUntil time.Time
}

// OpenRotatingFile opens (creating it if needed) the log file at path. When
// the owner opens an existing file from an earlier run that is already past
// MaxSize or from an earlier day, it is rotated first.
func OpenRotatingFile(path string, opts FileOptions) (*RotatingFile, error) {
	if opts.MaxSize <= 0 {
		opts.MaxSize = DefaultMaxSize
	}
	if opts.KeepDays <= 0 {
		opts.KeepDays = DefaultKeepDays
	}
	if opts.KeepFiles <= 0 {
		opts.KeepFiles = DefaultKeepFiles
	}
	if opts.FileMode == 0 {
		opts.FileMode = 0o600
	}
	r := &RotatingFile{path: path, opts: opts, now: time.Now}
	r.mu.Lock()
	defer r.mu.Unlock()
	if opts.Owner {
		// A failure here only means the old file keeps growing; carry on.
		_ = r.rotateIfStaleLocked()
	}
	if err := r.openLocked(); err != nil {
		return nil, err
	}
	return r, nil
}

// Path is the path of the live log file.
func (r *RotatingFile) Path() string { return r.path }

// Write appends p to the log file, rotating or reopening it first if needed.
func (r *RotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.now()
	if r.f == nil || now.Sub(r.lastCheck) >= checkInterval {
		r.lastCheck = now
		r.syncLocked()
	}
	if r.f == nil {
		if err := r.openLocked(); err != nil {
			return 0, err
		}
	}
	if r.opts.Owner && r.size > 0 && r.size+int64(len(p)) > r.opts.MaxSize && !now.Before(r.noRotateUntil) {
		if err := r.rotateLocked(); err != nil {
			// Keep writing to the current file rather than losing lines, and
			// do not retry the rename for every line.
			r.noRotateUntil = now.Add(rotateRetryDelay)
		}
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

// Close closes the file.
func (r *RotatingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return nil
	}
	err := r.f.Close()
	r.f = nil
	return err
}

// syncLocked compares the open file with what is at the path. If another
// process rotated or removed it, the handle is dropped so the next write
// reopens the path; otherwise the size is refreshed, since other processes
// append too.
func (r *RotatingFile) syncLocked() {
	if r.f == nil {
		return
	}
	onDisk, err := os.Stat(r.path)
	if err != nil {
		r.closeLocked()
		return
	}
	open, err := r.f.Stat()
	if err != nil || !os.SameFile(open, onDisk) {
		r.closeLocked()
		return
	}
	r.size = onDisk.Size()
}

func (r *RotatingFile) closeLocked() {
	if r.f != nil {
		_ = r.f.Close()
		r.f = nil
	}
}

func (r *RotatingFile) openLocked() error {
	f, err := openAppend(r.path, r.opts.FileMode)
	if err != nil {
		return err
	}
	if r.opts.Owner {
		// Files from older versions were created world-readable.
		restrictFile(f, r.opts.FileMode)
	}
	r.f = f
	r.size = 0
	if info, err := f.Stat(); err == nil {
		r.size = info.Size()
	}
	return nil
}

// rotateIfStaleLocked rotates a file left by an earlier run when it is from a
// previous day or already past MaxSize.
func (r *RotatingFile) rotateIfStaleLocked() error {
	info, err := os.Stat(r.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	now := r.now()
	mod := info.ModTime()
	sameDay := now.Year() == mod.Year() && now.YearDay() == mod.YearDay()
	if sameDay && info.Size() <= r.opts.MaxSize {
		return nil
	}
	return r.renameAwayLocked(mod)
}

// rotateLocked moves the live file aside and starts a new one.
func (r *RotatingFile) rotateLocked() error {
	r.closeLocked()
	renameErr := r.renameAwayLocked(r.now())
	if err := r.openLocked(); err != nil {
		return err
	}
	return renameErr
}

// renameAwayLocked renames the live file to pangolin-<time>.log and prunes old files.
func (r *RotatingFile) renameAwayLocked(stamp time.Time) error {
	dir := filepath.Dir(r.path)
	base := strings.TrimSuffix(filepath.Base(r.path), filepath.Ext(r.path))
	rotated := filepath.Join(dir, fmt.Sprintf("%s-%s.log", base, stamp.Format("2006-01-02T150405")))
	// Pick a name that is free. Any answer but "it exists" ends the search,
	// and so does a bound on the tries, since this runs under the write lock.
	for i := 1; i <= 100; i++ {
		if _, err := os.Lstat(rotated); err != nil {
			break
		}
		rotated = filepath.Join(dir, fmt.Sprintf("%s-%s-%d.log", base, stamp.Format("2006-01-02T150405"), i))
	}
	if err := os.Rename(r.path, rotated); err != nil {
		return fmt.Errorf("failed to rotate log file: %w", err)
	}
	pruneRotated(dir, base, r.opts.KeepDays, r.opts.KeepFiles, r.now())
	return nil
}

// pruneRotated removes rotated files older than keepDays and all but the
// keepFiles newest of the rest. The live file (no date in its name) is never touched.
func pruneRotated(dir, base string, keepDays, keepFiles int, now time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := now.AddDate(0, 0, -keepDays)
	type rotatedFile struct {
		path string
		mod  time.Time
	}
	var kept []rotatedFile
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, base+"-") || !strings.HasSuffix(name, ".log") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		p := filepath.Join(dir, name)
		if info.ModTime().Before(cutoff) {
			_ = os.Remove(p)
			continue
		}
		kept = append(kept, rotatedFile{p, info.ModTime()})
	}
	if len(kept) <= keepFiles {
		return
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].mod.After(kept[j].mod) })
	for _, f := range kept[keepFiles:] {
		_ = os.Remove(f.path)
	}
}
