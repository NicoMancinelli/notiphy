package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"text/tabwriter"

	"github.com/NicoMancinelli/notiphy/internal/config"
	"github.com/NicoMancinelli/notiphy/internal/store"
)

// openStore opens the database directly. Token management works against the
// file rather than the API so you can mint the first token before (or without)
// the server running.
func openStore(dbFlag, cfgFlag string) (*store.Store, error) {
	path := dbFlag
	if path == "" {
		cfg, err := config.Load(cfgFlag)
		if err != nil {
			return nil, err
		}
		path = cfg.DB
	}
	return store.Open(path)
}

func cmdToken(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: notiphy token create|list|revoke [flags]")
	}

	fs := flag.NewFlagSet("token", flag.ExitOnError)
	var (
		dbPath  = fs.String("db", "", "SQLite database path")
		cfgPath = fs.String("config", "", "config file to read the db path from")
		name    = fs.String("name", "", "token name (create only)")
		baseURL = fs.String("base-url", "", "base URL to print alongside the token")
	)
	sub := args[0]
	parseArgs(fs, args[1:])

	st, err := openStore(*dbPath, *cfgPath)
	if err != nil {
		return err
	}
	defer st.Close()

	base := *baseURL
	if base == "" {
		if cfg, err := config.Load(*cfgPath); err == nil {
			base = cfg.BaseURL
		}
	}

	switch sub {
	case "create":
		tok, err := st.CreateToken(*name)
		if err != nil {
			return err
		}
		fmt.Println(tok.Token)
		fmt.Fprintf(os.Stderr, "\nwebhook URL:\n  %s/hooks/%s\n\n", base, tok.Token)
		fmt.Fprintf(os.Stderr, "export NOTIPHY_URL=%s\nexport NOTIPHY_TOKEN=%s\n", base, tok.Token)
		return nil

	case "list":
		tokens, err := st.ListTokens()
		if err != nil {
			return err
		}
		if len(tokens) == 0 {
			fmt.Fprintln(os.Stderr, "no tokens yet — run: notiphy token create")
			return nil
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tNAME\tSTATE\tWEBHOOK URL")
		for _, t := range tokens {
			state := "active"
			url := base + "/hooks/" + t.Token
			if t.Revoked {
				state, url = "revoked", "—"
			}
			name := t.Name
			if name == "" {
				name = "-"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", t.ID, name, state, url)
		}
		return w.Flush()

	case "revoke":
		if fs.NArg() == 0 {
			return fmt.Errorf("usage: notiphy token revoke <token-id>")
		}
		if err := st.RevokeToken(fs.Arg(0)); err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, "revoked", fs.Arg(0))
		return nil

	default:
		return fmt.Errorf("unknown token subcommand %q (want create, list, or revoke)", sub)
	}
}

func cmdDevice(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: notiphy device list|test|rm [flags]")
	}

	fs := flag.NewFlagSet("device", flag.ExitOnError)
	server := fs.String("server", "", "notiphy server base URL (or NOTIPHY_URL)")
	admin := fs.String("admin-token", "", "operator token (or NOTIPHY_ADMIN_TOKEN)")
	sub := args[0]
	parseArgs(fs, args[1:])

	base := *server
	if base == "" {
		base = os.Getenv("NOTIPHY_URL")
	}
	if base == "" {
		if cfg, err := config.Load(""); err == nil {
			base = cfg.BaseURL
		}
	}
	// Device management needs the operator token rather than a webhook token:
	// these endpoints add delivery targets, so they are gated separately.
	c := &client{baseURL: base, admin: adminToken(*admin), http: http.DefaultClient}

	switch sub {
	case "list":
		var out struct {
			Devices []struct {
				ID           string `json:"id"`
				Name         string `json:"name"`
				Transport    string `json:"transport"`
				Platform     string `json:"platform"`
				Disabled     bool   `json:"disabled"`
				Buttons      bool   `json:"buttons"`
				LiveActivity bool   `json:"liveActivity"`
			} `json:"devices"`
		}
		if _, err := c.do(http.MethodGet, base+"/api/devices", nil, &out); err != nil {
			return err
		}
		if len(out.Devices) == 0 {
			fmt.Fprintf(os.Stderr, "no devices yet — register one at %s/subscribe\n", base)
			return nil
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tNAME\tTRANSPORT\tPLATFORM\t1-TAP\tLIVE\tSTATE")
		for _, d := range out.Devices {
			state := "enabled"
			if d.Disabled {
				state = "disabled"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				d.ID, d.Name, d.Transport, d.Platform,
				yesNo(d.Buttons), yesNo(d.LiveActivity), state)
		}
		return w.Flush()

	case "test":
		if fs.NArg() == 0 {
			return fmt.Errorf("usage: notiphy device test <device-id>")
		}
		if _, err := c.do(http.MethodPost, base+"/api/devices/"+fs.Arg(0)+"/test", nil, nil); err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, "test push sent — check your phone")
		return nil

	case "rm":
		if fs.NArg() == 0 {
			return fmt.Errorf("usage: notiphy device rm <device-id>")
		}
		if _, err := c.do(http.MethodDelete, base+"/api/devices/"+fs.Arg(0), nil, nil); err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, "removed", fs.Arg(0))
		return nil

	default:
		return fmt.Errorf("unknown device subcommand %q (want list, test, or rm)", sub)
	}
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
