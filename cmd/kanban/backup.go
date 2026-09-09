package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"kanban/internal/backup"
	"kanban/internal/config"
	"kanban/internal/store"
)

// stdin is where `kanban import` reads a snapshot from when no file is named.
// A variable, so a test can pipe one in without writing it to disk first.
var stdin io.Reader = os.Stdin

// exportCmd writes a snapshot of the whole store.
//
//	kanban export             to stdout, so it can be piped or redirected
//	kanban export -o file     to a file, 0o600
func exportCmd(ctx context.Context, st store.Store, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	fs.SetOutput(stderr)
	out := fs.String("o", "", "write to this file instead of stdout")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "kanban export: unexpected argument %q\n", fs.Arg(0))
		return 2
	}

	snap, err := backup.Export(ctx, st)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "kanban export: %v\n", err)
		return 1
	}
	snap.Build = version
	if *out == "" {
		if err := backup.Write(stdout, snap); err != nil {
			_, _ = fmt.Fprintf(stderr, "kanban export: %v\n", err)
			return 1
		}
		return 0
	}
	body, err := backup.Bytes(snap)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "kanban export: %v\n", err)
		return 1
	}
	// 0o600: the file is every card on every board, including whatever people
	// wrote in the comments.
	if err := os.WriteFile(*out, body, 0o600); err != nil {
		_, _ = fmt.Fprintf(stderr, "kanban export: %v\n", err)
		return 1
	}
	rep, _ := backup.Check(snap)
	_, _ = fmt.Fprintf(stdout, "wrote %s: %s\n", *out, rep)
	return 0
}

// importCmd reads a snapshot back in.
//
//	kanban import file          refuses a board that is already there
//	kanban import -replace f    overwrites those boards instead
//	kanban import -dry-run f    checks the file and writes nothing
//	kanban import               reads stdin
func importCmd(ctx context.Context, st store.Store, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	fs.SetOutput(stderr)
	replace := fs.Bool("replace", false, "overwrite a board that is already in the store")
	dry := fs.Bool("dry-run", false, "check the snapshot and write nothing")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		_, _ = fmt.Fprintf(stderr, "kanban import: one file at a time, not %d\n", fs.NArg())
		return 2
	}

	r, name := stdin, "stdin"
	if path := fs.Arg(0); path != "" && path != "-" {
		f, err := os.Open(path)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "kanban import: %v\n", err)
			return 1
		}
		defer func() { _ = f.Close() }()
		r, name = f, path
	}
	snap, err := backup.Read(r)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "kanban import: %s: %v\n", name, err)
		return 1
	}

	if *dry {
		rep, err := backup.Check(snap)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "kanban import: %s: %v\n", name, err)
			return 1
		}
		_, _ = fmt.Fprintf(stdout, "%s is a format %d snapshot taken %s: %s\n",
			name, snap.Format, snap.TakenAt.Format(time.RFC3339), rep)
		return 0
	}

	rep, err := backup.Import(ctx, st, snap, backup.Options{Replace: *replace})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "kanban import: %s: %v\n", name, err)
		if errors.Is(err, backup.ErrExists) {
			return 2
		}
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "imported %s\n", rep)
	return 0
}

// backupTarget builds the target the schedule writes to, or nil when none is
// configured. Only this package knows which target is in use, the same way it
// is the only package that knows which store backend is.
func backupTarget(cfg config.Config) (backup.Target, error) {
	switch {
	case cfg.BackupDir != "":
		return backup.Dir{Path: cfg.BackupDir}, nil
	case cfg.BackupS3Bucket != "":
		return &backup.S3{
			Bucket:          cfg.BackupS3Bucket,
			Prefix:          cfg.BackupS3Prefix,
			Region:          cfg.BackupS3Region,
			Endpoint:        cfg.BackupS3Endpoint,
			AccessKeyID:     cfg.AWSAccessKeyID,
			SecretAccessKey: cfg.AWSSecretAccessKey,
			SessionToken:    cfg.AWSSessionToken,
		}, nil
	}
	return nil, nil
}

// startBackups runs the snapshot schedule alongside the server and returns a
// function that stops it and waits for a snapshot in flight to finish. Calling
// it is what keeps the schedule from writing through a store the caller is
// about to close.
//
// The wait is bounded. A backup is worth a few seconds of a shutdown, and it is
// not worth a pod that will not terminate.
func startBackups(ctx context.Context, cfg config.Config, st store.Store, log *slog.Logger) (func(), error) {
	if !cfg.BackupScheduled() {
		if cfg.BackupDir != "" || cfg.BackupS3Bucket != "" {
			log.Info("backup schedule off", "reason", "BACKUP_INTERVAL is 0")
		}
		return func() {}, nil
	}
	target, err := backupTarget(cfg)
	if err != nil {
		return nil, err
	}
	sched := &backup.Schedule{
		Store:  st,
		Target: target,
		Every:  cfg.BackupInterval,
		Keep:   cfg.BackupKeep,
		Build:  version,
		Log:    log,
	}
	// Its own context, so the wait below ends the schedule however serve
	// returned: a signal cancels the parent, a failed listen does not.
	runCtx, stopSchedule := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := sched.Run(runCtx); err != nil {
			log.Error("backup schedule", "err", err)
		}
	}()
	return func() {
		stopSchedule()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			log.Warn("backup: gave up waiting for the snapshot in flight")
		}
	}, nil
}
