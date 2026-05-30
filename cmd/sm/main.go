// Command sm is the agentic sessions manager CLI and daemon entry point.
//
// Usage:
//
//	sm daemon              # run the long-running manager (systemd --user)
//	sm ls [--all]          # list active sessions (--all/-a includes finished)
//	sm show <id>           # details + recent events
//	sm mark <id> <state>   # override session state (e.g. idle)
//	sm status              # JSON snapshot for walker / scripts
//	sm focus <id>          # raise the window/pane hosting a session's agent
//	sm emit <kind> [k=v]   # publish an event (low-level)
//	sm hook <agent>        # entry point for agent hook scripts; reads stdin
package main

import (
	"fmt"
	"os"

	"github.com/just-barcodes/agentic-sessions-manager/internal/cli"
	"github.com/just-barcodes/agentic-sessions-manager/internal/daemon"
	"github.com/just-barcodes/agentic-sessions-manager/internal/hook"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]

	var err error
	switch cmd {
	case "daemon":
		err = daemon.Run(args)
	case "ls":
		err = cli.List(args)
	case "show":
		err = cli.Show(args)
	case "mark":
		err = cli.Mark(args)
	case "status":
		err = cli.Status(args)
	case "focus":
		err = cli.Focus(args)
	case "emit":
		err = cli.Emit(args)
	case "hook":
		err = hook.Run(args)
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "sm:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: sm <daemon|ls|show|mark|status|focus|emit|hook> [args...]")
}
