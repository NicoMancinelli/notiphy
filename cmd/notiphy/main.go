// Command notiphy is a self-hosted webhook-to-phone notification server and
// its client. One binary is the whole product: `notiphy serve` runs the server,
// and the other subcommands talk to it.
package main

import (
	"fmt"
	"os"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "dev"

// Exit codes. These are the contract for scripts and agent hooks, so they are
// documented here rather than scattered through the commands.
const (
	exitOK        = 0 // success; an approval was granted
	exitError     = 1 // usage or transport error
	exitDenied    = 2 // the request was explicitly denied
	exitTimeout   = 4 // expired before anyone answered
	exitNoDevices = 7 // nothing registered to notify
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(exitError)
	}

	var err error
	switch os.Args[1] {
	case "serve":
		err = cmdServe(os.Args[2:])
	case "token":
		err = cmdToken(os.Args[2:])
	case "device":
		err = cmdDevice(os.Args[2:])
	case "send":
		err = cmdSend(os.Args[2:])
	case "ask":
		err = cmdAsk(os.Args[2:])
	case "activity":
		err = cmdActivity(os.Args[2:])
	case "hook":
		err = cmdHook(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Println("notiphy", version)
		return
	case "help", "--help", "-h":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "notiphy: unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(exitError)
	}

	if err != nil {
		// Commands that need a specific exit code return an exitError value.
		if ec, ok := err.(*exitCodeError); ok {
			if ec.msg != "" {
				fmt.Fprintln(os.Stderr, "notiphy:", ec.msg)
			}
			os.Exit(ec.code)
		}
		fmt.Fprintln(os.Stderr, "notiphy:", err)
		os.Exit(exitError)
	}
}

// exitCodeError carries a specific process exit code up to main.
type exitCodeError struct {
	code int
	msg  string
}

func (e *exitCodeError) Error() string { return e.msg }

func exitWith(code int, format string, args ...any) error {
	return &exitCodeError{code: code, msg: fmt.Sprintf(format, args...)}
}

func usage() {
	fmt.Fprint(os.Stderr, `notiphy — self-hosted webhooks to phone notifications

USAGE
  notiphy <command> [flags]

SERVER
  serve                 Run the notification server
  token create|list|revoke
                        Manage webhook tokens (operates on the database directly)
  device list|test|rm   Manage registered devices

CLIENT  (needs NOTIPHY_URL and NOTIPHY_TOKEN, or --url/--token)
  send <body>           Send a notification
  ask <body>            Ask for an approval, yes/no, or text reply
  activity start|update|end
                        Drive a Live Activity
  hook                  Claude Code / agent permission hook (reads JSON on stdin)

EXIT CODES (ask, hook)
  0  approved / yes        2  denied / no
  4  timed out             7  no devices registered
  1  usage or network error

Run "notiphy <command> -h" for the flags of a command.
`)
}
