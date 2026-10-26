package app

import (
	"encoding/json"
	"strings"
	"testing"
)

func noticeFixture(classification string) Snapshot {
	return Snapshot{"example/fixture#1", classification, Objects{
		"issue": {"title": "修复显存分配失败", "state": "open"},
		"pull":  {"state": "open", "merged": false, "head": "first"},
	}}
}

func TestNoticeUsesEventSubjectAndConcreteChanges(t *testing.T) {
	store := freshTestStore(t)
	snapshot := noticeFixture("public")
	if _, err := store.Observe(t.Context(), snapshot.Key, canonical(snapshot)); err != nil {
		t.Fatal(err)
	}
	snapshot.Objects["comment:7"] = map[string]any{"actor": "alice", "body": "请补充多容器用例。"}
	snapshot.Objects["check:8"] = map[string]any{"name": "unit-tests", "status": "completed", "conclusion": "failure"}
	id, err := store.Observe(t.Context(), snapshot.Key, canonical(snapshot))
	if err != nil {
		t.Fatal(err)
	}
	event, err := store.Event(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	// The next observation renames the PR before the previous event is sent.
	snapshot.Objects["issue"]["title"] = "不应替代旧事件的标题"
	if _, err := store.Observe(t.Context(), snapshot.Key, canonical(snapshot)); err != nil {
		t.Fatal(err)
	}
	r := &Raft{Store: store}
	notice, err := r.notice(t.Context(), event)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"检查失败", "修复显存分配失败", "alice", "请补充多容器用例", "unit-tests", "https://github.com/example/fixture/pull/1"} {
		if !strings.Contains(notice.Title+notice.Body, want) {
			t.Errorf("missing useful context: %s", want)
		}
	}
	if strings.Contains(notice.Title, "不应替代") || strings.Contains(notice.Title, event.ID) {
		t.Fatal("title uses another observation or transport identity")
	}
	t.Log(notice.Title + "\n" + notice.Body)
}

func TestNoticeKeepsPrivateBodiesLocalAndMetadataOptIn(t *testing.T) {
	payload := map[string]any{"data_classification": "private", "subject": eventSubject{"PR", "内部功能标题"}, "changes": []Change{
		{"comment:7", nil, map[string]any{"actor": "private-person", "body": "PRIVATE COMMENT BODY"}, "added"},
		{"check:8", nil, map[string]any{"name": "internal-check", "conclusion": "failure"}, "added"},
	}}
	event := Event{Key: "private-owner/private-project#42", Payload: canonical(payload)}
	r := &Raft{}
	for _, details := range []bool{false, true} {
		r.Config.PrivateNotificationDetails = details
		notice, err := r.notice(t.Context(), event)
		if err != nil {
			t.Fatal(err)
		}
		text := notice.Title + notice.Body
		if strings.Contains(text, "PRIVATE COMMENT BODY") {
			t.Fatal("private comment was copied")
		}
		for _, metadata := range []string{"内部功能标题", "private-owner/private-project", "private-person", "internal-check"} {
			if strings.Contains(text, metadata) != details {
				t.Errorf("metadata visibility differs from explicit option: %s", metadata)
			}
		}
		if !strings.Contains(text, "检查失败") || !strings.Contains(text, "新增评论") {
			t.Fatal("private summary lost useful event categories")
		}
	}
}

func TestNoticeDistinguishesHistoricalAndUncertainChanges(t *testing.T) {
	for _, tc := range []struct {
		name         string
		changes      []Change
		want, absent string
	}{
		{"baseline", []Change{{"pull", nil, map[string]any{"merged": true}, "baseline"}}, "首次读取当前状态：PR（已合并）", "- PR 已合并"},
		{"history", []Change{{"comment:1", nil, map[string]any{"body": "old"}, "backfill"}}, "首次读取时补查的近期历史记录", ""},
		{"missing", []Change{{"comment:1", map[string]any{"body": "old"}, nil, "not_observed"}}, "不能据此判断它们已删除", "评论已删除"},
		{"edited review", []Change{{"review:1", map[string]any{"state": "APPROVED", "body": "old"}, map[string]any{"state": "APPROVED", "body": "new"}, "changed"}}, "评审内容已更新", "评审通过"},
		{"merge", []Change{{"pull", map[string]any{"merged": false, "state": "open"}, map[string]any{"merged": true, "state": "closed"}, "changed"}}, "PR 已合并", "PR 已关闭"},
		{"reopen", []Change{{"pull", map[string]any{"state": "closed"}, map[string]any{"state": "open"}, "changed"}}, "PR 已重新打开", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			event := Event{Key: "example/fixture#1", Payload: canonical(map[string]any{"subject": eventSubject{"PR", "标题"}, "changes": tc.changes})}
			notice, err := (&Raft{}).notice(t.Context(), event)
			if err != nil || !strings.Contains(notice.Body, tc.want) || (tc.absent != "" && strings.Contains(notice.Body, tc.absent)) {
				t.Fatal("misleading change description", notice, err)
			}
		})
	}
}

func TestNoticeBoundsAndQuotesGitHubProse(t *testing.T) {
	payload := map[string]any{"subject": eventSubject{"Issue", "fix @everyone\n#channel\u202e [link](https://example.invalid)"}, "changes": []Change{
		{"comment:1", nil, map[string]any{"actor": "@worker", "body": "<img> [click](https://example.invalid) `code` @everyone " + strings.Repeat("长", 400)}, "added"},
	}}
	notice, err := (&Raft{}).notice(t.Context(), Event{Key: "example/fixture#1", Payload: canonical(payload)})
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(notice.Title, "\n\r\u202e") || strings.Contains(notice.Title+notice.Body, "@everyone") || strings.Contains(notice.Body, "<img>") || strings.Contains(notice.Body, "[click](") || strings.Contains(notice.Body, strings.Repeat("长", 151)) {
		t.Fatal("untrusted prose was not bounded or escaped", notice)
	}
	if !strings.Contains(notice.Body, "…") || !strings.Contains(notice.Body, "＠worker") {
		t.Fatal("excerpt or actor missing")
	}
}

func TestPrivateNoticeRetryKeepsFrozenVisibility(t *testing.T) {
	f := &taskAPI{lostReply: true, status: "todo"}
	svc, _ := sharedFixture(t, f)
	snapshot := noticeFixture("private")
	id, err := svc.Store.Observe(t.Context(), snapshot.Key, canonical(snapshot))
	if err != nil {
		t.Fatal(err)
	}
	event, err := svc.Store.Event(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Raft.Send(t.Context(), event); err == nil {
		t.Fatal("lost reply accepted")
	}
	first := f.first
	svc.Raft.Config.PrivateNotificationDetails = true
	if _, err := svc.Raft.Send(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	if f.first != first || f.creates != 1 || strings.Contains(f.first, "修复显存") || strings.Contains(f.first, "example/fixture") {
		t.Fatal("retry rewrote the prepared task")
	}
	var request struct {
		Tasks []map[string]string `json:"tasks"`
	}
	if json.Unmarshal([]byte(f.first), &request) != nil || !strings.Contains(request.Tasks[0]["description"], event.ID) {
		t.Fatal("executor lost event identity")
	}
}
