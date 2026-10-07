package memory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/swiftugandan/tallyfold/internal/entry"
	"github.com/swiftugandan/tallyfold/internal/gitx"
	"github.com/swiftugandan/tallyfold/internal/mount"
	"github.com/swiftugandan/tallyfold/internal/publish"
)

// Action is what a write does to one entry.
type Action string

const (
	Add     Action = "add"
	Replace Action = "replace"
	Remove  Action = "remove"
)

// WriteRequest changes one entry.
type WriteRequest struct {
	// Scope names the memory to write: "personal" in this version. Team and swarm
	// scopes arrive with their publish policies.
	Scope  string
	Action Action
	// File is the repo-relative Markdown file; it defaults to MEMORY.md. A new
	// topic file is created and linked from the MEMORY.md index.
	File string
	// Text is the fact, for add and replace.
	Text string
	// Match selects the existing entry for replace and remove: part of its text.
	Match string
	// Source optionally links to where the fact was learned.
	Source string
	// Title names a new topic file; it defaults to one made from the file name.
	Title string
	// By names who wrote the entry; it defaults to the Store's Writer.
	By string
}

// WriteResult reports a completed write.
type WriteResult struct {
	File    string
	Entry   string // the line written, or the line removed
	Outcome publish.Outcome
	Notes   []string
}

func (r WriteResult) String() string {
	s := fmt.Sprintf("%s in %s: %s", r.Outcome.Detail, r.File, r.Entry)
	for _, n := range r.Notes {
		s += "\nNote: " + n
	}
	return s
}

// ErrNoChange means the write would not change memory.
var ErrNoChange = errors.New("no change")

// Write applies one change to memory, commits it and publishes it.
func (s *Store) Write(ctx context.Context, req WriteRequest) (WriteResult, error) {
	m, err := s.mountFor(req.Scope)
	if err != nil {
		return WriteResult{}, err
	}
	policy, err := publish.For(m.Role)
	if err != nil {
		return WriteResult{}, err
	}
	unlock, err := m.Lock(ctx)
	if err != nil {
		return WriteResult{}, err
	}
	defer unlock()

	// Start from the newest copy so the change lands on what other sessions wrote.
	if _, err := policy.Sync(ctx, m, gitx.PreferNone); err != nil {
		return WriteResult{}, fmt.Errorf("could not bring memory up to date before writing: %w", err)
	}

	change, err := s.plan(m.Dir, req)
	if errors.Is(err, ErrNoChange) {
		return WriteResult{File: change.file, Entry: change.line, Outcome: publish.Outcome{State: publish.Pushed, Detail: "already remembered"}}, nil
	}
	if err != nil {
		return WriteResult{}, err
	}
	for path, content := range change.files {
		full := filepath.Join(m.Dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return WriteResult{}, err
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			return WriteResult{}, err
		}
	}
	paths := make([]string, 0, len(change.files))
	for p := range change.files {
		paths = append(paths, p)
	}
	if err := m.Repo.Commit(ctx, change.message, paths...); err != nil {
		return WriteResult{}, err
	}
	made, err := m.Repo.Head(ctx)
	if err != nil {
		return WriteResult{}, err
	}
	out, err := policy.Publish(ctx, m, made)
	if err != nil {
		return WriteResult{}, err
	}
	return WriteResult{File: change.file, Entry: change.line, Outcome: out, Notes: change.notes}, nil
}

func (s *Store) mountFor(scope string) (*mount.Mount, error) {
	switch mount.Role(scope) {
	case "", mount.Personal:
		return s.layout.Personal()
	case mount.Team, mount.Swarm, mount.Guest:
		return nil, fmt.Errorf("%s memory is %w", scope, publish.ErrUnsupported)
	}
	return nil, fmt.Errorf("unknown scope %q; use personal", scope)
}

// change is a planned write: new file contents keyed by repo-relative path.
type change struct {
	file    string
	line    string
	files   map[string]string
	message string
	notes   []string
}

