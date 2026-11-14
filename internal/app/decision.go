package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
)

// Routine delivery must not mention the human itself: the executor asks only
// when there is a concrete decision, after reading the trusted configuration.
const decisionInstruction = "需要用户决策时，先读取 guide.decision_maker，在本讨论发送以该用户提及开头的具体问题、选项和建议，再记录 needs-decision。等待 CI 用 waiting；普通进展不提醒用户，同一未决问题不重复提及。"

// The report stays in the original discussion, even when engineering work is
// linked to another task. Reading it must not consume the collector's inbox.
// This verifies a published question's address and author, not whether the human
// has received/read the mention. The sending executor must check its CLI result.
func (s *Service) verifyDecisionReport(ctx context.Context, w WorkContext, in WorkResult) error {
	if !agentHandleRE.MatchString(w.Guide.DecisionMaker) {
		return errors.New("configure decision_maker before recording needs-decision; do not infer it from a GitHub login")
	}
	root, err := s.fixedTask(ctx, w)
	if err != nil {
		return err
	}
	if err := s.Raft.checkTaskChannel(ctx, root.Channel); err != nil {
		return err
	}
	query := url.Values{"channel": {root.target()}, "around": {in.ReportID}, "limit": {"10"}, "consume": {"false"}}
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
			Content     string `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(raw, &history) != nil || (history.Target != "" && history.Target != root.target() && history.Target != root.Channel+":"+root.MessageID) {
		return errors.New("Raft did not confirm decision report history in the original discussion")
	}
	for _, message := range history.Messages {
		if message.ID != in.ReportID && message.MessageID != in.ReportID {
			continue
		}
		name, kind := message.SenderName, message.SenderType
		if name == "" {
			name = message.SenderName2
		}
		if kind == "" {
			kind = message.SenderType2
		}
		if "@"+strings.TrimPrefix(name, "@") != in.Executor || kind != "agent" {
			return errors.New("decision report must be sent by the current executor")
		}
		// A bare first token avoids counting quotes, code blocks, links, escaped
		// handles or a different user whose handle merely shares a prefix.
		words := strings.Fields(message.Content)
		if len(words) < 2 || words[0] != w.Guide.DecisionMaker {
			return errors.New("decision report must start with the configured human's bare @handle, a space, and the decision question")
		}
		return nil
	}
	return errors.New("decision report not found in the original discussion; send the question before recording needs-decision")
}
