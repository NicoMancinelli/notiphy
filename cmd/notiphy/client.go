package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// client talks to a notiphy server over the same public API a webhook uses, so
// the CLI works identically against localhost and a remote tailnet host.
type client struct {
	baseURL string
	token   string
	http    *http.Client
}

// cliConfig is the on-disk client config at ~/.config/notiphy/config.json.
type cliConfig struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

func configPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "notiphy", "config.json")
}

// newClient resolves connection settings with flags > env > config file.
func newClient(urlFlag, tokenFlag string) (*client, error) {
	url, token := urlFlag, tokenFlag

	if url == "" {
		url = os.Getenv("NOTIPHY_URL")
	}
	if token == "" {
		token = os.Getenv("NOTIPHY_TOKEN")
	}

	if url == "" || token == "" {
		if p := configPath(); p != "" {
			if data, err := os.ReadFile(p); err == nil {
				var c cliConfig
				if err := json.Unmarshal(data, &c); err == nil {
					if url == "" {
						url = c.URL
					}
					if token == "" {
						token = c.Token
					}
				}
			}
		}
	}

	if url == "" {
		return nil, fmt.Errorf("no server URL: pass --url, set NOTIPHY_URL, or write %s", configPath())
	}
	if token == "" {
		return nil, fmt.Errorf("no webhook token: pass --token, set NOTIPHY_TOKEN, or write %s", configPath())
	}

	return &client{
		baseURL: strings.TrimRight(url, "/"),
		token:   token,
		http:    &http.Client{Timeout: 30 * time.Second},
	}, nil
}

// hookURL builds a URL under this client's webhook token.
func (c *client) hookURL(suffix string) string {
	return c.baseURL + "/hooks/" + c.token + suffix
}

// do sends a JSON request and decodes the JSON response. It returns the status
// code so callers can distinguish 409 and 404 from transport failures.
func (c *client) do(method, url string, body any, out any) (int, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, fmt.Errorf("marshal request: %w", err)
		}
		rdr = bytes.NewReader(b)
	}

	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		return 0, fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("connect to %s: %w", c.baseURL, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			return resp.StatusCode, fmt.Errorf("server returned %d: %s", resp.StatusCode, e.Error)
		}
		return resp.StatusCode, fmt.Errorf("server returned %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, fmt.Errorf("decode response: %w", err)
		}
	}
	return resp.StatusCode, nil
}

// --- shared response shapes ---

type responseView struct {
	ID            string `json:"id"`
	Type          string `json:"type"`
	Status        string `json:"status"`
	CorrelationID string `json:"correlationId"`
	Answer        string `json:"answer"`
	AnsweredBy    string `json:"answeredBy"`
	ExpiresAt     int64  `json:"expiresAt"`
	ApprovalURL   string `json:"approvalUrl"`
}

type notifyResult struct {
	OK         bool          `json:"ok"`
	EventID    string        `json:"eventId"`
	Delivered  int           `json:"delivered"`
	Idempotent bool          `json:"idempotent"`
	Response   *responseView `json:"response"`
	Warning    string        `json:"warning"`
}

type activityResult struct {
	OK        bool     `json:"ok"`
	ID        string   `json:"id"`
	Key       string   `json:"key"`
	Title     string   `json:"title"`
	Status    string   `json:"status"`
	Progress  *float64 `json:"progress"`
	State     string   `json:"state"`
	Seq       int      `json:"seq"`
	Delivered int      `json:"delivered"`
	LiveURL   string   `json:"liveUrl"`
	Native    bool     `json:"native"`
	Warning   string   `json:"warning"`
}
