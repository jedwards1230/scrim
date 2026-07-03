package cli

import (
	"flag"
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