// plan computes a write without touching the disk.
func (s *Store) plan(root string, req WriteRequest) (change, error) {
	file, err := normaliseFile(req.File)
	if err != nil {
		return change{}, err
	}
	full, err := safeJoin(root, file)
	if err != nil {
		return change{}, err
	}
	var existing string
	data, err := os.ReadFile(full)
	switch {
	case err == nil:
		existing = string(data)
	case !errors.Is(err, os.ErrNotExist):
		return change{}, err
	case req.Action != Add:
		return change{}, fmt.Errorf("%s does not exist", file)
	}
	c := change{file: file, files: map[string]string{}}

	switch req.Action {
	case Add:
		e, err := s.newEntry(req, nil)
		if err != nil {
			return change{}, err
		}
		c.line = e.String()
		if _, dup := findEntry(existing, file, e.Text, true); dup {
			return c, ErrNoChange
		}
		if existing == "" {
			existing = "# " + titleFor(file, req.Title) + "\n\n"
			index, err := os.ReadFile(filepath.Join(root, MemoryFile))
			if err != nil {
				return change{}, err
			}
			c.files[MemoryFile] = addToIndex(string(index), strings.TrimSuffix(file, ".md"))
			c.notes = append(c.notes, "created "+file+" and linked it from the MEMORY.md index")
		}
		c.files[file] = insertEntry(existing, file, c.line)
		c.message = "Remember: " + summary(e.Text)
	case Replace, Remove:
		if strings.TrimSpace(req.Match) == "" {
			return change{}, fmt.Errorf("%s needs match: part of the text of the entry to change", req.Action)
		}
		idx, err := matchOne(existing, file, req.Match)
		if err != nil {
			return change{}, err
		}
		lines := strings.Split(existing, "\n")
		old, _ := entry.Parse(lines[idx])
		if req.Action == Remove {
			c.line = lines[idx]
			lines = append(lines[:idx], lines[idx+1:]...)
			c.message = "Forget: " + summary(old.Text)
		} else {
			e, err := s.newEntry(req, &old)
			if err != nil {
				return change{}, err
			}
			c.line = e.String()
			if c.line == lines[idx] {
				return c, ErrNoChange
			}
			lines[idx] = c.line
			c.message = "Update: " + summary(e.Text)
		}
		c.files[file] = strings.Join(lines, "\n")
	default:
		return change{}, fmt.Errorf("unknown action %q; use add, replace or remove", req.Action)
	}

	if body, ok := c.files[MemoryFile]; ok && len(body) > MemoryFileLimit {
		return change{}, fmt.Errorf("MEMORY.md would be %d characters, over the %d limit that keeps startup context small; put this in a topic file such as people.md, or remove an entry that not every session needs", len(body), MemoryFileLimit)
	}
	for _, target := range links(c.line) {
		if strings.Contains(target, ":") {
			continue
		}
		if _, ok := resolveLink(root, target); !ok && strings.TrimSuffix(file, ".md") != target {
			c.notes = append(c.notes, fmt.Sprintf("[[%s]] does not point to a file yet", target))
		}
	}
	return c, nil
}

// newEntry builds the entry to write, stamping metadata.
func (s *Store) newEntry(req WriteRequest, old *entry.Entry) (entry.Entry, error) {
	text := strings.TrimSpace(req.Text)
	if strings.HasPrefix(text, "- ") {
		text = strings.TrimSpace(text[2:])
	}
	if kind := findSecret(text); kind != "" {
		return entry.Entry{}, fmt.Errorf("the entry looks like it contains a %s; memory never stores credentials", kind)
	}
	e := entry.Entry{Text: text}
	if old != nil {
		e.Meta = append(e.Meta, old.Meta...)
	}
	if req.Source != "" {
		e.Set("source", req.Source)
	}
	e.Set("added", s.Now().Format("2006-01-02"))
	by := req.By
	if by == "" {
		by = s.Writer
	}
	e.Set("by", by)
	if err := e.Validate(); err != nil {
		return entry.Entry{}, err
	}
	return e, nil
}

func normaliseFile(file string) (string, error) {
	file = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(file), "[["), "]]"))
	if file == "" {
		return MemoryFile, nil
	}
	file = filepath.ToSlash(filepath.Clean(file))
	switch filepath.Ext(file) {
	case "":
		file += ".md"
	case ".md":
	default:
		return "", fmt.Errorf("entries go in Markdown files; %s is not one", file)
	}
	if strings.HasPrefix(file, ".tallyfold/") {
		return "", errors.New(".tallyfold/ holds configuration, not entries")
	}
	return file, nil
}

