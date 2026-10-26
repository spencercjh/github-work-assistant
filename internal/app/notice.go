package app

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"unicode"
)

// Preserve only the subject needed to describe this event, not another snapshot.
// A later observation must not relabel an older pending event with a new title.
type eventSubject struct {
	Kind  string `json:"kind"`
	Title string `json:"title"`
}

func subjectOf(s Snapshot) eventSubject {
	if s.Objects["issue"] == nil {
		return eventSubject{}
	}
	kind := "Issue"
	if s.Objects["pull"] != nil {
		kind = "PR"
	}
	return eventSubject{kind, str(s.Objects["issue"]["title"])}
}

type eventNotice struct{ Title, Body, Subject string }
type noticeChange struct {
	priority    int
	label, text string
}

// GitHub prose is quoted data. Flatten control characters and neutralize Raft
// mentions; Markdown escaping is applied separately to the message body.
func noticeText(s string, limit int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf) {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	s = strings.NewReplacer("@", "＠", "#", "＃").Replace(s)
	r := []rune(s)
	if len(r) > limit {
		s = string(r[:limit]) + "…"
	}
	return s
}

func noticeMarkdown(s string) string {
	return strings.NewReplacer("\\", "\\\\", "`", "\\`", "*", "\\*", "_", "\\_", "[", "\\[", "]", "\\]", "<", "&lt;", ">", "&gt;", "|", "\\|", "~", "\\~").Replace(s)
}

func (r *Raft) notice(ctx context.Context, ev Event) (eventNotice, error) {
	watch, err := resourceWatch(ev.Key)
	if err != nil {
		return eventNotice{}, err
	}
	var p struct {
		Classification string       `json:"data_classification"`
		Subject        eventSubject `json:"subject"`
		Changes        []Change     `json:"changes"`
	}
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		return eventNotice{}, err
	}
	// Older events have no subject. Use only a matching version, never a newer
	// snapshot, and leave the event readable with generic wording if unavailable.
	if p.Subject.Kind == "" && r.Store != nil {
		if body, version, err := r.Store.Snapshot(ctx, ev.Key); err == nil && version == ev.Version {
			var snapshot Snapshot
			if json.Unmarshal(body, &snapshot) == nil {
				p.Subject = subjectOf(snapshot)
			}
		}
	}
	if p.Subject.Kind != "PR" && p.Subject.Kind != "Issue" {
		p.Subject.Kind = "事项"
	}
	private := p.Classification == "private"
	hideMetadata := private && !r.Config.PrivateNotificationDetails
	changes := describeChanges(p.Changes, p.Subject.Kind, hideMetadata, !private)
	subject := fmt.Sprintf("%s %s %d", watch.Repo, p.Subject.Kind, watch.Number)
	if hideMetadata {
		subject = "私有" + p.Subject.Kind
	} else if text := noticeText(p.Subject.Title, 100); text != "" {
		subject += " · " + text
	}
	title := changes[0].label + " · " + subject
	if private && !hideMetadata {
		title = "[私有] " + title
		subject = "[私有] " + subject
	}
	lines := []string{}
	for i, change := range changes {
		if i == 6 {
			lines = append(lines, fmt.Sprintf("- 另有 %d 类变化，完整记录见本机上下文。", len(changes)-i))
			break
		}
		lines = append(lines, "- "+noticeMarkdown(change.text))
	}
	if hideMetadata {
		lines = append(lines, "\n私有事项：标题、参与者和正文请从本机上下文读取；详细提醒仅发送给配置的人类收件人。")
	} else {
		path := "issues"
		if p.Subject.Kind == "PR" {
			path = "pull"
		}
		lines = append(lines, fmt.Sprintf("\n[查看 GitHub %s](https://github.com/%s/%s/%d)", p.Subject.Kind, watch.Repo, path, watch.Number))
		if private {
			lines = append(lines, "私有仓库：本通知展示标题和变更摘要，评论正文保留在本机。")
		}
	}
	return eventNotice{Title: title, Body: strings.Join(lines, "\n"), Subject: subject}, nil
}

