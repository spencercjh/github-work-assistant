package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github-raft-app/internal/database"
)

type Watch struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
}
type Config struct {
	Forks                      []ForkSpec `json:"forks,omitempty"`
	PrivateRepositories        []string   `json:"private_repositories,omitempty"`
	PrivateNotificationsSince  string     `json:"private_notifications_since,omitempty"`
	PrivateNotificationDetails bool       `json:"private_notification_details,omitempty"`
	HistoryRetentionDays       int        `json:"history_retention_days,omitempty"`
	DatabaseDriver             string     `json:"database_driver,omitempty"`
	RaftMode                   string     `json:"raft_mode,omitempty"`
	ExternalAgent              string     `json:"external_agent_id,omitempty"`
	DeliveryTarget             string     `json:"delivery_target,omitempty"`
	Listen                     string     `json:"listen"`
	Origin                     string     `json:"origin"`
	RaftAPI                    string     `json:"raft_api"`
	ClientID                   string     `json:"client_id"`
	ServerID                   string     `json:"server_id"`
	ServerSlug                 string     `json:"server_slug"`
	AgentID                    string     `json:"agent_id"`
	GitHubLogin                string     `json:"github_login"`
	Watches                    []Watch    `json:"watches"`
	PollSeconds                int        `json:"poll_seconds"`
	Discover                   bool       `json:"discover_notifications"`
	NotifyTarget               string     `json:"notify_target"`
	DecisionMaker              string     `json:"decision_maker,omitempty"`
	LookbackHours              int        `json:"lookback_hours"`
	// Only local development may use loopback HTTP. Raft API still requires HTTPS.
	LocalHTTP     bool               `json:"local_http"`
	Repositories  []RepositoryPolicy `json:"repositories,omitempty"`
	WorkspaceRoot string             `json:"workspace_root,omitempty"`
	configDir     string
}

var repoRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

func (w Watch) Key() string { return strings.ToLower(w.Repo) + fmt.Sprintf("#%d", w.Number) }
func LoadConfig(path string) (Config, error) {
	var c Config
	b, e := os.ReadFile(path)
	if e != nil {
		return c, e
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	e = d.Decode(&c)
	if e != nil {
		return c, e
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return c, errors.New("unexpected trailing configuration data")
	}
	c.configDir, e = filepath.Abs(filepath.Dir(path))
	if e != nil {
		return c, e
	}
	if e = c.validateRepositoryAccess(); e != nil {
		return c, e
	}
	if _, e = database.Driver(c.DatabaseDriver); e != nil {
		return c, e
	}
	if c.WorkspaceRoot == "" {
		c.WorkspaceRoot = filepath.Join(c.configDir, "state", "worktrees")
	}
	if !filepath.IsAbs(c.WorkspaceRoot) {
		c.WorkspaceRoot = filepath.Join(c.configDir, c.WorkspaceRoot)
	}
	if c.Listen == "" {
		c.Listen = "127.0.0.1:4185"
	}
	if c.RaftAPI == "" {
		c.RaftAPI = "https://api.raft.build"
	}
	if c.RaftMode == "" {
		c.RaftMode = "app"
	}
	if c.RaftMode != "app" && c.RaftMode != "external" {
		return c, errors.New("raft_mode must be app or external")
	}
	if c.PollSeconds == 0 {
		c.PollSeconds = 120
	}
	if c.HistoryRetentionDays == 0 {
		c.HistoryRetentionDays = 30
	}
	if c.HistoryRetentionDays < 1 || c.HistoryRetentionDays > 3650 {
		return c, errors.New("history_retention_days must be 1..3650")
	}
	if c.LookbackHours == 0 {
		c.LookbackHours = 6
	}
	if c.LookbackHours < 1 || c.LookbackHours > 168 {
		return c, errors.New("lookback_hours must be 1..168")
	}
	if c.NotifyTarget != "" && !strings.HasPrefix(c.NotifyTarget, "dm:@") {
		return c, errors.New("notifications require an explicit DM destination")
	}
	if c.DecisionMaker != "" && !agentHandleRE.MatchString(c.DecisionMaker) {
		return c, errors.New("decision_maker must be one human's stable Raft handle, such as @owner")
	}

	if c.PollSeconds < 60 {
		return c, errors.New("poll_seconds must be at least 60")
	}
	identities := []string{c.ServerID, c.GitHubLogin}
	origins := []string{c.RaftAPI}
	if c.RaftMode == "app" {
		identities = append(identities, c.ClientID, c.AgentID)
		origins = append(origins, c.Origin)
	} else {
		identities = append(identities, c.ExternalAgent)
		if err := c.ValidateDeliveryTarget(); err != nil {
			return c, err
		}
	}
	for _, s := range identities {
		if s == "" {
			return c, errors.New("required Raft and GitHub identities are missing")
		}
	}
	for _, raw := range origins {
		u, e := url.Parse(raw)
		if e != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.Trim(u.Path, "/") != "" {
			return c, errors.New("origins must be absolute URLs without path, credentials or query")
		}
		if u.Scheme != "https" && !(c.RaftMode == "app" && raw != c.RaftAPI && raw == c.Origin && c.LocalHTTP && u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost")) {
			return c, errors.New("HTTPS required except explicitly enabled local app HTTP")
		}
	}
	c.Origin = strings.TrimRight(c.Origin, "/")
	c.RaftAPI = strings.TrimRight(c.RaftAPI, "/")
	if len(c.Watches) == 0 && len(c.Forks) == 0 && !c.Discover {
		return c, errors.New("configure a watch, fork maintenance rule or discover_notifications")
	}
	seen := map[string]bool{}
	for _, w := range c.Watches {
		if !repoRE.MatchString(w.Repo) || strings.Contains(w.Repo, "..") || w.Number < 1 {
			return c, errors.New("invalid watch")
		}
		if seen[w.Key()] {
			return c, errors.New("duplicate watch")
		}
		seen[w.Key()] = true
	}
	seenRepos := map[string]bool{}
	for _, p := range c.Repositories {
		if err := p.validate(); err != nil {
			return c, err
		}
		key := strings.ToLower(p.Repo)
		if seenRepos[key] {
			return c, errors.New("duplicate repository policy")
		}
		seenRepos[key] = true
		if _, err := c.RepositoryGuide(p.Repo); err != nil {
			return c, err
		}
	}
	if err := c.validateForks(); err != nil {
		return c, err
	}
	return c, nil
}

// Private files live beside the instance configuration; explicit environment paths
// can still select an operator-managed secret store.
func (c Config) PrivateFile(name string) string {
	return filepath.Join(c.configDir, "private", name)
}

func (c Config) DatabaseLocation(dsn string) string {
	kind, _ := database.Driver(c.DatabaseDriver)
	if kind == "sqlite" && !filepath.IsAbs(dsn) {
		return filepath.Join(c.configDir, dsn)
	}
	return dsn
}
func (c Config) Watch(key string) (Watch, bool) {
	for _, w := range c.Watches {
		if w.Key() == strings.ToLower(key) {
			return w, true
		}
	}
	return Watch{}, false
}
