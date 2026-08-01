package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// connFlags registers the flags every client command shares. The server flag is
// --server so that --url stays free for a notification's tap target.
func connFlags(fs *flag.FlagSet) (server, token *string) {
	server = fs.String("server", "", "notiphy server base URL (or NOTIPHY_URL)")
	token = fs.String("token", "", "webhook token (or NOTIPHY_TOKEN)")
	return
}

func cmdSend(args []string) error {
	fs := flag.NewFlagSet("send", flag.ExitOnError)
	server, token := connFlags(fs)
	var (
		title    = fs.String("title", "", "notification title")
		link     = fs.String("url", "", "URL to open when the notification is tapped")
		image    = fs.String("image", "", "image URL to attach")
		priority = fs.Int("priority", 3, "priority 1-5 (higher breaks through Focus)")
		devices  = fs.String("devices", "", "comma-separated device IDs (default: all)")
		idem     = fs.String("idempotency-key", "", "de-duplicate retries of this send")
	)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "usage: notiphy send <body> [flags]\n\n")
		fs.PrintDefaults()
	}
	rest := parseArgs(fs, args)

	body := strings.Join(rest, " ")
	if body == "" {
		fs.Usage()
		return fmt.Errorf("a message body is required")
	}

	c, err := newClient(*server, *token)
	if err != nil {
		return err
	}

	req := map[string]any{"body": body, "priority": *priority}
	if *title != "" {
		req["title"] = *title
	}
	if *link != "" {
		req["url"] = *link
	}
	if *image != "" {
		req["imageUrl"] = *image
	}
	if *devices != "" {
		req["deviceIds"] = strings.Split(*devices, ",")
	}

	var out notifyResult
	status, err := c.doWithIdempotency(http.MethodPost, c.hookURL(""), req, &out, *idem)
	if err != nil {
		if status == http.StatusBadRequest && strings.Contains(err.Error(), "no enabled devices") {
			return exitWith(exitNoDevices, "%s", err)
		}
		return err
	}

	fmt.Printf("sent %s to %d device(s)\n", out.EventID, out.Delivered)
	if out.Warning != "" {
		fmt.Fprintln(os.Stderr, "warning:", out.Warning)
	}
	return nil
}

// doWithIdempotency is do() plus an optional Idempotency-Key header.
func (c *client) doWithIdempotency(method, url string, body, out any, key string) (int, error) {
	if key == "" {
		return c.do(method, url, body, out)
	}
	// Only the header differs, so wrap the transport rather than duplicating do().
	orig := c.http.Transport
	if orig == nil {
		orig = http.DefaultTransport
	}
	c.http.Transport = headerTransport{base: orig, key: "Idempotency-Key", value: key}
	defer func() { c.http.Transport = nil }()

	return c.do(method, url, body, out)
}

type headerTransport struct {
	base       http.RoundTripper
	key, value string
}

func (t headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set(t.key, t.value)
	return t.base.RoundTrip(r)
}

func cmdAsk(args []string) error {
	fs := flag.NewFlagSet("ask", flag.ExitOnError)
	server, token := connFlags(fs)
	var (
		title    = fs.String("title", "", "notification title")
		approval = fs.Bool("approval", false, "ask for Approve/Deny (default)")
		yesNo    = fs.Bool("yes-no", false, "ask for Yes/No")
		text     = fs.Bool("text", false, "ask for a free-text reply")
		wait     = fs.Bool("wait", false, "block until answered, then exit with a code")
		timeout  = fs.Duration("timeout", 5*time.Minute, "how long the request stays answerable")
		corrID   = fs.String("correlation-id", "", "your own reference, echoed back")
		callback = fs.String("callback", "", "URL to POST the answer to")
		cbToken  = fs.String("callback-token", "", "bearer token for the callback")
		priority = fs.Int("priority", 4, "priority 1-5")
	)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "usage: notiphy ask <question> [flags]\n\n")
		fs.PrintDefaults()
	}
	rest := parseArgs(fs, args)

	question := strings.Join(rest, " ")
	if question == "" {
		fs.Usage()
		return fmt.Errorf("a question is required")
	}

	respType := "approval"
	switch {
	case *yesNo:
		respType = "yes_no"
	case *text:
		respType = "text"
	case *approval:
		respType = "approval"
	}

	c, err := newClient(*server, *token)
	if err != nil {
		return err
	}

	spec := map[string]any{
		"type":             respType,
		"expiresInSeconds": int(timeout.Seconds()),
	}
	if *corrID != "" {
		spec["correlationId"] = *corrID
	}
	if *callback != "" {
		cb := map[string]string{"url": *callback}
		if *cbToken != "" {
			cb["token"] = *cbToken
		}
		spec["callback"] = cb
	}

	req := map[string]any{"body": question, "priority": *priority, "response": spec}
	if *title != "" {
		req["title"] = *title
	}

	var out notifyResult
	status, err := c.do(http.MethodPost, c.hookURL(""), req, &out)
	if err != nil {
		if status == http.StatusBadRequest && strings.Contains(err.Error(), "no enabled devices") {
			return exitWith(exitNoDevices, "%s", err)
		}
		return err
	}
	if out.Response == nil {
		return fmt.Errorf("server did not return a response object")
	}

	if !*wait {
		fmt.Println(out.Response.ID)
		fmt.Fprintf(os.Stderr, "asked on %d device(s); answer at %s\n", out.Delivered, out.Response.ApprovalURL)
		return nil
	}

	fmt.Fprintf(os.Stderr, "waiting for an answer (%s)…\n", timeout.String())
	answer, err := c.waitForAnswer(out.EventID, *timeout)
	if answer != "" {
		fmt.Println(answer)
	}
	return err
}

// waitForAnswer polls until the response settles. It returns the answer text
// and an error carrying the process exit code.
//
// It deliberately prints nothing: `ask` writes the answer to stdout, but the
// agent hook must keep stdout clean for its JSON decision object.
func (c *client) waitForAnswer(eventID string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout + 15*time.Second)

	// Poll briskly at first — most approvals are answered within seconds — then
	// back off so a long timeout does not hammer the server.
	interval := time.Second
	for time.Now().Before(deadline) {
		time.Sleep(interval)
		if interval < 5*time.Second {
			interval += 500 * time.Millisecond
		}

		var out notifyResult
		if _, err := c.do(http.MethodGet, c.hookURL("/events/"+eventID), nil, &out); err != nil {
			return "", err
		}
		if out.Response == nil {
			return "", fmt.Errorf("event %s has no interactive response", eventID)
		}

		switch out.Response.Status {
		case "pending":
			continue
		case "answered":
			answer := out.Response.Answer
			switch answer {
			case "deny", "no":
				return answer, exitWith(exitDenied, "")
			default:
				// approve, yes, or free text — all successes.
				return answer, nil
			}
		case "expired":
			return "", exitWith(exitTimeout, "no answer before the request expired")
		case "cancelled":
			return "", exitWith(exitTimeout, "the request was withdrawn")
		}
	}
	return "", exitWith(exitTimeout, "gave up waiting for an answer")
}
