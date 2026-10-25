package app

import (
	"errors"
	"strings"
	"time"
)

// Private repositories are opt-in. Patterns are exact owner/repo, owner/* or *;
// neither matching nor configuration grants additional GitHub permissions.
func privateRepositoryAllowed(patterns []string, repo string) bool {
	if !repoRE.MatchString(repo) || strings.Contains(repo, "..") {
		return false
	}
	repo = strings.ToLower(repo)
	owner, _, _ := strings.Cut(repo, "/")
	for _, pattern := range patterns {
		pattern = strings.ToLower(pattern)
		if pattern == "*" || pattern == repo || pattern == owner+"/*" {
			return true
		}
	}
	return false
}

func (c Config) validateRepositoryAccess() error {
	seen := map[string]bool{}
	for _, pattern := range c.PrivateRepositories {
		key := strings.ToLower(pattern)
		probe := strings.TrimSuffix(key, "/*") + "/repository"
		if key != "*" && !((repoRE.MatchString(key) || (strings.HasSuffix(key, "/*") && repoRE.MatchString(probe))) && !strings.Contains(key, "..")) {
			return errors.New("private_repositories entries must be owner/repo, owner/* or *")
		}
		if seen[key] {
			return errors.New("duplicate private_repositories entry")
		}
		seen[key] = true
	}
	if c.PrivateNotificationsSince != "" {
		if _, err := time.Parse(time.RFC3339, c.PrivateNotificationsSince); err != nil {
			return errors.New("private_notifications_since must be an RFC3339 timestamp")
		}
	}
	return nil
}
