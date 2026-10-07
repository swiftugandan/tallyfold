package memory

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/swiftugandan/tallyfold/internal/entry"
)

// MemoryFile is the entry point every session loads.
const MemoryFile = "MEMORY.md"

// IndexHeading starts the list of topic files in MEMORY.md.
const IndexHeading = "## Index"

// MemoryFileLimit keeps each repo's MEMORY.md small enough that every mount's
// MEMORY.md together fits in startup context.
const MemoryFileLimit = 4000

// secretPatterns match credentials that must never be written to memory. The list
// covers common token formats; it is a guard against accidents, not a scanner of
// record.
var secretPatterns = []struct {
	name string
	re   *regexp.Regexp
}{
	{"private key", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
	{"AWS access key", regexp.MustCompile(`\b(AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{"GitHub token", regexp.MustCompile(`\b(gh[pousr]_[A-Za-z0-9]{36,}|github_pat_[A-Za-z0-9_]{40,})\b`)},
	{"GitLab token", regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{20,}\b`)},
	{"Slack token", regexp.MustCompile(`\bxox[abposr]-[A-Za-z0-9-]{10,}\b`)},
	{"Anthropic key", regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{20,}\b`)},
	{"OpenAI key", regexp.MustCompile(`\bsk-(proj-)?[A-Za-z0-9_-]{32,}\b`)},
	{"Google API key", regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`)},
	{"Stripe key", regexp.MustCompile(`\b[rs]k_live_[0-9A-Za-z]{20,}\b`)},
	{"JSON web token", regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`)},
	{"password assignment", regexp.MustCompile(`(?i)\b(password|passwd|pwd|secret|api[_-]?key|token)\s*[:=]\s*\S{6,}`)},
}

// findSecret returns the kind of the first credential in s, or "".
func findSecret(s string) string {
	for _, p := range secretPatterns {
		if p.re.MatchString(s) {
			return p.name
		}
	}
	return ""
}

var linkPattern = regexp.MustCompile(`\[\[([^\[\]]+)\]\]`)

// links returns the [[targets]] in s.
func links(s string) []string {
	var out []string
	for _, m := range linkPattern.FindAllStringSubmatch(s, -1) {
		out = append(out, strings.TrimSpace(m[1]))
	}
	return out
}

// resolveLink maps a link target to a file in the mount. Targets without an
// extension refer to Markdown files, as in the Agent Memory Repo spec.
func resolveLink(root, target string) (string, bool) {
	rel := target
	if filepath.Ext(rel) == "" {
		rel += ".md"
	}
	path, err := safeJoin(root, rel)
	if err != nil {
		return "", false
	}
	fi, err := os.Stat(path)
	return rel, err == nil && !fi.IsDir()
}

// safeJoin joins a repo-relative path to root and refuses paths that leave the repo
// or reach into .git.
func safeJoin(root, rel string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(strings.TrimPrefix(rel, "/")))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || filepath.IsAbs(clean) {
		return "", fmt.Errorf("%q is outside the memory repo", rel)
	}
	first := strings.SplitN(filepath.ToSlash(clean), "/", 2)[0]
	if first == ".git" {
		return "", fmt.Errorf("%q is inside .git", rel)
	}
	return filepath.Join(root, clean), nil
}

// Problem is one lint finding.
type Problem struct {
	Mount, Path string
	Line        int
	Message     string
}

func (p Problem) String() string {
	if p.Line > 0 {
		return fmt.Sprintf("%s:%s:%d: %s", p.Mount, p.Path, p.Line, p.Message)
	}
	return fmt.Sprintf("%s:%s: %s", p.Mount, p.Path, p.Message)
}

// lintTree checks every text file in a mount.
func lintTree(mountName, root string) ([]Problem, error) {
	var problems []Problem
	err := walkText(root, func(rel, content string) {
		if rel == MemoryFile && len(content) > MemoryFileLimit {
			problems = append(problems, Problem{mountName, rel, 0,
				fmt.Sprintf("%d characters, over the %d limit; move entries that not every session needs into topic files", len(content), MemoryFileLimit)})
		}
		for i, line := range strings.Split(content, "\n") {
			if kind := findSecret(line); kind != "" {
				problems = append(problems, Problem{mountName, rel, i + 1, "looks like a " + kind + "; remove it and rotate the credential"})
			}
			if filepath.Ext(rel) != ".md" {
				continue
			}
			if entry.IsBullet(line) {
				if _, err := entry.Parse(line); err != nil {
					problems = append(problems, Problem{mountName, rel, i + 1, err.Error()})
				}
			}
			for _, target := range links(line) {
				if strings.Contains(target, ":") {
					continue // a link into another mount; checked when that mount is loaded
				}
				if _, ok := resolveLink(root, target); !ok {
					problems = append(problems, Problem{mountName, rel, i + 1, fmt.Sprintf("[[%s]] does not resolve to a file", target)})
				}
			}
		}
	})
	return problems, err
}

// maxTextFile bounds the files search and lint read.
const maxTextFile = 1 << 20

// walkText calls fn with every text file under root, skipping .git and binaries.
func walkText(root string, fn func(rel, content string)) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil || !info.Mode().IsRegular() || info.Size() > maxTextFile {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		head := data
		if len(head) > 8000 {
			head = head[:8000]
		}
		if strings.IndexByte(string(head), 0) >= 0 {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		fn(filepath.ToSlash(rel), string(data))
		return nil
	})
}
