package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const (
	defaultDBPath  = "aoe-queue.db"
	defaultBaseURL = "http://localhost:8080"
	pollInterval   = 5 * time.Second
	stableNeeded   = 3 // consecutive identical polls before declaring idle
	idleTimeout    = 15 * time.Minute
)

type Config struct {
	SessionID string
	Token     string
	BaseURL   string
}

func main() {
	log.SetFlags(log.Ltime)
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}
	switch os.Args[1] {
	case "enqueue":
		cmdEnqueue(os.Args[2:])
	case "run":
		cmdRun(os.Args[2:])
	case "list":
		cmdList(os.Args[2:])
	default:
		usage()
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `aoe-queue - enqueue and send commands to Agent of Empires sessions

Usage:
  aoe-queue enqueue --session <id> [--db path] "cmd1" "cmd2" ...
  aoe-queue run     --session <id> --token <token> [--db path] [--url base-url]
  aoe-queue list    [--session <id>] [--db path]

Env vars: AOE_SESSION, AOE_TOKEN, AOE_URL`)
}

func openDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS messages (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		session_id TEXT NOT NULL,
		command    TEXT NOT NULL,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	)`)
	if err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func cmdEnqueue(args []string) {
	fs := flag.NewFlagSet("enqueue", flag.ExitOnError)
	session := fs.String("session", os.Getenv("AOE_SESSION"), "Session ID")
	dbPath := fs.String("db", defaultDBPath, "SQLite DB path")
	fs.Parse(args)

	cmds := fs.Args()
	if *session == "" || len(cmds) == 0 {
		fmt.Fprintln(os.Stderr, "enqueue: --session and at least one command required")
		os.Exit(1)
	}

	db, err := openDB(*dbPath)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer db.Close()

	tx, err := db.Begin()
	if err != nil {
		log.Fatalf("begin tx: %v", err)
	}
	for _, cmd := range cmds {
		if _, err := tx.Exec("INSERT INTO messages (session_id, command) VALUES (?, ?)", *session, cmd); err != nil {
			tx.Rollback()
			log.Fatalf("insert: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		log.Fatalf("commit: %v", err)
	}
	log.Printf("Enqueued %d command(s) for session %s", len(cmds), *session)
}

func cmdList(args []string) {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	session := fs.String("session", os.Getenv("AOE_SESSION"), "Session ID (optional)")
	dbPath := fs.String("db", defaultDBPath, "SQLite DB path")
	fs.Parse(args)

	db, err := openDB(*dbPath)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer db.Close()

	var (
		rows    *sql.Rows
		queryErr error
	)
	if *session != "" {
		rows, queryErr = db.Query(
			"SELECT id, session_id, command, created_at FROM messages WHERE session_id = ? ORDER BY id",
			*session,
		)
	} else {
		rows, queryErr = db.Query(
			"SELECT id, session_id, command, created_at FROM messages ORDER BY id",
		)
	}
	if queryErr != nil {
		log.Fatalf("query: %v", queryErr)
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var id int64
		var sid, cmd, createdAt string
		rows.Scan(&id, &sid, &cmd, &createdAt)
		fmt.Printf("[%d] session=%-20s created=%s\n    %s\n", id, sid, createdAt, cmd)
		count++
	}
	if count == 0 {
		fmt.Println("No pending messages.")
	}
}

func cmdRun(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	session := fs.String("session", os.Getenv("AOE_SESSION"), "Session ID")
	token := fs.String("token", os.Getenv("AOE_TOKEN"), "API token")
	baseURL := fs.String("url", envOr("AOE_URL", defaultBaseURL), "AOE base URL")
	dbPath := fs.String("db", defaultDBPath, "SQLite DB path")
	fs.Parse(args)

	if *session == "" || *token == "" {
		fmt.Fprintln(os.Stderr, "run: --session and --token required")
		os.Exit(1)
	}

	db, err := openDB(*dbPath)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer db.Close()

	cfg := Config{
		SessionID: *session,
		Token:     *token,
		BaseURL:   strings.TrimRight(*baseURL, "/"),
	}

	if err := processQueue(cfg, db); err != nil {
		log.Fatalf("fatal: %v", err)
	}
}

// ThrottleError is returned when the session output contains a rate-limit message.
type ThrottleError struct {
	ResetAt      string
	WaitDuration time.Duration
}

func (e *ThrottleError) Error() string {
	return fmt.Sprintf("throttled (resets %s UTC), waiting %s", e.ResetAt, e.WaitDuration.Round(time.Second))
}

// Pattern: "You've hit your limit · resets 4:30am (UTC)"
var throttleRe = regexp.MustCompile(`(?i)You.ve hit your limit[^.]*resets\s+(\d{1,2}:\d{2}\s*(?:am|pm))\s*\(UTC\)`)

func detectThrottle(output string) (*ThrottleError, bool) {
	m := throttleRe.FindStringSubmatch(output)
	if m == nil {
		return nil, false
	}
	timeStr := strings.ToUpper(strings.ReplaceAll(m[1], " ", ""))
	t, err := time.Parse("3:04PM", timeStr)
	if err != nil {
		log.Printf("warn: could not parse throttle time %q: %v", m[1], err)
		return nil, false
	}

	now := time.Now().UTC()
	reset := time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, time.UTC)
	if !reset.After(now) {
		reset = reset.Add(24 * time.Hour)
	}

	remaining := time.Until(reset)
	return &ThrottleError{
		ResetAt:      m[1],
		WaitDuration: remaining / 2,
	}, true
}

func apiGet(cfg Config, path string) (*http.Response, error) {
	req, err := http.NewRequest("GET", cfg.BaseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	return http.DefaultClient.Do(req)
}

func apiPost(cfg Config, path string, body []byte) (*http.Response, error) {
	req, err := http.NewRequest("POST", cfg.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	req.Header.Set("Content-Type", "application/json")
	return http.DefaultClient.Do(req)
}

func getOutput(cfg Config) (string, error) {
	resp, err := apiGet(cfg, fmt.Sprintf("/api/sessions/%s/output?lines=200&format=text", cfg.SessionID))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("output API %d: %s", resp.StatusCode, bytes.TrimSpace(body))
	}

	var out struct {
		Content string `json:"content"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.Content, nil
}

