package cli

import (
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jedwards1230/scrim/internal/config"
)

// commonFlags are the --port/--host/--idle-timeout/--no-auth/--dir flags
// shared by every verb, defaulted from SCRIM_* environment variables (and,
// under those, scrim's built-in defaults).
type commonFlags struct {
	dir         string
	host        string
	port        int
	idleTimeout time.Duration
	noAuth      bool
}

// registerCommonFlags adds the shared flags to fs and returns a handle to
// read their parsed values from.
func registerCommonFlags(fs *flag.FlagSet) *commonFlags {
	envDefaults := config.FromEnv()
	cf := &commonFlags{}
	fs.StringVar(&cf.dir, "dir", envDefaults.Dir, "directory for canvases + daemon state (env SCRIM_DIR)")
	fs.StringVar(&cf.host, "host", envDefaults.Host, "host the daemon binds to (env SCRIM_HOST)")
	fs.IntVar(&cf.port, "port", envDefaults.Port, "port the daemon listens on (env SCRIM_PORT)")
	fs.DurationVar(&cf.idleTimeout, "idle-timeout", envDefaults.IdleTimeout, "idle time before the daemon exits (env SCRIM_IDLE_TIMEOUT); 0 or negative disables idle exit entirely")
	fs.BoolVar(&cf.noAuth, "no-auth", envDefaults.NoAuth, "disable the local auth token (env SCRIM_NO_AUTH)")
	return cf
}

// toConfig resolves the parsed flags into a config.Config, expanding a
// leading "~" in --dir.
func (cf *commonFlags) toConfig() config.Config {
	return config.Config{
		Dir:         config.ExpandHome(cf.dir),
		Host:        cf.host,
		Port:        cf.port,
		IdleTimeout: cf.idleTimeout,
		NoAuth:      cf.noAuth,
	}
}

// newFlagSet returns a FlagSet whose usage/error output goes to stderr and
// whose parse errors are returned rather than causing an os.Exit.
func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

// parseArgs parses args against fs, tolerating flags and positional
// arguments in any order (e.g. "add report --title T" as well as
// "add --title T report"). The standard library's flag package stops
// scanning for flags at the first non-flag token, which would otherwise
// force every verb's flags to precede its positional arguments; scrim's
// documented usage (e.g. "add <id> [--title T]") puts the id first.
func parseArgs(fs *flag.FlagSet, args []string) error {
	return fs.Parse(reorderFlagsFirst(fs, args))
}

// reorderFlagsFirst returns args with every flag token (and, for
// non-boolean flags, its value) moved before any positional argument,
// preserving relative order within each group.
func reorderFlagsFirst(fs *flag.FlagSet, args []string) []string {
	var flagArgs, positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if len(a) < 2 || a[0] != '-' {
			positional = append(positional, a)
			continue
		}
		flagArgs = append(flagArgs, a)

		name := strings.TrimLeft(a, "-")
		if strings.ContainsRune(name, '=') {
			continue // value embedded in this token, e.g. --title=Foo
		}
		if isBoolFlag(fs, name) {
			continue // no value token to consume, e.g. --no-auth
		}
		if i+1 < len(args) {
			i++
			flagArgs = append(flagArgs, args[i])
		}
	}
	return append(flagArgs, positional...)
}

func isBoolFlag(fs *flag.FlagSet, name string) bool {
	f := fs.Lookup(name)
	if f == nil {
		return false
	}
	bf, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && bf.IsBoolFlag()
}

func usageError(stderr io.Writer, format string, args ...any) int {
	outf(stderr, format+"\n", args...)
	return 2
}

// outln/outf/errOut write to a CLI output stream, deliberately ignoring the
// write error: there is nothing actionable a CLI verb can do if writing to
// its own stdout/stderr fails.
func outln(w io.Writer, args ...any) {
	_, _ = fmt.Fprintln(w, args...)
}

func outf(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}

func errOut(w io.Writer, err error) {
	_, _ = fmt.Fprintln(w, "error:", err)
}
