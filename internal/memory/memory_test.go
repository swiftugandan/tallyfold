package memory_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/swiftugandan/tallyfold/internal/memory"
	"github.com/swiftugandan/tallyfold/internal/mount"
	"github.com/swiftugandan/tallyfold/internal/publish"
)

// These tests drive real git against bare repositories that stand in for a hosted
// remote, so they exercise the same commands a user's session runs.

func TestMain(m *testing.M) {
	// Keep the developer's global git config (signing, hooks, identity) out of the tests.
	dir, _ := os.MkdirTemp("", "tallyfold-gitconfig")
	cfg := filepath.Join(dir, "gitconfig")
	_ = os.WriteFile(cfg, []byte("[user]\n\tname = Test\n\temail = test@example.com\n[init]\n\tdefaultBranch = main\n"), 0o644)
	os.Setenv("GIT_CONFIG_GLOBAL", cfg)
	os.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

var ctx = context.Background()

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func bareRemote(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "memory.git")
	if out, err := exec.Command("git", "init", "--quiet", "--bare", "--initial-branch=main", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v\n%s", err, out)
	}
	return dir
}

// session is one client on one machine: its own data directory and clone.
func session(t *testing.T, remote string) *memory.Store {
	t.Helper()
	s := memory.Open(t.TempDir())
	s.Writer = "test"
	s.Now = func() time.Time { return time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC) }
	if _, err := s.Init(ctx, remote); err != nil {
		t.Fatalf("Init: %v", err)
	}
	return s
}

func read(t *testing.T, s *memory.Store, target string) string {
	t.Helper()
	out, err := s.Read(ctx, target)
	if err != nil {
		t.Fatalf("Read(%s): %v", target, err)
	}
	return out
}

func write(t *testing.T, s *memory.Store, req memory.WriteRequest) memory.WriteResult {
	t.Helper()
	res, err := s.Write(ctx, req)
	if err != nil {
		t.Fatalf("Write(%+v): %v", req, err)
	}
	return res
}

func TestInitStartsAnEmptyRemoteAndAdoptsIt(t *testing.T) {
	remote := bareRemote(t)
	first := session(t, remote)
	if got := git(t, remote, "log", "--format=%s", "main"); got != "Start Tallyfold memory" {
		t.Fatalf("remote history = %q, want the scaffold commit", got)
	}
	second := session(t, remote) // another machine finds the existing memory repo
	if read(t, second, "MEMORY.md") != read(t, first, "MEMORY.md") {
		t.Fatal("second session does not see the same MEMORY.md")
	}
}

func TestInitRefusesARepoThatIsNotMemory(t *testing.T) {
	remote := bareRemote(t)
	work := t.TempDir()
	git(t, work, "clone", "--quiet", remote, ".")
	os.WriteFile(filepath.Join(work, "main.go"), []byte("package main\n"), 0o644)
	git(t, work, "add", ".")
	git(t, work, "commit", "--quiet", "-m", "code")
	git(t, work, "push", "--quiet", "origin", "main")

	s := memory.Open(t.TempDir())
	if _, err := s.Init(ctx, remote); err == nil || !strings.Contains(err.Error(), "no MEMORY.md") {
		t.Fatalf("Init adopted a code repo: %v", err)
	}
	if s.SetUp() {
		t.Fatal("a refused clone was left behind")
	}
}

func TestLocalRepoCanBeGivenARemoteLater(t *testing.T) {
	s := session(t, "")
	write(t, s, memory.WriteRequest{Action: memory.Add, Text: "Prefers tables for comparisons"})
	remote := bareRemote(t)
	if _, err := s.Init(ctx, remote); err != nil {
		t.Fatalf("attach remote: %v", err)
	}
	if got := git(t, remote, "log", "-1", "--format=%s", "main"); got != "Remember: Prefers tables for comparisons" {
		t.Fatalf("remote head = %q", got)
	}
}

