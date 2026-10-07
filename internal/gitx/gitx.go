// Package gitx runs git for one working copy. It never prompts, never opens an
// editor, and classifies the failures callers need to act on: a rejected push, a
// rebase conflict, and an unreachable remote.
package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// NetworkTimeout bounds fetch, push and clone.
const NetworkTimeout = 20 * time.Second

var (
	// ErrRejected means the remote has commits this copy does not.
	ErrRejected = errors.New("push rejected: the remote has newer commits")
	// ErrConflict means a rebase stopped on conflicting edits.
	ErrConflict = errors.New("rebase conflict")
	// ErrOffline means the remote could not be reached.
	ErrOffline = errors.New("remote unreachable")
)

// Binary locates git. Clients may start the server with a minimal PATH, so a few
// standard install locations are tried after PATH.
func Binary() (string, error) {
	if p, err := exec.LookPath("git"); err == nil {
		return p, nil
	}
	for _, p := range []string{"/usr/bin/git", "/opt/homebrew/bin/git", "/usr/local/bin/git"} {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p, nil
		}
	}
	return "", errors.New("git is not installed or not on PATH")
}

// Repo is one working copy.
type Repo struct {
	Dir string
	git string
}

// Open returns a Repo for dir without checking that it exists.
func Open(dir string) (*Repo, error) {
	bin, err := Binary()
	if err != nil {
		return nil, err
	}
	return &Repo{Dir: dir, git: bin}, nil
}

// Error carries git's own output for diagnosis.
type Error struct {
	Args   []string
	Stderr string
	Kind   error // ErrRejected, ErrConflict, ErrOffline or nil
	err    error
}

func (e *Error) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if msg == "" {
		msg = e.err.Error()
	}
	return fmt.Sprintf("git %s: %s", strings.Join(e.Args, " "), msg)
}

func (e *Error) Unwrap() []error {
	if e.Kind != nil {
		return []error{e.Kind, e.err}
	}
	return []error{e.err}
}

// Run runs git in the working copy and returns trimmed stdout.
func (r *Repo) Run(ctx context.Context, args ...string) (string, error) {
	return r.run(ctx, r.Dir, args...)
}

func (r *Repo) run(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, r.git, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_EDITOR=true",
		"GIT_SEQUENCE_EDITOR=true",
		"GIT_MERGE_AUTOEDIT=no",
		"GCM_INTERACTIVE=never",
		"LC_ALL=C",
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		// Rebase reports conflicts on stdout, so classify both streams.
		out := stderr.String() + stdout.String()
		return "", &Error{Args: args, Stderr: out, Kind: classify(out, ctx.Err()), err: err}
	}
	return strings.TrimSpace(stdout.String()), nil
}

// classify matches git's own wording. Paths appear in git's output too, so the
// conflict check relies on git's capitalised marker and not the bare word.
func classify(output string, ctxErr error) error {
	s := strings.ToLower(output)
	switch {
	case strings.Contains(s, "[rejected]") || strings.Contains(s, "non-fast-forward") || strings.Contains(s, "fetch first"),
		// Another push won the race for the ref; retrying after a fetch can succeed.
		strings.Contains(s, "[remote rejected]") && (strings.Contains(s, "cannot lock ref") || strings.Contains(s, "incorrect old value")):
		return ErrRejected
	case strings.Contains(output, "CONFLICT (") || strings.Contains(output, "error: could not apply"):
		return ErrConflict
	case ctxErr != nil,
		strings.Contains(s, "could not resolve host"),
		strings.Contains(s, "could not read from remote"),
		strings.Contains(s, "unable to access"),
		strings.Contains(s, "connection refused"),
		strings.Contains(s, "connection timed out"),
		strings.Contains(s, "network is unreachable"),
		strings.Contains(s, "operation timed out"):
		return ErrOffline
	}
	return nil
}

// Clone clones url into dir, which must not exist yet.
func Clone(ctx context.Context, url, dir string) (*Repo, error) {
	r, err := Open(dir)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, NetworkTimeout)
	defer cancel()
	if _, err := r.run(ctx, "", "clone", "--quiet", url, dir); err != nil {
		return nil, err
	}
	return r, nil
}

