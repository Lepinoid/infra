package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/lepinoid/infra/updater/internal/fsutil"
	"github.com/lepinoid/infra/updater/internal/journal"
	"github.com/lepinoid/infra/updater/internal/ready"
	recovery "github.com/lepinoid/infra/updater/internal/recover"
)

var errUsage = errors.New("usage: updater run|init-recover|ready-check|write-json-atomic PATH|fsync-dir PATH|sha256 PATH|verify-jar PATH SHA256|move-atomic SRC DST")

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	if err := dispatch(ctx, os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func dispatch(ctx context.Context, args []string, input io.Reader, output io.Writer) error {
	if len(args) == 0 {
		return errUsage
	}
	const root = "/data/plugins/.lepinoid"
	switch args[0] {
	case "write-json-atomic":
		if len(args) != 2 {
			return errUsage
		}
		data, err := io.ReadAll(io.LimitReader(input, 4<<20))
		if err != nil {
			return err
		}
		return fsutil.WriteJSON(args[1], data)
	case "fsync-dir":
		if len(args) != 2 {
			return errUsage
		}
		return fsutil.SyncDir(args[1])
	case "sha256":
		if len(args) != 2 {
			return errUsage
		}
		hash, err := fsutil.SHA256(args[1])
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(output, hash)
		return err
	case "verify-jar":
		if len(args) != 3 {
			return errUsage
		}
		return fsutil.VerifyJar(args[1], args[2])
	case "move-atomic":
		if len(args) != 3 {
			return errUsage
		}
		return fsutil.Move(args[1], args[2])
	case "init-recover":
		if len(args) != 1 || os.Getenv("POD_UID") == "" {
			return errUsage
		}
		r := recovery.Runner{Store: journal.Store{Root: root}, Data: "/data", PodUID: os.Getenv("POD_UID"), Now: time.Now}
		return r.Run()
	case "ready-check":
		if len(args) != 1 || os.Getenv("POD_UID") == "" {
			return errUsage
		}
		return ready.Check(ready.Config{Root: root, PodUID: os.Getenv("POD_UID"), Commands: []string{"oras", "kubectl", "mc-monitor", "updater"}, Now: time.Now})
	case "run":
		if len(args) != 1 {
			return errUsage
		}
		return run(ctx)
	default:
		return errUsage
	}
}
