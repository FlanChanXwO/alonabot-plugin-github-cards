package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/AlonaBot/AlonaBot/sdk"
	"github.com/google/go-github/v93/github"
)

type hookObject struct {
	Number  int
	Title   string
	Name    string
	TagName string `json:"tag_name"`
	HTMLURL string `json:"html_url"`
	Merged  bool
}
type hookPayload struct {
	Repository                                          *github.Repository
	Sender                                              *github.User
	Action                                              string
	Ref                                                 string
	RefType                                             string `json:"ref_type"`
	Compare                                             string
	Issue, Discussion, Comment, Review, Release, Forkee *hookObject
	PR                                                  *hookObject `json:"pull_request"`
}

func (s *cards) hook(w http.ResponseWriter, r *http.Request) {
	if !s.webhook {
		http.NotFound(w, r)
		return
	}
	if !strings.HasPrefix(r.Header.Get(github.SHA256SignatureHeader), "sha256=") {
		http.Error(w, "signature required", http.StatusUnauthorized)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 25*1024*1024)
	body, err := github.ValidatePayload(r, []byte(s.secret))
	if err != nil {
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}
	kind := github.WebHookType(r)
	id := github.DeliveryID(r)
	if id == "" {
		http.Error(w, "missing GitHub delivery ID", http.StatusBadRequest)
		return
	}
	if kind != "ping" {
		if _, ok := parseEvents(kind); !ok {
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	event, err := github.ParseWebHook(kind, body)
	if err != nil {
		http.Error(w, "invalid GitHub event", http.StatusBadRequest)
		return
	}
	if _, ok := event.(*github.PingEvent); ok {
		slog.Info("GitHub webhook ping accepted", "delivery", id)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var payload hookPayload
	if err := json.Unmarshal(body, &payload); err != nil || payload.Repository.GetFullName() == "" {
		http.Error(w, "missing GitHub repository", http.StatusBadRequest)
		return
	}
	if payload.Repository.GetPrivate() {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	repo := strings.ToLower(payload.Repository.GetFullName())
	subs, err := s.subscribers(r.Context(), "", "", repo)
	if err != nil {
		slog.Error("read GitHub webhook subscriptions", "error", err)
		http.Error(w, "subscription lookup failed", http.StatusInternalServerError)
		return
	}
	if !allowsAction(kind, payload.Action) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := s.enqueueWebhook(r, subs, kind, id, payload); err != nil {
		slog.Error("queue GitHub webhook notifications", "delivery", id, "error", err)
		http.Error(w, "notification queue unavailable", http.StatusInternalServerError)
		return
	}
	slog.Info("GitHub webhook accepted", "delivery", id, "event", kind, "repository", repo)
	w.WriteHeader(http.StatusNoContent)
}

func allowsHook(f filter, kind, ref string) bool {
	if !f.allows(eventName(kind)) {
		return false
	}
	if f.branch == "" {
		return true
	}
	branch, ok := strings.CutPrefix(ref, "refs/heads/")
	if ok {
		return f.branch == branch
	}
	return kind != "push" && kind != "create"
}

func (p hookPayload) message(kind string) string {
	labels := map[string]string{
		"issues": "Issue", "issue_comment": "Issue 评论", "pull_request": "PR",
		"pull_request_review": "PR 审查", "pull_request_review_comment": "PR 审查评论",
		"pull_request_review_thread": "PR 审查讨论", "commit_comment": "提交评论", "discussion": "Discussion",
		"discussion_comment": "Discussion 评论", "fork": "Fork", "star": "Star", "create": "创建",
		"push": "Push", "release": "Release",
	}
	label := labels[kind]
	if label == "" {
		label = kind
	}
	action := p.Action
	if kind == "pull_request" && action == "closed" && p.PR != nil && p.PR.Merged {
		action = "merged"
	}
	text := "GitHub · " + p.Repository.GetFullName() + "\n" + strings.TrimSpace(label+" "+action)
	if who := p.Sender.GetLogin(); who != "" {
		text += " · @" + who
	}
	for _, object := range []*hookObject{p.Issue, p.PR, p.Discussion, p.Release, p.Forkee} {
		if object == nil {
			continue
		}
		var title []string
		if object.Number != 0 {
			title = append(title, fmt.Sprintf("#%d", object.Number))
		}
		for _, part := range []string{object.Title, object.TagName, object.Name} {
			if part != "" {
				title = append(title, part)
			}
		}
		if len(title) > 0 {
			text += "\n" + strings.Join(title, " ")
		}
		if object.HTMLURL != "" {
			text += "\n" + object.HTMLURL
		}
	}
	for _, object := range []*hookObject{p.Comment, p.Review} {
		if object != nil && object.HTMLURL != "" {
			text += "\n" + object.HTMLURL
		}
	}
	if p.Ref != "" {
		text += "\n" + strings.TrimSpace(p.RefType+" "+p.Ref)
	}
	if p.Compare != "" {
		text += "\n" + p.Compare
	}
	return text
}

func allowsAction(kind, action string) bool {
	var allowed []string
	switch kind {
	case "issues", "pull_request":
		allowed = []string{"opened", "closed", "reopened"}
	case "issue_comment", "discussion_comment", "pull_request_review_comment":
		allowed = []string{"created", "edited", "deleted"}
	case "commit_comment":
		allowed = []string{"", "created"}
	case "discussion":
		allowed = []string{"created", "edited", "answered", "unanswered", "labeled", "unlabeled"}
	case "pull_request_review":
		allowed = []string{"submitted", "edited", "dismissed"}
	case "pull_request_review_thread":
		allowed = []string{"created", "resolved", "unresolved"}
	case "star":
		allowed = []string{"created", "deleted"}
	case "release":
		allowed = []string{"published", "created", "edited", "deleted", "prereleased", "released"}
	default:
		return true
	}
	return slices.Contains(allowed, action)
}

func (s *cards) enqueueWebhook(r *http.Request, subs []subscriber, kind, delivery string, p hookPayload) error {
	ctx := r.Context()
	// 投递号用于查日志；去重键使用已验证的正文签名。
	id := "webhook:" + kind + ":" + strings.ToLower(r.Header.Get(github.SHA256SignatureHeader))
	text := p.message(kind)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, sub := range subs {
		if !allowsHook(sub.filter, kind, p.Ref) {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO webhook_queue(instance,session,id,delivery,text)
   VALUES(?,?,?,?,?) ON CONFLICT(instance,session,id) DO UPDATE SET failed=0`,
			sub.instance, sub.session, id, delivery, text); err != nil {
			return err
		}
	}
	return tx.Commit()
}

type queuedWebhook struct {
	sub                subscriber
	id, delivery, text string
}

func (s *cards) pendingWebhook(ctx context.Context) ([]queuedWebhook, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT instance,session,id,delivery,text FROM webhook_queue WHERE failed=0 ORDER BY rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []queuedWebhook
	for rows.Next() {
		var item queuedWebhook
		if err := rows.Scan(&item.sub.instance, &item.sub.session, &item.id, &item.delivery, &item.text); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *cards) deliverWebhook(c *sdk.Context) error {
	if !s.webhook {
		return nil
	}
	if !s.polls.TryLock() {
		return nil
	}
	defer s.polls.Unlock()
	if !s.modeRecorded {
		if err := s.recordMode(c); err != nil {
			return err
		}
		s.modeRecorded = true
	}
	items, err := s.pendingWebhook(c)
	if err != nil {
		return err
	}
	var problems []error
	for _, item := range items {
		if c.Err() != nil {
			break
		}
		err := s.notify(c, item.sub, item.id, item.text)
		ctx := context.WithoutCancel(c)
		if err != nil {
			c.Logger().Error("send GitHub webhook notification", "delivery", item.delivery, "error", err)
			_, err = s.db.ExecContext(ctx, "UPDATE webhook_queue SET failed=1 WHERE instance=? AND session=? AND id=?",
				item.sub.instance, item.sub.session, item.id)
		} else {
			_, err = s.db.ExecContext(ctx, "DELETE FROM webhook_queue WHERE instance=? AND session=? AND id=?",
				item.sub.instance, item.sub.session, item.id)
		}
		if err != nil {
			problems = append(problems, err)
		}
	}
	return errors.Join(problems...)
}
