// Command tallyfold keeps an AI agent's memory in a git repo the user owns. It runs
// as an MCP server for agent clients, as a session-start hook, and as a CLI.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/swiftugandan/tallyfold/internal/gitx"
	"github.com/swiftugandan/tallyfold/internal/mcpserver"
	"github.com/swiftugandan/tallyfold/internal/memory"
	"github.com/swiftugandan/tallyfold/internal/mount"
)

// version is set at build time with -ldflags "-X main.version=…".
var version = "dev"

const usage = `tallyfold keeps an AI agent's memory in a git repo you own.

Usage:
  tallyfold [--data-dir DIR] <command> [arguments]

Commands:
  init [URL]                 set up memory from an empty or existing private repo; no URL for local-only
  status                     show where memory lives and what is not pushed yet
  context                    print MEMORY.md as an agent sees it at session start
  search WORDS...            find lines containing all the words
  read TARGET                print a memory file, such as projects/payments or [[people]]
  write add|replace|remove   change one entry (see: tallyfold write -h)
  sync [--prefer remote|local]
                             pull other sessions' memory and push waiting changes
  lint                       check entry format, links, secrets and MEMORY.md size
  mcp                        serve the MCP tools on stdin and stdout
  hook session-start         print startup context for a client hook
  version                    print the version

The data directory defaults to $PLUGIN_DATA, then $CLAUDE_PLUGIN_DATA.
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "tallyfold:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout io.Writer) error {
	global := flag.NewFlagSet("tallyfold", flag.ContinueOnError)
	global.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	dataDir := global.String("data-dir", "", "plugin data directory")
	if err := global.Parse(args); err != nil {
		return err
	}
	args = global.Args()
	if len(args) == 0 {
		global.Usage()
		return errors.New("no command")
	}
	cmd, rest := args[0], args[1:]
	if cmd == "version" {
		fmt.Fprintln(stdout, version)
		return nil
	}
	if cmd == "help" || cmd == "-h" {
		global.Usage()
		return nil
	}

	dir, err := mount.DataDir(*dataDir)
	if err != nil {
		if cmd == "hook" {
			return nil // a hook must never break the session
		}
		return err
	}
	store := memory.Open(dir)
	store.Writer = cliWriter()

	switch cmd {
	case "init":
		remote := ""
		if len(rest) > 0 {
			remote = rest[0]
		}
		return printed(stdout)(store.Init(ctx, remote))
	case "status":
		fmt.Fprint(stdout, store.Status(ctx, version))
		return nil
	case "context":
		return printed(stdout)(store.Context(ctx, memory.ContextBudget))
	case "search":
		hits, more, err := store.Search(ctx, strings.Join(rest, " "), 100)
		if err != nil {
			return err
		}
		for _, h := range hits {
			fmt.Fprintln(stdout, h)
		}
		if more > 0 {
			fmt.Fprintf(stdout, "…and %d more\n", more)
		}
		return nil
	case "read":
		if len(rest) != 1 {
			return errors.New("read takes one target, such as projects/payments")
		}
		return printed(stdout)(store.Read(ctx, rest[0]))
	case "write":
		return writeCmd(ctx, store, rest, stdout)
	case "sync":
		fs := flag.NewFlagSet("sync", flag.ContinueOnError)
		prefer := fs.String("prefer", "", "on a conflicting line keep the remote or local version")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		p, err := mcpserver.ParsePrefer(*prefer)
		if err != nil {
			return err
		}
		return printed(stdout)(store.Sync(ctx, p))
	case "lint":
		problems, err := store.Lint(ctx)
		if err != nil {
			return err
		}
		for _, p := range problems {
			fmt.Fprintln(stdout, p)
		}
		if len(problems) > 0 {
			return fmt.Errorf("%d problem(s)", len(problems))
		}
		fmt.Fprintln(stdout, "memory is clean")
		return nil
	case "mcp":
		store.Writer = "agent" // each write names the connected client when it can
		return mcpserver.New(store, version).Run(ctx, &mcp.StdioTransport{})
	case "hook":
		if len(rest) != 1 || rest[0] != "session-start" {
			return errors.New("the only hook is session-start")
		}
		sessionStart(ctx, store, stdout)
		return nil
	}
	global.Usage()
	return fmt.Errorf("unknown command %q", cmd)
}

// sessionStart prints startup context. It always succeeds: a failing hook would
// get in the way of the session, and memory is useful even when stale.
func sessionStart(ctx context.Context, store *memory.Store, stdout io.Writer) {
	if !store.SetUp() {
		fmt.Fprintln(stdout, "Tallyfold is installed but has no memory repo yet. When the user wants memory that lasts across sessions, offer to set it up with memory_init and an empty private repository they own.")
		return
	}
	syncCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	_, syncErr := store.Sync(syncCtx, gitx.PreferNone)
	cancel()
	out, err := store.Context(ctx, memory.ContextBudget)
	if err != nil {
		fmt.Fprintln(stdout, "Tallyfold could not load memory:", err)
		return
	}
	fmt.Fprint(stdout, out)
	if syncErr != nil {
		fmt.Fprintf(stdout, "\nNote: memory could not be synced (%v); this is the local copy.\n", syncErr)
	}
}

func writeCmd(ctx context.Context, store *memory.Store, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("write", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "Usage: tallyfold write add|replace|remove [--file F] [--text T] [--match M] [--source S] [--title T]\n")
		fs.PrintDefaults()
	}
	file := fs.String("file", "", "Markdown file in the memory repo (default MEMORY.md)")
	text := fs.String("text", "", "the fact, for add and replace")
	match := fs.String("match", "", "part of the entry to replace or remove")
	source := fs.String("source", "", "where the fact was learned")
	title := fs.String("title", "", "heading for a new topic file")
	if len(args) == 0 {
		fs.Usage()
		return errors.New("write needs an action")
	}
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	res, err := store.Write(ctx, memory.WriteRequest{
		Action: memory.Action(args[0]), File: *file, Text: *text, Match: *match, Source: *source, Title: *title,
	})
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, res)
	return nil
}

func printed(w io.Writer) func(string, error) error {
	return func(s string, err error) error {
		if err != nil {
			return err
		}
		fmt.Fprintln(w, strings.TrimRight(s, "\n"))
		return nil
	}
}

// cliWriter names the author of entries written from the command line.
func cliWriter() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return "cli"
}