func sendMessage(cfg Config, command string) error {
	payload, _ := json.Marshal(map[string]string{"message": command})
	resp, err := apiPost(cfg, fmt.Sprintf("/api/sessions/%s/send", cfg.SessionID), payload)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("send API %d: %s", resp.StatusCode, bytes.TrimSpace(body))
	}
	return nil
}

// waitForStableOutput polls until output stabilizes after changing from `before`.
// Returns *ThrottleError if a rate-limit message is detected in the settled output.
func waitForStableOutput(cfg Config, before string) error {
	deadline := time.Now().Add(idleTimeout)
	prev := before
	stable := 0
	changed := false

	for time.Now().Before(deadline) {
		time.Sleep(pollInterval)

		out, err := getOutput(cfg)
		if err != nil {
			log.Printf("poll error: %v", err)
			continue
		}

		if out != before {
			changed = true
		}

		if changed {
			if out == prev {
				stable++
				if stable >= stableNeeded {
					if te, ok := detectThrottle(out); ok {
						return te
					}
					return nil
				}
			} else {
				stable = 0
			}
		}

		prev = out
	}

	return fmt.Errorf("timeout (%s) waiting for session to become idle", idleTimeout)
}

func processQueue(cfg Config, db *sql.DB) error {
	for {
		var id int64
		var command string
		err := db.QueryRow(
			"SELECT id, command FROM messages WHERE session_id = ? ORDER BY id LIMIT 1",
			cfg.SessionID,
		).Scan(&id, &command)

		if errors.Is(err, sql.ErrNoRows) {
			log.Println("Queue empty — done.")
			return nil
		}
		if err != nil {
			return fmt.Errorf("db query: %w", err)
		}

		log.Printf("[%d] Sending: %.120s", id, command)

		for {
			before, err := getOutput(cfg)
			if err != nil {
				return fmt.Errorf("pre-send snapshot: %w", err)
			}

			if err := sendMessage(cfg, command); err != nil {
				return fmt.Errorf("send: %w", err)
			}

			err = waitForStableOutput(cfg, before)
			var te *ThrottleError
			if errors.As(err, &te) {
				log.Printf("Throttled: resets %s UTC — waiting %s before retry",
					te.ResetAt, te.WaitDuration.Round(time.Second))
				time.Sleep(te.WaitDuration)
				log.Println("Resuming after throttle wait...")
				continue
			}
			if err != nil {
				return err
			}
			break
		}

		if _, err := db.Exec("DELETE FROM messages WHERE id = ?", id); err != nil {
			return fmt.Errorf("delete msg %d: %w", id, err)
		}
		log.Printf("[%d] Sent and removed from queue", id)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