func TestAddPutsEntriesAboveTheIndexAndPushes(t *testing.T) {
	remote := bareRemote(t)
	s := session(t, remote)
	res := write(t, s, memory.WriteRequest{Action: memory.Add, Text: "Prefers short answers", Source: "claude-code:abc"})
	if res.Outcome.State != publish.Pushed {
		t.Fatalf("outcome = %+v, want pushed", res.Outcome)
	}
	want := "# Memory\n\n- Prefers short answers [source: claude-code:abc; added: 2026-10-07; by: test]\n\n## Index\n"
	if got := read(t, s, "MEMORY.md"); got != want {
		t.Fatalf("MEMORY.md =\n%s\nwant\n%s", got, want)
	}
	if got := git(t, remote, "show", "main:MEMORY.md"); got != strings.TrimSpace(want) {
		t.Fatalf("remote MEMORY.md =\n%s", got)
	}
}

func TestAddToANewTopicFileLinksItFromTheIndex(t *testing.T) {
	s := session(t, bareRemote(t))
	write(t, s, memory.WriteRequest{Action: memory.Add, File: "people", Text: "Priya owns pricing"})
	write(t, s, memory.WriteRequest{Action: memory.Add, File: "projects/payments.md", Title: "Payments", Text: "Hosted on Kubernetes only"})

	if got := read(t, s, "[[people]]"); got != "# People\n\n- Priya owns pricing [added: 2026-10-07; by: test]\n" {
		t.Fatalf("people.md =\n%s", got)
	}
	if got := read(t, s, "projects/payments"); !strings.HasPrefix(got, "# Payments\n\n- Hosted on Kubernetes only") {
		t.Fatalf("projects/payments.md =\n%s", got)
	}
	if got, want := read(t, s, "MEMORY.md"), "# Memory\n\n## Index\n\n- [[people]]\n- [[projects/payments]]\n"; got != want {
		t.Fatalf("MEMORY.md =\n%s\nwant\n%s", got, want)
	}
	if problems, _ := s.Lint(ctx); len(problems) != 0 {
		t.Fatalf("lint after writes: %v", problems)
	}
}

func TestAddingTheSameFactTwiceChangesNothing(t *testing.T) {
	remote := bareRemote(t)
	s := session(t, remote)
	write(t, s, memory.WriteRequest{Action: memory.Add, Text: "Uses mise for toolchains"})
	before := git(t, remote, "rev-parse", "main")
	res := write(t, s, memory.WriteRequest{Action: memory.Add, Text: "uses mise for toolchains"})
	if res.Outcome.Detail != "already remembered" || git(t, remote, "rev-parse", "main") != before {
		t.Fatalf("duplicate add made a change: %+v", res)
	}
}

func TestReplaceAndRemoveNeedExactlyOneMatch(t *testing.T) {
	s := session(t, bareRemote(t))
	write(t, s, memory.WriteRequest{Action: memory.Add, File: "tools", Text: "Uses Go 1.27 for services"})
	write(t, s, memory.WriteRequest{Action: memory.Add, File: "tools", Text: "Uses Go templates for docs"})

	if _, err := s.Write(ctx, memory.WriteRequest{Action: memory.Replace, File: "tools", Match: "Uses Go", Text: "x"}); err == nil || !strings.Contains(err.Error(), "2 entries") {
		t.Fatalf("ambiguous match accepted: %v", err)
	}
	write(t, s, memory.WriteRequest{Action: memory.Replace, File: "tools", Match: "Go 1.27", Text: "Uses Go 1.28 for services"})
	write(t, s, memory.WriteRequest{Action: memory.Remove, File: "tools", Match: "templates"})
	if got, want := read(t, s, "tools"), "# Tools\n\n- Uses Go 1.28 for services [added: 2026-10-07; by: test]\n"; got != want {
		t.Fatalf("tools.md =\n%s\nwant\n%s", got, want)
	}
}