// entryLines yields the indexes of entry lines, skipping the MEMORY.md index.
func entryLines(content, file string) []int {
	var out []int
	inIndex := false
	for i, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, "#") {
			inIndex = file == MemoryFile && strings.TrimSpace(line) == IndexHeading
			continue
		}
		if !inIndex && entry.IsBullet(line) {
			out = append(out, i)
		}
	}
	return out
}

// findEntry looks for an entry whose text equals text (exact) or contains it.
func findEntry(content, file, text string, exact bool) (int, bool) {
	lines := strings.Split(content, "\n")
	want := strings.ToLower(strings.TrimSpace(text))
	for _, i := range entryLines(content, file) {
		e, err := entry.Parse(lines[i])
		if err != nil {
			continue
		}
		got := strings.ToLower(e.Text)
		if (exact && got == want) || (!exact && strings.Contains(got, want)) {
			return i, true
		}
	}
	return 0, false
}

func matchOne(content, file, match string) (int, error) {
	lines := strings.Split(content, "\n")
	want := strings.ToLower(strings.TrimSpace(match))
	var found []int
	for _, i := range entryLines(content, file) {
		if strings.Contains(strings.ToLower(lines[i]), want) {
			found = append(found, i)
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return 0, fmt.Errorf("no entry in %s contains %q", file, match)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d entries in %s contain %q; use more of the text:", len(found), file, match)
	for _, i := range found {
		b.WriteString("\n" + lines[i])
	}
	return 0, errors.New(b.String())
}

// insertEntry adds line to content: at the end of a topic file, or at the end of
// the entries above the index in MEMORY.md.
func insertEntry(content, file, line string) string {
	lines := strings.Split(strings.TrimRight(content, "\n"), "\n")
	index := len(lines)
	if file == MemoryFile {
		for i, l := range lines {
			if strings.TrimSpace(l) == IndexHeading {
				index = i
				break
			}
		}
	}
	at := index
	for at > 0 && strings.TrimSpace(lines[at-1]) == "" {
		at--
	}
	out := append([]string{}, lines[:at]...)
	if at > 0 && strings.HasPrefix(lines[at-1], "#") {
		out = append(out, "") // a blank line between a heading and the first entry
	}
	out = append(out, line)
	if index < len(lines) {
		out = append(out, "")
		out = append(out, lines[index:]...)
	}
	return strings.Join(out, "\n") + "\n"
}

// addToIndex links target from the MEMORY.md index, creating the index if needed.
func addToIndex(memory, target string) string {
	link := "- [[" + target + "]]"
	lines := strings.Split(strings.TrimRight(memory, "\n"), "\n")
	start := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == IndexHeading {
			start = i
		}
	}
	if start < 0 {
		return strings.TrimRight(memory, "\n") + "\n\n" + IndexHeading + "\n\n" + link + "\n"
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "#") {
			end = i
			break
		}
	}
	at := end
	for at > start+1 && strings.TrimSpace(lines[at-1]) == "" {
		at--
	}
	out := append([]string{}, lines[:at]...)
	if at == start+1 {
		out = append(out, "")
	}
	out = append(out, link)
	out = append(out, lines[at:]...)
	return collapseBlanks(strings.Join(out, "\n")) + "\n"
}

func collapseBlanks(s string) string {
	for strings.Contains(s, "\n\n\n") {
		s = strings.ReplaceAll(s, "\n\n\n", "\n\n")
	}
	return strings.TrimRight(s, "\n")
}

func titleFor(file, title string) string {
	if title = strings.TrimSpace(title); title != "" {
		return title
	}
	base := strings.TrimSuffix(filepath.Base(file), ".md")
	base = strings.NewReplacer("-", " ", "_", " ").Replace(base)
	if base == "" {
		return "Notes"
	}
	return strings.ToUpper(base[:1]) + base[1:]
}

func summary(text string) string {
	if len(text) <= 60 {
		return text
	}
	cut := strings.LastIndexByte(text[:60], ' ')
	if cut < 30 {
		cut = 60
	}
	return text[:cut] + "…"
}
