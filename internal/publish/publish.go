// Package publish decides how a mount's local commits reach other sessions.
//
// Each mount role has a Policy. Personal memory pushes straight to its branch.
// Team memory (proposal branches) and swarm memory (a shared branch with
// append-only logs) will add their own policies here; until then For reports
// them as unsupported.
package publish

import (
	"context"
	"errors"
	"fmt"

	"github.com/swiftugandan/tallyfold/internal/gitx"
	"github.com/swiftugandan/tallyfold/internal/mount"
)

// State says where a change ended up.
type State string

const (
	Pushed  State = "pushed"  // on the remote
	Pending State = "pending" // committed here; the remote was unreachable, the next sync pushes it
	Local   State = "local"   // committed here; the repo has no remote
)

// Outcome describes the result of publishing or syncing.
type Outcome struct {
	State  State
	Detail string
}

// ConflictError carries the conflicting hunks so an agent can write one merged
// entry and try again.
type ConflictError struct {
	Diff string
	// Dropped is true when the write that caused the conflict was undone.
	Dropped bool
}

func (e *ConflictError) Error() string {
	if e.Dropped {
		return "another session changed the same lines first, so this write was undone; read the current file, then write one merged entry\n" + e.Diff
	}
	return "local and remote memory changed the same lines; sync again with prefer=remote or prefer=local\n" + e.Diff
}

// Policy shares a mount's commits.
type Policy interface {
	// Sync brings the mount up to date and shares any commits made earlier.
	Sync(ctx context.Context, m *mount.Mount, prefer gitx.Prefer) (Outcome, error)
	// Publish shares commit made, which the caller has just created. If made
	// cannot be shared without a conflict, Publish undoes it and returns a
	// *ConflictError with Dropped set.
	Publish(ctx context.Context, m *mount.Mount, made string) (Outcome, error)
}

// ErrUnsupported means this version has no policy for the role.
var ErrUnsupported = errors.New("not supported in this version of Tallyfold")

// For returns the policy for a role.
func For(role mount.Role) (Policy, error) {
	if role == mount.Personal {
		return DirectPush{Attempts: 3}, nil
	}
	return nil, fmt.Errorf("%s memory is %w", role, ErrUnsupported)
}

// DirectPush pushes commits straight to the mount's branch, rebasing onto newer
// remote commits first. It never force-pushes.
type DirectPush struct{ Attempts int }

// Sync fetches, rebases local commits onto the remote, and pushes them.
func (p DirectPush) Sync(ctx context.Context, m *mount.Mount, prefer gitx.Prefer) (Outcome, error) {
	r := m.Repo
	if r.Remote(ctx) == "" {
		return Outcome{Local, "no remote configured"}, nil
	}
	for range p.Attempts {
		if err := r.Fetch(ctx); err != nil {
			if errors.Is(err, gitx.ErrOffline) {
				return Outcome{Pending, "remote unreachable; using the local copy"}, nil
			}
			return Outcome{}, err
		}
		up := r.Upstream(ctx)
		if up != "" {
			ahead, behind, err := r.AheadBehind(ctx, up)
			if err != nil {
				return Outcome{}, err
			}
			if behind > 0 {
				if diff, err := r.Rebase(ctx, up, prefer); err != nil {
					if errors.Is(err, gitx.ErrConflict) {
						return Outcome{}, &ConflictError{Diff: diff}
					}
					return Outcome{}, err
				}
			}
			if ahead == 0 {
				return Outcome{Pushed, "up to date"}, nil
			}
		} else if !r.HasCommits(ctx) {
			return Outcome{Pushed, "empty repo"}, nil
		}
		err := r.Push(ctx)
		switch {
		case err == nil:
			return Outcome{Pushed, "pushed local changes"}, nil
		case errors.Is(err, gitx.ErrRejected):
			continue // someone pushed between fetch and push; go round again
		case errors.Is(err, gitx.ErrOffline):
			return Outcome{Pending, "remote unreachable; local changes will be pushed at the next sync"}, nil
		default:
			return Outcome{}, err
		}
	}
	return Outcome{Pending, "the remote kept changing; local changes will be pushed at the next sync"}, nil
}

// Publish pushes made, rebasing onto newer remote commits when the push is rejected.
func (p DirectPush) Publish(ctx context.Context, m *mount.Mount, made string) (Outcome, error) {
	r := m.Repo
	if r.Remote(ctx) == "" {
		return Outcome{Local, "saved in the local memory repo (no remote configured)"}, nil
	}
	for range p.Attempts {
		err := r.Push(ctx)
		switch {
		case err == nil:
			return Outcome{Pushed, "saved and pushed"}, nil
		case errors.Is(err, gitx.ErrOffline):
			return Outcome{Pending, "saved locally; the remote was unreachable, so the next sync will push it"}, nil
		case !errors.Is(err, gitx.ErrRejected):
			return Outcome{}, err
		}
		if err := r.Fetch(ctx); err != nil {
			if errors.Is(err, gitx.ErrOffline) {
				return Outcome{Pending, "saved locally; the remote was unreachable, so the next sync will push it"}, nil
			}
			return Outcome{}, err
		}
		diff, err := r.Rebase(ctx, r.Upstream(ctx), gitx.PreferNone)
		if errors.Is(err, gitx.ErrConflict) {
			// The rebase was aborted, so HEAD is made again: drop only that commit.
			if head, _ := r.Head(ctx); head == made {
				if err := r.ResetTo(ctx, made+"~1"); err != nil {
					return Outcome{}, err
				}
				return Outcome{}, &ConflictError{Diff: diff, Dropped: true}
			}
			return Outcome{}, &ConflictError{Diff: diff}
		}
		if err != nil {
			return Outcome{}, err
		}
	}
	return Outcome{Pending, "saved locally; the remote kept changing, so the next sync will push it"}, nil
}
