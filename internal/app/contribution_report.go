package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
)

// A participant's report stays in that task's own thread. Reading around its
// exact UUID does not consume unread human messages or follow other threads.
func (s *Service) verifyContributionReport(ctx context.Context, task TaskDispatch, executor, report string) error {
	query := url.Values{"channel": {task.target()}, "around": {report}, "limit": {"10"}, "consume": {"false"}}
	raw, _, err := s.Raft.request(ctx, "/internal/agent-api/history?"+query.Encode(), nil, s.Raft.Credential, false)
	if err != nil {
		return err
	}
	var history struct {
		Target   string `json:"target"`
		Messages []struct {
			ID          string `json:"id"`
			MessageID   string `json:"message_id"`
			SenderName  string `json:"senderName"`
			SenderName2 string `json:"sender_name"`
			SenderType  string `json:"senderType"`
			SenderType2 string `json:"sender_type"`
		} `json:"messages"`
	}
	if json.Unmarshal(raw, &history) != nil || (history.Target != "" && history.Target != task.target() && history.Target != task.Channel+":"+task.MessageID) {
		return errors.New("Raft did not confirm the participant report's thread")
	}
	for _, message := range history.Messages {
		if message.ID != report && message.MessageID != report {
			continue
		}
		name, kind := message.SenderName, message.SenderType
		if name == "" {
			name = message.SenderName2
		}
		if kind == "" {
			kind = message.SenderType2
		}
		if kind == "agent" && "@"+strings.TrimPrefix(name, "@") == executor {
			return nil
		}
		return errors.New("the report was not written by this task's current executor")
	}
	return errors.New("publish the report in the participant task's thread before recording it")
}
