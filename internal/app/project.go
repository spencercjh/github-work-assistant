package app

import (
	"errors"
	"slices"
)

// A project channel is trusted routing configuration, not a grant to its
// members. Raft remains the authority for each participant's claimed task.
func (c Config) DeliveryTargetFor(key string) string {
	if f, ok := c.Fork(key); ok && f.Channel != "" {
		return f.Channel
	}
	return c.DeliveryTarget
}

func (c Config) SharedTasksFor(key string) bool {
	return c.RaftMode == "external" && taskChannelRE.MatchString(c.DeliveryTargetFor(key))
}

func (c Config) Project(key string) bool {
	f, ok := c.Fork(key)
	return ok && f.Channel != "" && c.RaftMode == "external"
}

func (c Config) TaskChannels() []string {
	var channels []string
	if c.RaftMode != "external" {
		return channels
	}
	if taskChannelRE.MatchString(c.DeliveryTarget) {
		channels = append(channels, c.DeliveryTarget)
	}
	for _, f := range c.Forks {
		if f.Channel != "" && !slices.Contains(channels, f.Channel) {
			channels = append(channels, f.Channel)
		}
	}
	return channels
}

func (c Config) discussionResource(key string) string {
	if c.Project(key) {
		// Changing a project's destination establishes a new coordination task;
		// it never moves old messages or rewrites their durable receipts.
		return key + ":channel:" + c.DeliveryTargetFor(key)
	}
	return key
}

func (r *Raft) forResource(key string) *Raft {
	copy := *r
	copy.Config.DeliveryTarget = r.Config.DeliveryTargetFor(key)
	return &copy
}

func (c Config) validateProjectChannels() error {
	for _, f := range c.Forks {
		if f.Channel != "" && (c.RaftMode != "external" || !taskChannelRE.MatchString(f.Channel)) {
			return errors.New("fork channel requires external mode and a top-level channel")
		}
	}
	return nil
}

const projectCollaborationInstruction = `This fork uses a project channel. The persistent root task coordinates the project; it is not the only executor. Associate existing top-level tasks from that channel with the current PR/batch using register-work-item: one sync item for source writes, test items for the trusted validator, review items for independent reports, and custom kinds for investigation. Each item has its own live owner and thread. Participants use their own task UUID with verify-claim, work-context and link-task, and publish their own thread report before record-contribution. Registration does not assign ownership or grant additional GitHub actions. An independent review work item may check the review action only when the repository guide already delegates it. A source writer or tester must use the coordinator-selected batch; reading/linking/reporting on another queued PR does not select it. Do not reuse a task across PRs. Rebuilds retain task scope but invalidate results when the head, base, mapping or guide changes. Check current result bindings before acting; a contribution is not a GitHub approval or merge permission. A paused/closed project coordinator blocks participants. Never transfer, resume or duplicate someone else's task automatically.`
