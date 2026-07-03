package openurl

import (
	"reflect"
	"testing"
)

func TestCommandFor(t *testing.T) {
	tests := []struct {
		name     string
		goos     string
		rawURL   string
		wantName string
		wantArgs []string
		wantErr  bool
	}{
		{
			name:     "darwin uses open",
			goos:     "darwin",
			rawURL:   "http://127.0.0.1:7777/?t=abc",
			wantName: "open",
			wantArgs: []string{"http://127.0.0.1:7777/?t=abc"},
		},
		{
			name:     "linux uses xdg-open",
			goos:     "linux",
			rawURL:   "http://127.0.0.1:7777/",
			wantName: "xdg-open",
			wantArgs: []string{"http://127.0.0.1:7777/"},
		},
		{
			name:     "windows uses cmd start with an empty title arg",
			goos:     "windows",
			rawURL:   "http://127.0.0.1:7777/?t=a&b=c",
			wantName: "cmd",
			wantArgs: []string{"/c", "start", "", "http://127.0.0.1:7777/?t=a&b=c"},
		},
		{
			name:    "unsupported platform errors instead of guessing",
			goos:    "plan9",
			rawURL:  "http://127.0.0.1:7777/",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			name, args, err := commandFor(tt.goos, tt.rawURL)
			if (err != nil) != tt.wantErr {
				t.Fatalf("commandFor() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if name != tt.wantName {
				t.Errorf("commandFor() name = %q, want %q", name, tt.wantName)
			}
			if !reflect.DeepEqual(args, tt.wantArgs) {
				t.Errorf("commandFor() args = %v, want %v", args, tt.wantArgs)
			}
		})
	}
}
