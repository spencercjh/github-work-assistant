package app

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// Drain the durable inbox, coalescing all due deliveries for one resource into
// one fresh read. Delivery IDs remain independent so crashes are replayable.
func (s *Service) ProcessWebhooks(ctx context.Context) (bool, error) {
	if s.Config.GitHubSource != "webhook" {
		return false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().Unix()
	cooldown, err := s.Store.Meta(ctx, "webhook-rate-until", "0")
	if err != nil {
		return false, err
	}
	until, err := strconv.ParseInt(cooldown, 10, 64)
	if err != nil {
		return false, errors.New("invalid webhook rate-limit deadline")
	}
	if now < until {
		return false, ErrGitHubRateLimited
	}
	deliveries, err := s.Store.PendingWebhooks(ctx, now)
	if err != nil || len(deliveries) == 0 {
		return false, err
	}
	if err := s.reconcileWatches(ctx); err != nil {
		return false, err
	}
	results := map[string]error{}
	identityErr := s.GH.CheckIdentity(ctx)
	var limitErr error
	if errors.Is(identityErr, ErrGitHubRateLimited) {
		limitErr = identityErr
	}
	var errs []error
	for _, delivery := range deliveries {
		pending := []string{}
		failure := ""
		for _, key := range delivery.Keys {
			err, seen := results[key]
			if !seen {
				_, watchConfigured := s.Config.Watch(key)
				_, forkConfigured := s.Config.Fork(key)
				switch {
				case !watchConfigured && !forkConfigured:
					// Removed configuration is not permission to continue work.
				case identityErr != nil:
					err = identityErr
				case limitErr != nil:
					err = limitErr
				default:
					var definition WorkDefinition
					definition, err = s.Config.WorkDefinition(key)
					if err == nil {
						err = definition.Collect(ctx, s)
						if definition.Fork == nil {
							err = errors.Join(err, s.Store.CollectionStatus(ctx, key, err))
						} else {
							message := ""
							if err != nil {
								message = webhookFailure(err)
							}
							err = errors.Join(err, s.Store.PutMeta(ctx, "fork-error:"+definition.Fork.ID, message))
						}
					}
				}
				results[key] = err
				if err != nil {
					errs = append(errs, fmt.Errorf("webhook refresh %s: %w", key, err))
				}
				if errors.Is(err, ErrGitHubRateLimited) {
					limitErr = err
				}
			}
			if err != nil {
				pending = append(pending, key)
				failure = webhookFailure(err)
			}
		}
		delay := webhookRetryDelay(delivery.Attempts)
		if limitErr != nil && delay < time.Minute {
			delay = time.Minute
		}
		if err := s.Store.FinishWebhook(ctx, delivery, pending, failure, now+int64(delay.Seconds())); err != nil {
			errs = append(errs, err)
		}
	}
	if limitErr != nil {
		// A global cooldown also covers newly arriving deliveries. The current
		// gh adapter exposes rate-limit classification, not response headers.
		raw, err := s.Store.Meta(ctx, "webhook-rate-attempts", "0")
		if err != nil {
			return true, errors.Join(append(errs, err)...)
		}
		attempts, err := strconv.Atoi(raw)
		if err != nil {
			return true, errors.Join(append(errs, errors.New("invalid webhook rate-limit counter"))...)
		}
		attempts = min(max(attempts, 0), 6) + 1
		delay := webhookRetryDelay(attempts)
		errs = append(errs, s.Store.PutMeta(ctx, "webhook-rate-attempts", strconv.Itoa(attempts)))
		errs = append(errs, s.Store.PutMeta(ctx, "webhook-rate-until", strconv.FormatInt(now+int64(delay.Seconds()), 10)))
	} else if identityErr == nil {
		errs = append(errs, s.Store.PutMeta(ctx, "webhook-rate-attempts", "0"))
	}
	return true, errors.Join(errs...)
}

func webhookRetryDelay(attempt int) time.Duration {
	attempt = min(max(attempt, 0), 7)
	return min(30*time.Second*time.Duration(1<<attempt), time.Hour)
}

func webhookFailure(err error) string {
	switch {
	case errors.Is(err, ErrGitHubRateLimited):
		return "github_rate_limited"
	case errors.Is(err, ErrGitHubAccessDenied), errors.Is(err, ErrRepositoryNotAllowed):
		return "github_access_denied"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "refresh_interrupted"
	default:
		return "refresh_failed"
	}
}

func (s *Service) collectScheduled(ctx context.Context) (bool, error) {
	if s.Config.GitHubSource != "webhook" {
		return true, s.Collect(ctx)
	}
	activity, err := s.ProcessWebhooks(ctx)
	errs := []error{err}
	// These deadlines refer to local work, not periodically queried GitHub
	// state. Their events retain the original source observation timestamp.
	for _, fork := range s.Config.Forks {
		changed, e := s.Store.TickFork(ctx, fork, time.Now().Unix())
		activity = activity || changed
		errs = append(errs, e)
	}
	return activity, errors.Join(errs...)
}
