// Package mount finds the memory repos a session can use and how each is shared.
//
// A mount is one local clone of a memory repo, with a role that says who owns it
// and how its changes reach other sessions. This version implements the personal
// role. The other roles are named here so that a mount list written for a later
// version loads cleanly: their mounts are reported and skipped.
package mount

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/swiftugandan/tallyfold/internal/gitx"
)

// Role says who owns a mount and how its writes are shared.
type Role string

const (
	Personal Role = "personal" // one user's private repo; direct push
	Team     Role = "team"     // shared repo; writes go through proposals
	Swarm    Role = "swarm"    // a branch shared by parallel agents
	Guest    Role = "guest"    // someone else's personal repo, read-only
)

// Supported reports whether this version can mount the role.
func (r Role) Supported() bool { return r == Personal }

// PersonalName is the mount name of the personal repo.
const PersonalName = "personal"

// ConfigPath is where the personal repo lists further mounts.
const ConfigPath = ".tallyfold/mounts.toml"

// Mount is one memory repo cloned into the data directory.
type Mount struct {
	Name string
	Role Role
	Dir  string
	Repo *gitx.Repo
	lock string
}

// Layout places mounts inside the plugin data directory.
type Layout struct{ Data string }

// DataDir resolves the plugin data directory: an explicit value first, then the
// Agent Plugins variable, then Claude Code's own.
func DataDir(explicit string) (string, error) {
	for _, d := range []string{explicit, os.Getenv("PLUGIN_DATA"), os.Getenv("CLAUDE_PLUGIN_DATA")} {
		if d != "" {
			return filepath.Abs(d)
		}
	}
	return "", errors.New("no data directory: run tallyfold from an agent client, or pass --data-dir")
}

func (l Layout) dir(name string) string  { return filepath.Join(l.Data, "mounts", name) }
func (l Layout) lock(name string) string { return filepath.Join(l.Data, "mounts", name+".lock") }

// PersonalDir is where the personal clone lives.
func (l Layout) PersonalDir() string { return l.dir(PersonalName) }

// ErrNotSetUp means there is no personal memory repo yet.
var ErrNotSetUp = errors.New("tallyfold has no memory repo yet: set one up with memory_init (or `tallyfold init`)")

// Personal returns the personal mount.
func (l Layout) Personal() (*Mount, error) {
	dir := l.PersonalDir()
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		return nil, ErrNotSetUp
	}
	return l.open(PersonalName, Personal)
}

func (l Layout) open(name string, role Role) (*Mount, error) {
	repo, err := gitx.Open(l.dir(name))
	if err != nil {
		return nil, err
	}
	return &Mount{Name: name, Role: role, Dir: l.dir(name), Repo: repo, lock: l.lock(name)}, nil
}

// Spec is one entry in the mount list.
type Spec struct {
	Name string `toml:"name"`
	URL  string `toml:"url"`
	Role Role   `toml:"role"`
}

type config struct {
	Mounts []Spec `toml:"mount"`
}

// Load returns every usable mount, starting with the personal one, plus notes about
// listed mounts this version skips.
func (l Layout) Load() (mounts []*Mount, notes []string, err error) {
	personal, err := l.Personal()
	if err != nil {
		return nil, nil, err
	}
	mounts = append(mounts, personal)

	var cfg config
	path := filepath.Join(personal.Dir, ConfigPath)
	if _, statErr := os.Stat(path); statErr == nil {
		if _, err := toml.DecodeFile(path, &cfg); err != nil {
			notes = append(notes, fmt.Sprintf("%s could not be read (%v); only personal memory is loaded", ConfigPath, err))
		}
	}
	for _, s := range cfg.Mounts {
		switch {
		case s.Role == Personal:
			notes = append(notes, fmt.Sprintf("mount %q is listed as personal; the personal mount is this repo, so the entry is skipped", s.Name))
		case !s.Role.Supported():
			notes = append(notes, fmt.Sprintf("mount %q has role %q, which this version of Tallyfold does not support yet; it is skipped", s.Name, s.Role))
		}
	}
	return mounts, notes, nil
}

// LockTimeout bounds how long a write waits for another session on the same mount.
const LockTimeout = 30 * time.Second

// Lock serialises changes to the mount across processes on this machine.
func (m *Mount) Lock(ctx context.Context) (unlock func(), err error) {
	if err := os.MkdirAll(filepath.Dir(m.lock), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(m.lock, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(LockTimeout)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) || time.Now().After(deadline) {
			_ = f.Close()
			return nil, fmt.Errorf("another session is still writing to %s memory: %w", m.Name, err)
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}
