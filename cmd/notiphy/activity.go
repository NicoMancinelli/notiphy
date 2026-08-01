package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
)

// activityBook remembers the server-side activity ID for each --key, so scripts
// can say `activity update --key deploy` without threading an ID through.
type activityBook map[string]string

func bookPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "notiphy", "activities.json")
}

func loadBook() activityBook {
	b := activityBook{}
	p := bookPath()
	if p == "" {
		return b
	}
	if data, err := os.ReadFile(p); err == nil {
		json.Unmarshal(data, &b)
	}
	return b
}

func saveBook(b activityBook) error {
	p := bookPath()
	if p == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o600)
}

// resolveActivityID prefers an explicit ID, then a remembered --key.
func resolveActivityID(id, key string, args []string) (string, error) {
	if id != "" {
		return id, nil
	}
	if len(args) > 0 && args[0] != "" {
		return args[0], nil
	}
	if key != "" {
		if v := loadBook()[key]; v != "" {
			return v, nil
		}
		return "", fmt.Errorf("no activity is being tracked for key %q; start one first", key)
	}
	return "", fmt.Errorf("an activity id (or --key) is required")
}

func cmdActivity(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: notiphy activity start|update|end [flags]")
	}
	switch args[0] {
	case "start":
		return activityStart(args[1:])
	case "update":
		return activityUpdate(args[1:])
	case "end":
		return activityEnd(args[1:])
	default:
		return fmt.Errorf("unknown activity subcommand %q (want start, update, or end)", args[0])
	}
}

func activityStart(args []string) error {
	fs := flag.NewFlagSet("activity start", flag.ExitOnError)
	server, token := connFlags(fs)
	var (
		key      = fs.String("key", "", "stable key so update/end can find this activity")
		title    = fs.String("title", "", "activity title (required)")
		status   = fs.String("status", "", "current status line")
		progress = fs.Float64("progress", -1, "progress 0.0-1.0")
		style    = fs.String("style", "standard", "layout: standard, ring, hero, terminal, steps, approval, shell, verdict, signal")
		symbol   = fs.String("symbol", "", "symbol name")
		accent   = fs.String("accent", "", "accent colour, e.g. #FF9F0A")
		expires  = fs.Duration("expires", 0, "how long the activity lives (default 8h)")
		replace  = fs.Bool("replace", true, "displace an existing activity with the same key")
	)
	parseArgs(fs, args)

	if *title == "" {
		return fmt.Errorf("--title is required")
	}
	c, err := newClient(*server, *token)
	if err != nil {
		return err
	}

	req := map[string]any{"title": *title, "style": *style, "replace": *replace}
	if *key != "" {
		req["key"] = *key
	}
	if *status != "" {
		req["status"] = *status
	}
	if *progress >= 0 {
		req["progress"] = *progress
	}
	if *symbol != "" {
		req["symbol"] = *symbol
	}
	if *accent != "" {
		req["accentColor"] = *accent
	}
	if *expires > 0 {
		req["expiresInSeconds"] = int(expires.Seconds())
	}

	var out activityResult
	if _, err := c.do(http.MethodPost, c.hookURL("/live-activities"), req, &out); err != nil {
		return err
	}

	if *key != "" {
		b := loadBook()
		b[*key] = out.ID
		if err := saveBook(b); err != nil {
			fmt.Fprintln(os.Stderr, "warning: could not remember activity key:", err)
		}
	}

	fmt.Println(out.ID)
	fmt.Fprintf(os.Stderr, "live view: %s\n", out.LiveURL)
	if out.Warning != "" {
		fmt.Fprintln(os.Stderr, "note:", out.Warning)
	}
	return nil
}

func activityUpdate(args []string) error {
	fs := flag.NewFlagSet("activity update", flag.ExitOnError)
	server, token := connFlags(fs)
	var (
		id       = fs.String("id", "", "activity id")
		key      = fs.String("key", "", "activity key from start")
		status   = fs.String("status", "", "new status line")
		progress = fs.Float64("progress", -1, "new progress 0.0-1.0")
		title    = fs.String("title", "", "new title")
		symbol   = fs.String("symbol", "", "new symbol")
		accent   = fs.String("accent", "", "new accent colour")
	)
	rest := parseArgs(fs, args)

	actID, err := resolveActivityID(*id, *key, rest)
	if err != nil {
		return err
	}
	c, err := newClient(*server, *token)
	if err != nil {
		return err
	}

	// Only send fields the caller actually set: the server treats PATCH as a
	// merge, so an absent field must stay absent rather than become "".
	req := map[string]any{}
	if *status != "" {
		req["status"] = *status
	}
	if *progress >= 0 {
		req["progress"] = *progress
	}
	if *title != "" {
		req["title"] = *title
	}
	if *symbol != "" {
		req["symbol"] = *symbol
	}
	if *accent != "" {
		req["accentColor"] = *accent
	}
	if len(req) == 0 {
		return fmt.Errorf("nothing to update: pass at least one of --status, --progress, --title, --symbol, --accent")
	}

	var out activityResult
	if _, err := c.do(http.MethodPatch, c.hookURL("/live-activities/"+actID), req, &out); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "updated %s (seq %d)\n", out.ID, out.Seq)
	return nil
}

func activityEnd(args []string) error {
	fs := flag.NewFlagSet("activity end", flag.ExitOnError)
	server, token := connFlags(fs)
	var (
		id       = fs.String("id", "", "activity id")
		key      = fs.String("key", "", "activity key from start")
		status   = fs.String("status", "", "final status line")
		progress = fs.Float64("progress", -1, "final progress 0.0-1.0")
	)
	fs.Parse(args)

	actID, err := resolveActivityID(*id, *key, fs.Args())
	if err != nil {
		return err
	}
	c, err := newClient(*server, *token)
	if err != nil {
		return err
	}

	req := map[string]any{}
	if *status != "" {
		req["status"] = *status
	}
	if *progress >= 0 {
		req["progress"] = *progress
	}

	var out activityResult
	if _, err := c.do(http.MethodPost, c.hookURL("/live-activities/"+actID+"/end"), req, &out); err != nil {
		return err
	}

	if *key != "" {
		b := loadBook()
		delete(b, *key)
		saveBook(b)
	}
	fmt.Fprintf(os.Stderr, "ended %s\n", out.ID)
	return nil
}