func TestTwoSessionsWritingDifferentFactsBothLand(t *testing.T) {
	remote := bareRemote(t)
	a, b := session(t, remote), session(t, remote)
	write(t, a, memory.WriteRequest{Action: memory.Add, File: "people", Text: "Priya owns pricing"})
	// b has not seen a's commit; its write must rebase onto it, not overwrite it.
	write(t, b, memory.WriteRequest{Action: memory.Add, Text: "Prefers tables"})

	people := git(t, remote, "show", "main:people.md")
	index := git(t, remote, "show", "main:MEMORY.md")
	if !strings.Contains(people, "Priya owns pricing") || !strings.Contains(index, "Prefers tables") || !strings.Contains(index, "[[people]]") {
		t.Fatalf("a write was lost:\npeople.md:\n%s\nMEMORY.md:\n%s", people, index)
	}
}

func TestConflictingEditIsUndoneAndReported(t *testing.T) {
	remote := bareRemote(t)
	a := session(t, remote)
	write(t, a, memory.WriteRequest{Action: memory.Add, Text: "Deploys on Fridays"})
	b := session(t, remote)

	// Both sessions edit the same line. a pushes first; b's write is built on the
	// old line because b's pre-write sync ran before a's push in this sequence.
	bDir := b.Dir()
	write(t, a, memory.WriteRequest{Action: memory.Replace, Match: "Fridays", Text: "Deploys on Thursdays"})
	git(t, bDir, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "unreachable.git"))
	write(t, b, memory.WriteRequest{Action: memory.Replace, Match: "Fridays", Text: "Never deploys on Fridays"})
	git(t, bDir, "remote", "set-url", "origin", remote)

	_, err := b.Sync(ctx, 0)
	var conflict *publish.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("Sync = %v, want a conflict", err)
	}
	if !strings.Contains(conflict.Diff, "Thursdays") || !strings.Contains(conflict.Diff, "Never deploys") {
		t.Fatalf("conflict does not show both versions:\n%s", conflict.Diff)
	}
	// Choosing the remote's line resolves it and leaves both copies equal.
	if _, err := b.Sync(ctx, 1); err != nil { // gitx.PreferRemote
		t.Fatalf("Sync(prefer remote): %v", err)
	}
	if got := read(t, b, "MEMORY.md"); !strings.Contains(got, "Deploys on Thursdays") || strings.Contains(got, "Never deploys") {
		t.Fatalf("after resolving, MEMORY.md =\n%s", got)
	}
}

func TestPushRaceUndoesOnlyTheLosingWrite(t *testing.T) {
	remote := bareRemote(t)
	a := session(t, remote)
	write(t, a, memory.WriteRequest{Action: memory.Add, Text: "Deploys on Fridays"})
	b := session(t, remote)
	bDir := b.Dir()

	// Simulate a push that lands between b's pre-write sync and b's push: a hook in
	// b's clone runs a's write just before b pushes.
	aMem := filepath.Join(t.TempDir(), "a-edit")
	git(t, remote, "worktree", "add", "--quiet", "--detach", aMem, "main")
	os.WriteFile(filepath.Join(aMem, "MEMORY.md"), []byte(strings.Replace(git(t, aMem, "show", "HEAD:MEMORY.md"), "Fridays", "Thursdays", 1)+"\n"), 0o644)
	git(t, aMem, "commit", "--quiet", "-am", "race")
	hook := "#!/bin/sh\ngit -C " + aMem + " push --quiet " + remote + " HEAD:main 2>/dev/null || true\nexit 0\n"
	os.MkdirAll(filepath.Join(bDir, ".git", "hooks"), 0o755)
	os.WriteFile(filepath.Join(bDir, ".git", "hooks", "pre-push"), []byte(hook), 0o755)

	_, err := b.Write(ctx, memory.WriteRequest{Action: memory.Replace, Match: "Fridays", Text: "Never deploys on Fridays"})
	var conflict *publish.ConflictError
	if !errors.As(err, &conflict) || !conflict.Dropped {
		t.Fatalf("Write = %v, want a dropped conflict", err)
	}
	if ahead := git(t, bDir, "rev-list", "--count", "origin/main..HEAD"); ahead != "0" {
		t.Fatalf("the losing commit is still in b's clone (%s ahead)", ahead)
	}
	if dirty := git(t, bDir, "status", "--porcelain"); dirty != "" {
		t.Fatalf("b's clone is not clean: %s", dirty)
	}
}

