package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/jedwards1230/scrim/internal/canvas"
	"github.com/jedwards1230/scrim/internal/config"
	"github.com/jedwards1230/scrim/internal/pushclient"
)

// cmdPush implements `scrim push <id> --to URL --token TOKEN [--watch]`: it
// tars a LOCAL canvas directory and POSTs it to a hub's push endpoint (see
// internal/server's hub mode). It never talks to a local scrim daemon at
// all -- the canvas directory and its metadata are read straight off disk
// via --dir/SCRIM_DIR, exactly like `scrim path`/`scrim snap` do, so a push
// works whether or not anything is currently serving that canvas locally.
//
// It NEVER launches a browser: this is a background/scripted client
// operation, not something a human is expected to watch happen.
func cmdPush(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("push", stderr)
	dirFlag := fs.String("dir", config.FromEnv().Dir, "directory for the LOCAL canvas being pushed (env SCRIM_DIR)")
	to := fs.String("to", "", "REQUIRED: the hub's base URL, e.g. http://127.0.0.1:7788")
	token := fs.String("token", os.Getenv("SCRIM_PUSH_TOKEN"), "REQUIRED: push token (env SCRIM_PUSH_TOKEN)")
	watch := fs.Bool("watch", false, "watch the local canvas directory and re-push on every change, until interrupted")

	if err := parseArgs(fs, args); err != nil {
		return exitForParseErr(err)
	}
	pos := fs.Args()
	if len(pos) != 1 {
		return usageError(stderr, "usage: scrim push <id> --to URL --token TOKEN [--watch]")
	}
	id := pos[0]
	if err := canvas.ValidateID(id); err != nil {
		errOut(stderr, err)
		return 2
	}
	if *to == "" {
		return usageError(stderr, "usage: scrim push <id> --to URL --token TOKEN [--watch] (--to is required)")
	}
	if *token == "" {
		errOut(stderr, errors.New("--token (or SCRIM_PUSH_TOKEN) is required"))
		return 1
	}

	cfg := config.Config{Dir: config.ResolveDir(*dirFlag)}
	canvasDir := canvas.Dir(cfg.CanvasesDir(), id)
	if fi, err := os.Stat(canvasDir); err != nil || !fi.IsDir() {
		errOut(stderr, fmt.Errorf("canvas %q not found at %s", id, canvasDir))
		return 1
	}
	info, err := canvas.Get(cfg.CanvasesDir(), cfg.MetaDir(), id)
	if err != nil {
		errOut(stderr, err)
		return 1
	}

	pushOnce := func() error {
		data, err := pushclient.Pack(canvasDir)
		if err != nil {
			return err
		}
		hubURL, err := pushclient.Push(context.Background(), *to, id, *token, info.Title, info.Description, info.Icon, data)
		if err != nil {
			return err
		}
		outln(stdout, hubURL)
		return nil
	}

	if err := pushOnce(); err != nil {
		errOut(stderr, err)
		return 1
	}
	if !*watch {
		return 0
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := pushclient.Watch(ctx, canvasDir, func() {
		if err := pushOnce(); err != nil {
			errOut(stderr, err)
		}
	}); err != nil {
		errOut(stderr, err)
		return 1
	}
	return 0
}