// Init creates a repository with main as its branch.
func Init(ctx context.Context, dir string) (*Repo, error) {
	r, err := Open(dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if _, err := r.Run(ctx, "init", "--quiet", "--initial-branch=main"); err != nil {
		return nil, err
	}
	return r, nil
}

// Remote returns origin's URL, or "" when there is no origin.
func (r *Repo) Remote(ctx context.Context) string {
	url, err := r.Run(ctx, "remote", "get-url", "origin")
	if err != nil {
		return ""
	}
	return url
}

// HasCommits reports whether HEAD points at a commit.
func (r *Repo) HasCommits(ctx context.Context) bool {
	_, err := r.Run(ctx, "rev-parse", "--verify", "--quiet", "HEAD")
	return err == nil
}

// Branch returns the current branch name.
func (r *Repo) Branch(ctx context.Context) (string, error) {
	return r.Run(ctx, "symbolic-ref", "--short", "HEAD")
}

// Head returns the commit HEAD points at.
func (r *Repo) Head(ctx context.Context) (string, error) {
	return r.Run(ctx, "rev-parse", "HEAD")
}

// Dirty lists paths with uncommitted changes, including untracked files.
func (r *Repo) Dirty(ctx context.Context) ([]string, error) {
	out, err := r.Run(ctx, "status", "--porcelain")
	if err != nil || out == "" {
		return nil, err
	}
	var paths []string
	for line := range strings.SplitSeq(out, "\n") {
		if len(line) > 3 {
			paths = append(paths, line[3:])
		}
	}
	return paths, nil
}

// Commit stages exactly paths and commits them. Signing is off because a signing
// prompt would hang a background server. When git has no identity configured, it
// commits as Tallyfold so a missing global config never loses a write.
func (r *Repo) Commit(ctx context.Context, message string, paths ...string) error {
	if _, err := r.Run(ctx, append([]string{"add", "--"}, paths...)...); err != nil {
		return err
	}
	args := []string{"-c", "commit.gpgsign=false", "commit", "--quiet", "--no-verify", "-m", message, "--"}
	if name, _ := r.Run(ctx, "config", "user.name"); name == "" {
		args = append([]string{"-c", "user.name=Tallyfold", "-c", "user.email=tallyfold@users.noreply.github.com"}, args...)
	}
	_, err := r.Run(ctx, append(args, paths...)...)
	return err
}

// Fetch updates origin's refs.
func (r *Repo) Fetch(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, NetworkTimeout)
	defer cancel()
	_, err := r.Run(ctx, "fetch", "--quiet", "origin")
	return err
}

// Upstream returns origin's copy of the current branch, or "" when origin does
// not have it yet.
func (r *Repo) Upstream(ctx context.Context) string {
	branch, err := r.Branch(ctx)
	if err != nil {
		return ""
	}
	ref := "refs/remotes/origin/" + branch
	if _, err := r.Run(ctx, "rev-parse", "--verify", "--quiet", ref); err != nil {
		return ""
	}
	return ref
}

// AheadBehind counts local commits missing from upstream and the reverse.
func (r *Repo) AheadBehind(ctx context.Context, upstream string) (ahead, behind int, err error) {
	out, err := r.Run(ctx, "rev-list", "--left-right", "--count", "HEAD..."+upstream)
	if err != nil {
		return 0, 0, err
	}
	_, err = fmt.Sscanf(out, "%d %d", &ahead, &behind)
	return ahead, behind, err
}

// Prefer chooses whose edit wins when a rebase meets a conflicting line.
type Prefer int

const (
	PreferNone   Prefer = iota // stop and report the conflict
	PreferRemote               // keep the remote's line
	PreferLocal                // keep this copy's line
)

// Rebase replays local commits onto upstream. On a conflict with PreferNone it
// aborts, leaving the copy as it was, and returns the conflicting hunks.
func (r *Repo) Rebase(ctx context.Context, upstream string, prefer Prefer) (conflicts string, err error) {
	args := []string{"-c", "commit.gpgsign=false", "rebase", "--quiet"}
	switch prefer {
	case PreferRemote:
		args = append(args, "-X", "ours") // during a rebase "ours" is the upstream side
	case PreferLocal:
		args = append(args, "-X", "theirs")
	}
	if _, err := r.Run(ctx, append(args, upstream)...); err != nil {
		if !errors.Is(err, ErrConflict) {
			_, _ = r.Run(ctx, "rebase", "--abort")
			return "", err
		}
		conflicts, _ = r.Run(ctx, "diff")
		_, _ = r.Run(ctx, "rebase", "--abort")
		return conflicts, err
	}
	return "", nil
}

// Push pushes the current branch to origin and sets it as upstream.
func (r *Repo) Push(ctx context.Context) error {
	branch, err := r.Branch(ctx)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, NetworkTimeout)
	defer cancel()
	_, err = r.Run(ctx, "push", "--quiet", "--set-upstream", "origin", branch)
	return err
}

// ResetTo moves the branch to commit and discards later work. Callers use it only
// to drop a commit they made themselves.
func (r *Repo) ResetTo(ctx context.Context, commit string) error {
	_, err := r.Run(ctx, "reset", "--quiet", "--hard", commit)
	return err
}
