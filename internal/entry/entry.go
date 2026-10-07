// Package entry reads and writes memory entries: one fact per Markdown bullet,
// with optional metadata at the end of the line, as in the Agent Memory Repo spec.
//
//   - Prefers short bullet summaries [source: claude-code:3f1c; added: 2026-10-07; by: claude-code]
package entry

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Entry is one remembered fact.
type Entry struct {
	Text string
	Meta []Field // in written order; keys are unique
}

// Field is one metadata key and value.
type Field struct {
	Key, Value string
}

var (
	keyPattern  = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)
	metaPattern = regexp.MustCompile(`\s\[([a-z][a-z0-9_-]*:[^\[\]]*)\]\s*$`)
)

// IsBullet reports whether a line is written as an entry.
func IsBullet(line string) bool { return strings.HasPrefix(line, "- ") }

// Parse reads one entry line. Lines that are not bullets are an error.
func Parse(line string) (Entry, error) {
	if !IsBullet(line) {
		return Entry{}, fmt.Errorf("an entry starts with \"- \": %q", line)
	}
	body := strings.TrimSpace(strings.TrimPrefix(line, "- "))
	loc := metaPattern.FindStringSubmatchIndex(body)
	if loc == nil {
		return Entry{Text: body}, nil
	}
	meta, err := parseMeta(body[loc[2]:loc[3]])
	if err != nil {
		return Entry{}, err
	}
	return Entry{Text: strings.TrimSpace(body[:loc[0]]), Meta: meta}, nil
}

func parseMeta(s string) ([]Field, error) {
	var out []Field
	seen := map[string]bool{}
	for part := range strings.SplitSeq(s, ";") {
		key, value, ok := strings.Cut(strings.TrimSpace(part), ":")
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if !ok || !keyPattern.MatchString(key) || value == "" {
			return nil, fmt.Errorf("metadata %q is not written as [key: value; key: value]", s)
		}
		if seen[key] {
			return nil, fmt.Errorf("metadata key %q appears twice", key)
		}
		seen[key] = true
		out = append(out, Field{key, value})
	}
	return out, nil
}

// Get returns the value for key, or "".
func (e Entry) Get(key string) string {
	for _, f := range e.Meta {
		if f.Key == key {
			return f.Value
		}
	}
	return ""
}

// Set adds key or replaces its value, keeping the original position.
func (e *Entry) Set(key, value string) {
	for i, f := range e.Meta {
		if f.Key == key {
			e.Meta[i].Value = value
			return
		}
	}
	e.Meta = append(e.Meta, Field{key, value})
}

// String writes the entry as one line, without a trailing newline.
func (e Entry) String() string {
	var b strings.Builder
	b.WriteString("- ")
	b.WriteString(e.Text)
	if len(e.Meta) > 0 {
		b.WriteString(" [")
		for i, f := range e.Meta {
			if i > 0 {
				b.WriteString("; ")
			}
			b.WriteString(f.Key + ": " + f.Value)
		}
		b.WriteString("]")
	}
	return b.String()
}

// ErrInvalid is wrapped by Validate errors.
var ErrInvalid = errors.New("invalid entry")

// Validate checks that text and metadata can be written on one line and read back
// unchanged.
func (e Entry) Validate() error {
	switch {
	case strings.TrimSpace(e.Text) == "":
		return fmt.Errorf("%w: the text is empty", ErrInvalid)
	case strings.ContainsAny(e.Text, "\r\n"):
		return fmt.Errorf("%w: an entry is one line; split separate facts into separate entries", ErrInvalid)
	}
	for _, f := range e.Meta {
		switch {
		case !keyPattern.MatchString(f.Key):
			return fmt.Errorf("%w: metadata key %q must be lowercase letters, digits, - or _", ErrInvalid, f.Key)
		case f.Value == "" || strings.ContainsAny(f.Value, "[];\r\n"):
			return fmt.Errorf("%w: metadata value %q is empty or contains [ ] ; or a line break", ErrInvalid, f.Value)
		}
	}
	back, err := Parse(e.String())
	if err != nil || back.Text != e.Text || len(back.Meta) != len(e.Meta) {
		return fmt.Errorf("%w: the text ends with something that reads as metadata; reword the end of the entry", ErrInvalid)
	}
	return nil
}
