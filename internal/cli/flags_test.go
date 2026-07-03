package cli

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func newFlagSetForTest() *flag.FlagSet {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.String("title", "", "")
	fs.Bool("no-auth", false, "")
	fs.String("dir", "", "")
	return fs
}

func TestReorderFlagsFirst(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{
			name: "positional then flag",
			args: []string{"report", "--title", "My Report"},
			want: []string{"--title", "My Report", "report"},
		},
		{
			name: "flag then positional",
			args: []string{"--title", "My Report", "report"},
			want: []string{"--title", "My Report", "report"},
		},
		{
			name: "no flags",
			args: []string{"report"},
			want: []string{"report"},
		},
		{
			name: "equals form does not consume next token",
			args: []string{"report", "--title=My Report"},
			want: []string{"--title=My Report", "report"},
		},
		{
			name: "bool flag does not consume next token",
			args: []string{"--no-auth", "report"},
			want: []string{"--no-auth", "report"},
		},
		{
			name: "bool flag after positional",
			args: []string{"report", "--no-auth"},
			want: []string{"--no-auth", "report"},
		},
		{
			name: "double dash stops flag parsing",
			args: []string{"--title", "T", "--", "--weird-id"},
			want: []string{"--title", "T", "--weird-id"},
		},
		{
			name: "multiple positionals preserved in order",
			args: []string{"a", "--title", "T", "b"},
			want: []string{"--title", "T", "a", "b"},
		},
		{
			name: "empty args",
			args: []string{},
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := newFlagSetForTest()
			got := reorderFlagsFirst(fs, tt.args)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("reorderFlagsFirst(%v) = %v, want %v", tt.args, got, tt.want)
			}
		})
	}
}

func TestParseArgsPositionalBeforeFlags(t *testing.T) {
	fs := newFlagSet("add", &discard{})
	title := fs.String("title", "", "")
	if err := parseArgs(fs, []string{"report", "--title", "My Report"}); err != nil {
		t.Fatalf("parseArgs() error = %v", err)
	}
	if *title != "My Report" {
		t.Errorf("title = %q, want %q", *title, "My Report")
	}
	if got := fs.Args(); len(got) != 1 || got[0] != "report" {
		t.Errorf("positional args = %v, want [report]", got)
	}
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

func TestExitForParseErr(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"help request exits 0", flag.ErrHelp, 0},
		{"wrapped help request still exits 0", fmt.Errorf("parsing: %w", flag.ErrHelp), 0},
		{"any other error exits 2", errors.New("flag provided but not defined: -bogus"), 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := exitForParseErr(tt.err); got != tt.want {
				t.Errorf("exitForParseErr(%v) = %d, want %d", tt.err, got, tt.want)
			}
		})
	}
}

// TestCommonFlagsToConfigResolvesDir is a regression test for a relative
// --dir: a self-started detached daemon must resolve --dir the same way the
// CLI process that spawns it does, which only holds if toConfig() makes it
// absolute up front rather than leaving it relative (and therefore
// dependent on whatever cwd happens to be in effect wherever it's read).
func TestCommonFlagsToConfigResolvesDir(t *testing.T) {
	origWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd() error = %v", err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatalf("os.Chdir() error = %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(origWd); err != nil {
			t.Fatalf("os.Chdir() restore error = %v", err)
		}
	})
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd() error = %v", err)
	}

	cf := &commonFlags{dir: "relative-dir", host: "127.0.0.1", port: 7777}
	cfg := cf.toConfig()

	want := filepath.Join(cwd, "relative-dir")
	if cfg.Dir != want {
		t.Errorf("toConfig().Dir = %q, want %q", cfg.Dir, want)
	}
	if !filepath.IsAbs(cfg.Dir) {
		t.Errorf("toConfig().Dir = %q, want an absolute path", cfg.Dir)
	}
}

// TestRegisterCommonFlagsNoMDNS confirms --no-mdns parses through
// registerCommonFlags and survives into the resolved config.Config,
// following the exact same pattern as the existing --no-auth flag.
func TestRegisterCommonFlagsNoMDNS(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{name: "unset defaults to false", args: nil, want: false},
		{name: "--no-mdns sets true", args: []string{"--no-mdns"}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := newFlagSet("test", &discard{})
			cf := registerCommonFlags(fs)
			if err := parseArgs(fs, tt.args); err != nil {
				t.Fatalf("parseArgs() error = %v", err)
			}
			if cf.noMDNS != tt.want {
				t.Errorf("noMDNS = %v, want %v", cf.noMDNS, tt.want)
			}
			if got := cf.toConfig().NoMDNS; got != tt.want {
				t.Errorf("toConfig().NoMDNS = %v, want %v", got, tt.want)
			}
		})
	}
}
