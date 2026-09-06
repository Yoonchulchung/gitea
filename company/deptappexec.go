// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/urfave/cli/v3"
)

// `gitea deptapp-exec` locks itself down and becomes a department app.
//
// It exists because Landlock and seccomp have to be applied between fork and
// exec, and Go's os/exec has no hook there — the restrictions must be
// entered by the child itself, which then execve()s the real program. Both
// survive execve, so the app starts already inside a box it cannot leave.
//
// Registered as a standalone subcommand: it reads no config file, opens no
// database, and takes everything from its arguments, which also means the
// blast radius of anyone invoking it by hand is their own shell.
//
// Nothing here is a security boundary against the *caller* — anyone who can
// run this could equally run the app directly. It is a boundary against the
// app's own code, which is where the untrusted material is.

func NewDeptAppExecCommand() *cli.Command {
	return &cli.Command{
		Name:   "deptapp-exec",
		Usage:  "(internal) Run a department app inside the platform sandbox",
		Hidden: true, // internal, not something an operator ever types
		Description: "Applies resource limits, Landlock filesystem and network restrictions " +
			"and a seccomp filter to itself, then executes the given command. " +
			"Used by Gitea on hosts where bubblewrap cannot run.",
		Flags: []cli.Flag{
			&cli.StringSliceFlag{Name: "ro", Usage: "path the app may read (repeatable)"},
			&cli.StringSliceFlag{Name: "rw", Usage: "path the app may read and write (repeatable)"},
			&cli.BoolFlag{Name: "allow-network", Usage: "leave outbound TCP and socket creation alone"},
			&cli.IntFlag{Name: "gitea-pid", Usage: "pid to protect from signals"},
			&cli.IntFlag{Name: "memory-mb"},
			&cli.IntFlag{Name: "processes"},
			&cli.IntFlag{Name: "open-files"},
		},
		Action: runDeptAppExec,
	}
}

func runDeptAppExec(_ context.Context, cmd *cli.Command) error {
	argv := cmd.Args().Slice()
	if len(argv) == 0 {
		return errors.New("no command to run: pass it after --")
	}
	spec := sandboxSpec{
		ReadOnly:     cmd.StringSlice("ro"),
		ReadWrite:    cmd.StringSlice("rw"),
		AllowNetwork: cmd.Bool("allow-network"),
		GiteaPID:     cmd.Int("gitea-pid"),
		Limits: AppLimits{
			MemoryMB:  cmd.Int("memory-mb"),
			Processes: cmd.Int("processes"),
			OpenFiles: cmd.Int("open-files"),
		},
	}
	// The environment is inherited from Gitea's carefully constructed
	// cmd.Env (company/appproc.go's buildEnv), never rebuilt here — passing
	// secrets as flags would put them in argv, where `ps` shows them to
	// anyone on the host.
	if err := sandboxAndExec(spec, argv, os.Environ()); err != nil {
		return fmt.Errorf("could not start the app inside the sandbox: %w", err)
	}
	return nil // unreachable: a successful exec never returns
}