// Summaries report observed facts, not importance, authorization or recommended
// actions. Private excerpts are never copied, even when metadata is enabled.
func describeChanges(changes []Change, kind string, hideMetadata, excerpts bool) []noticeChange {
	out := []noticeChange{}
	add := func(priority int, label, text string) {
		out = append(out, noticeChange{priority, label, text})
	}
	baseline, missing := false, 0
	baselineState := ""
	for _, c := range changes {
		if c.Kind == "baseline" {
			baseline = true
			if c.Object == "pull" || (c.Object == "issue" && kind != "PR") {
				a := obj(c.After)
				baselineState = map[string]string{"open": "打开", "closed": "关闭"}[str(a["state"])]
				if a["merged"] == true {
					baselineState = "已合并"
				} else if a["draft"] == true {
					baselineState = "草稿"
				}
			}
			continue
		}
		if c.Kind == "not_observed" {
			missing++
			continue
		}
		a, b := obj(c.After), obj(c.Before)
		changed := func(key string) bool { return !reflect.DeepEqual(a[key], b[key]) }
		category := strings.SplitN(c.Object, ":", 2)[0]
		switch category {
		case "comment", "review_comment", "review":
			label := "新增评论"
			if category == "review_comment" {
				label = "新增代码评论"
			}
			if c.Kind == "changed" {
				label = "评论已更新"
			}
			if category == "review" {
				label = map[string]string{"APPROVED": "评审通过", "CHANGES_REQUESTED": "评审要求修改", "COMMENTED": "收到评审意见", "DISMISSED": "评审已撤销", "PENDING": "评审尚未提交"}[strings.ToUpper(str(a["state"]))]
				if label == "" {
					label = "评审已更新"
				}
				if c.Kind == "changed" && !changed("state") {
					label = "评审内容已更新"
				}
			}
			text := label
			if !hideMetadata {
				if actor := noticeText(str(a["actor"]), 60); actor != "" {
					text += "（" + actor + "）"
				}
			}
			if excerpts {
				if body := noticeText(str(a["body"]), 150); body != "" {
					text += "：「" + body + "」"
				}
			}
			if c.Kind == "backfill" {
				text = "首次读取时补查的近期历史记录：" + text
				label = "近期历史记录"
			}
			add(2, label, text)
		case "check", "status":
			state := str(a["conclusion"])
			if state == "" {
				state = str(a["status"])
			}
			if category == "status" {
				state = str(a["state"])
			}
			label := map[string]string{"success": "检查通过", "failure": "检查失败", "error": "检查出错", "timed_out": "检查超时", "cancelled": "检查已取消", "skipped": "检查已跳过", "neutral": "检查结果为中性", "action_required": "检查需要操作", "stale": "检查结果已过期", "startup_failure": "检查启动失败", "pending": "检查等待中", "queued": "检查排队中", "in_progress": "检查运行中"}[state]
			if label == "" {
				label = "检查状态已更新"
			}
			text := label
			if !hideMetadata {
				name := str(a["name"])
				if name == "" {
					name = str(a["context"])
				}
				if name = noticeText(name, 100); name != "" {
					text += "：" + name
				}
			}
			priority := 3
			if state == "failure" || state == "error" || state == "timed_out" || state == "action_required" || state == "startup_failure" {
				priority = 1
			}
			add(priority, label, text)
		case "pull", "issue":
			start := len(out)
			if category == "pull" && a["merged"] == true && changed("merged") {
				add(0, "PR 已合并", "PR 已合并")
			} else if changed("state") && (category == "pull" || kind != "PR") {
				if state := str(a["state"]); state == "closed" {
					add(1, kind+" 已关闭", kind+" 已关闭")
				} else if state == "open" {
					label := kind + " 已打开"
					if b["state"] == "closed" {
						label = kind + " 已重新打开"
					}
					add(1, label, label)
				}
			}
			for _, f := range []struct{ key, label string }{{"head", "PR 有新提交"}, {"base_sha", "目标分支有新提交"}, {"title", "事项标题已更新"}, {"body", "事项说明已更新"}, {"labels", "标签已更新"}, {"assignees", "负责人已更新"}, {"requested_reviewers", "评审请求已更新"}, {"requested_teams", "评审请求已更新"}, {"draft", "草稿状态已更新"}, {"conflict_status", "合并冲突状态已更新"}} {
				if changed(f.key) {
					add(3, f.label, f.label)
				}
			}
			if len(out) == start && !(category == "issue" && kind == "PR" && changed("state")) {
				add(4, "事项信息已更新", "事项信息已更新")
			}
		case "timeline":
			label := map[string]string{"labeled": "标签已更新", "unlabeled": "标签已更新", "assigned": "负责人已更新", "unassigned": "负责人已更新", "review_requested": "评审请求已更新", "review_request_removed": "评审请求已更新", "cross-referenced": "关联事项已更新", "mentioned": "事项中出现提及", "renamed": "事项标题已更新", "locked": "讨论已锁定", "unlocked": "讨论已解锁"}[str(a["event"])]
			if label != "" {
				add(4, label, label)
			}
		default:
			add(4, "其他内容已更新", "其他内容已更新")
		}
	}
	if baseline {
		text := "首次读取当前状态"
		if baselineState != "" {
			text += "：" + kind + "（" + baselineState + "）"
		}
		add(5, "首次关注", text+"。这是背景信息，不代表刚发生了新活动。")
	}
	if missing > 0 {
		add(4, "部分记录本次未获取", fmt.Sprintf("本次未获取到 %d 项此前记录；不能据此判断它们已删除。", missing))
	}
	if len(out) == 0 {
		add(4, "内容已更新", "检测到内容变化，完整记录见本机上下文。")
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].priority < out[j].priority })
	seen := map[string]bool{}
	unique := out[:0]
	for _, change := range out {
		if !seen[change.text] {
			seen[change.text] = true
			unique = append(unique, change)
		}
	}
	return unique
}