func TestOfflineWritesArePushedAtTheNextSync(t *testing.T) {
	remote := bareRemote(t)
	s := session(t, remote)
	moved := remote + ".away"
	os.Rename(remote, moved)

	res := write(t, s, memory.WriteRequest{Action: memory.Add, Text: "Works from Kampala"})
	if res.Outcome.State != publish.Pending {
		t.Fatalf("offline write outcome = %+v, want pending", res.Outcome)
	}
	if ctxText, _ := s.Context(ctx, memory.ContextBudget); !strings.Contains(ctxText, "not pushed yet") {
		t.Fatalf("context does not mention the waiting change:\n%s", ctxText)
	}

	os.Rename(moved, remote)
	if _, err := s.Sync(ctx, 0); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if !strings.Contains(git(t, remote, "show", "main:MEMORY.md"), "Works from Kampala") {
		t.Fatal("the offline write never reached the remote")
	}
}

func TestWritesRefuseSecretsAndOversizedMemory(t *testing.T) {
	s := session(t, bareRemote(t))
	if _, err := s.Write(ctx, memory.WriteRequest{Action: memory.Add, Text: "CI token is ghp_" + strings.Repeat("a", 36)}); err == nil || !strings.Contains(err.Error(), "GitHub token") {
		t.Fatalf("secret accepted: %v", err)
	}
	big := strings.Repeat("word ", 120)
	var err error
	for i := 0; i < 10 && err == nil; i++ {
		_, err = s.Write(ctx, memory.WriteRequest{Action: memory.Add, Text: big + string(rune('a'+i))})
	}
	if err == nil || !strings.Contains(err.Error(), "over the 4000 limit") {
		t.Fatalf("MEMORY.md grew past its limit: %v", err)
	}
}

func TestOnlyPersonalScopeIsWritableAndOtherMountsAreReported(t *testing.T) {
	s := session(t, bareRemote(t))
	if _, err := s.Write(ctx, memory.WriteRequest{Scope: "team", Action: memory.Add, Text: "x"}); !errors.Is(err, publish.ErrUnsupported) {
		t.Fatalf("team write = %v, want ErrUnsupported", err)
	}

	cfg := "[[mount]]\nname = \"platform-team\"\nurl = \"https://example.com/memory-platform\"\nrole = \"team\"\n"
	dir := s.Dir()
	os.MkdirAll(filepath.Join(dir, ".tallyfold"), 0o755)
	os.WriteFile(filepath.Join(dir, mount.ConfigPath), []byte(cfg), 0o644)

	out, err := s.Context(ctx, memory.ContextBudget)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "## personal memory") || !strings.Contains(out, `mount "platform-team" has role "team"`) {
		t.Fatalf("context =\n%s", out)
	}
}

func TestReadStaysInsideTheRepo(t *testing.T) {
	s := session(t, bareRemote(t))
	for _, target := range []string{"../../etc/passwd", ".git/config", "/etc/hosts", "elsewhere:MEMORY.md"} {
		if _, err := s.Read(ctx, target); err == nil {
			t.Errorf("Read(%q) succeeded", target)
		}
	}
}

func TestSearchMatchesAllWords(t *testing.T) {
	s := session(t, bareRemote(t))
	write(t, s, memory.WriteRequest{Action: memory.Add, File: "tools", Text: "Release builds pin BuildKit 0.17"})
	write(t, s, memory.WriteRequest{Action: memory.Add, File: "tools", Text: "Builds run on mise"})
	hits, _, err := s.Search(ctx, "buildkit RELEASE", 10)
	if err != nil || len(hits) != 1 || hits[0].Path != "tools.md" || hits[0].Line != 3 {
		t.Fatalf("Search = %v, %v", hits, err)
	}
}
