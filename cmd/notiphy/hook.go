package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// hookInput is the JSON Claude Code (and compatible agents) write to a hook's
// stdin. Only the fields we actually use are declared.
type hookInput struct {
	SessionID     string         `json:"session_id"`
	CWD           string         `json:"cwd"`
	HookEventName string         `json:"hook_event_name"`
	ToolName      string         `json:"tool_name"`
	ToolInput     map[string]any `json:"tool_input"`
	Message       string         `json:"message"`
}

// hookOutput is Claude Code's structured hook result. Emitting a decision here
// is more explicit than relying on exit codes alone.
type hookOutput struct {
	HookSpecificOutput struct {
		HookEventName            string `json:"hookEventName"`
		PermissionDecision       string `json:"permissionDecision,omitempty"`
		PermissionDecisionReason string `json:"permissionDecisionReason,omitempty"`
	} `json:"hookSpecificOutput"`
}

func cmdHook(args []string) error {
	fs := flag.NewFlagSet("hook", flag.ExitOnError)
	server, token := connFlags(fs)
	var (
		timeout     = fs.Duration("timeout", 5*time.Minute, "how long to wait for an answer")
		agent       = fs.String("agent", "Claude Code", "agent name shown on the phone")
		onTimeout   = fs.String("on-timeout", "deny", "what to do if nobody answers: deny or allow")
		printConfig = fs.Bool("print-config", false, "print the Claude Code settings snippet and exit")
	)
	parseArgs(fs, args)

	if *printConfig {
		printHookConfig()
		return nil
	}

	raw, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	if err != nil {
		return fmt.Errorf("read hook input: %w", err)
	}

	var in hookInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return fmt.Errorf("parse hook input: %w", err)
	}

	c, err := newClient(*server, *token)
	if err != nil {
		return err
	}

	project := filepath.Base(strings.TrimRight(in.CWD, "/"))
	if project == "" || project == "." {
		project = "workspace"
	}

	switch in.HookEventName {
	case "PreToolUse", "PermissionRequest":
		return hookApproval(c, in, *agent, project, *timeout, *onTimeout)
	case "Stop", "SubagentStop":
		return hookNotify(c, *agent+" finished", project, in.Message)
	default:
		// Notification and anything else: a plain "needs you" ping.
		msg := in.Message
		if msg == "" {
			msg = *agent + " needs your attention"
		}
		return hookNotify(c, *agent, project, msg)
	}
}

// hookApproval asks for permission and blocks until answered.
//
// The payload is deliberately thin: agent, tool, project directory, and how
// many arguments the call carries. Command text, file contents, URLs, and
// environment never leave the machine — a notification is not a secure channel
// and lands on a lock screen.
func hookApproval(c *client, in hookInput, agent, project string, timeout time.Duration, onTimeout string) error {
	tool := in.ToolName
	if tool == "" {
		tool = "an unnamed tool"
	}

	body := fmt.Sprintf("%s wants to run %s in %s", agent, tool, project)
	if n := len(in.ToolInput); n > 0 {
		body += fmt.Sprintf(" (%d argument%s)", n, plural(n))
	}

	req := map[string]any{
		"title":    "Permission request",
		"body":     body,
		"priority": 4,
		"response": map[string]any{
			"type":             "approval",
			"expiresInSeconds": int(timeout.Seconds()),
			"correlationId":    in.SessionID,
		},
	}

	var out notifyResult
	if _, err := c.do(http.MethodPost, c.hookURL(""), req, &out); err != nil {
		// A hook that cannot reach the server must not wedge the agent. Fail
		// open to "ask" so Claude Code falls back to its own terminal prompt.
		fmt.Fprintf(os.Stderr, "notiphy: could not ask for approval (%v); falling back to the local prompt\n", err)
		emitDecision(in.HookEventName, "ask", "notiphy was unreachable")
		return nil
	}
	if out.Response == nil {
		emitDecision(in.HookEventName, "ask", "notiphy returned no response object")
		return nil
	}

	fmt.Fprintf(os.Stderr, "notiphy: waiting for approval on %d device(s) — %s\n",
		out.Delivered, out.Response.ApprovalURL)

	// Nothing but the decision object may reach stdout: Claude Code parses it
	// as JSON, and a stray line would make the hook look malformed.
	_, err := c.waitForAnswer(out.EventID, timeout)
	switch {
	case err == nil:
		emitDecision(in.HookEventName, "allow", "approved from phone")
		return nil

	case isExitCode(err, exitDenied):
		emitDecision(in.HookEventName, "deny", "denied from phone")
		// Exit 2 is what blocks the tool call and feeds the reason back to the
		// agent, so keep it alongside the structured output.
		return exitWith(exitDenied, "denied from phone")

	case isExitCode(err, exitTimeout):
		if onTimeout == "allow" {
			emitDecision(in.HookEventName, "allow", "no answer before timeout; configured to allow")
			return nil
		}
		emitDecision(in.HookEventName, "deny", "no answer before timeout")
		return exitWith(exitDenied, "no answer before timeout")

	default:
		fmt.Fprintf(os.Stderr, "notiphy: %v\n", err)
		emitDecision(in.HookEventName, "ask", "notiphy could not determine an answer")
		return nil
	}
}

func hookNotify(c *client, title, project, message string) error {
	if message == "" {
		message = "done"
	}
	req := map[string]any{
		"title":    title,
		"body":     fmt.Sprintf("%s · %s", message, project),
		"priority": 3,
	}
	var out notifyResult
	if _, err := c.do(http.MethodPost, c.hookURL(""), req, &out); err != nil {
		// Never fail an agent turn because a courtesy ping did not send.
		fmt.Fprintf(os.Stderr, "notiphy: notification failed: %v\n", err)
	}
	return nil
}

// emitDecision writes the structured hook result on stdout.
func emitDecision(event, decision, reason string) {
	if event == "" {
		event = "PreToolUse"
	}
	var out hookOutput
	out.HookSpecificOutput.HookEventName = event
	out.HookSpecificOutput.PermissionDecision = decision
	out.HookSpecificOutput.PermissionDecisionReason = reason

	if err := json.NewEncoder(os.Stdout).Encode(out); err != nil {
		fmt.Fprintln(os.Stderr, "notiphy: could not write hook decision:", err)
	}
}

func isExitCode(err error, code int) bool {
	ec, ok := err.(*exitCodeError)
	return ok && ec.code == code
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func printHookConfig() {
	fmt.Print(`Add this to ~/.claude/settings.json (merge with any existing "hooks" block).

Set NOTIPHY_URL and NOTIPHY_TOKEN in your environment first, or pass
--server and --token in the command below.

{
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "Bash|Write|Edit",
        "hooks": [
          { "type": "command", "command": "notiphy hook --timeout 5m" }
        ]
      }
    ],
    "Stop": [
      {
        "hooks": [
          { "type": "command", "command": "notiphy hook" }
        ]
      }
    ]
  }
}

Notes:
  - "matcher" decides which tools require approval. Start narrow; every
    matched call blocks until you answer on your phone.
  - The hook fails open: if the server is unreachable it returns "ask", so
    Claude Code falls back to its normal terminal prompt rather than hanging.
  - Only the agent name, tool name, project directory, and argument count are
    sent. Command text and file contents never leave the machine.
`)
}
