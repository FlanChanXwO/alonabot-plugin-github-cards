package plugin

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/AlonaBot/AlonaBot/sdk"
	"github.com/AlonaBot/AlonaBot/sdk/message"
)

type filter struct{ repo, branch, events string }

type subscriber struct {
	instance, session string
	filter
	since int64
}

var eventAliases = map[string]string{
	"issue": "issues", "pr": "prs", "pulls": "prs", "pull_request": "prs",
	"commit": "commits", "push": "commits", "release": "releases",
}
var eventNames = []string{
	"issues", "prs", "commits", "releases", "issue_comment", "pull_request_review",
	"pull_request_review_comment", "pull_request_review_thread", "commit_comment",
	"discussion", "discussion_comment", "fork", "star", "create",
}

func eventName(event string) string {
	if name := eventAliases[event]; name != "" {
		return name
	}
	return event
}

func parseEvents(input string) (string, bool) {
	var names []string
	for _, item := range strings.Split(strings.ToLower(input), ",") {
		name := eventName(strings.TrimSpace(item))
		if !slices.Contains(eventNames, name) {
			return "", false
		}
		names = append(names, name)
	}
	slices.Sort(names)
	return strings.Join(slices.Compact(names), ","), true
}

func parseFilter(input string) (filter, error) {
	parts := strings.Fields(input)
	invalid := fmt.Errorf("用法：owner/repo [分支] [事件,事件]")
	if len(parts) < 1 || len(parts) > 3 {
		return filter{}, invalid
	}
	repoParts := strings.SplitN(parts[0], "/", 3)
	if len(repoParts) < 2 {
		return filter{}, invalid
	}
	f := filter{repo: strings.ToLower(strings.Join(repoParts[:2], "/"))}
	if !repoName.MatchString(f.repo) {
		return filter{}, invalid
	}
	if len(repoParts) == 3 {
		if len(parts) == 3 || repoParts[2] == "" {
			return filter{}, invalid
		}
		f.branch = repoParts[2]
	}
	switch {
	case len(parts) == 3:
		f.branch = parts[1]
		var ok bool
		if f.events, ok = parseEvents(parts[2]); !ok {
			return filter{}, invalid
		}
	case len(parts) == 2:
		if events, ok := parseEvents(parts[1]); ok {
			f.events = events
		} else if f.branch == "" && !strings.Contains(parts[1], ",") {
			f.branch = parts[1]
		} else {
			return filter{}, invalid
		}
	}
	return f, nil
}

func (f filter) allows(kind string) bool {
	return f.events == "" || slices.Contains(strings.Split(f.events, ","), kind)
}
func (f filter) label() string {
	text := f.repo
	if f.branch != "" {
		text += " · " + f.branch
	}
	if f.events != "" {
		text += " · " + f.events
	}
	return text
}

func (s *cards) subscribe(c *sdk.Context, args rawArgs) error {
	f, err := parseFilter(args.text())
	if err != nil {
		return c.Reply(err.Error())
	}
	f.repo, err = publicRepo(c, s.api, f.repo)
	if err != nil {
		return githubFailure(c, err)
	}
	tx, err := s.db.BeginTx(c, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(c, `INSERT INTO subscriptions(instance,session,repo,branch,events,since)
 VALUES(?,?,?,?,?,?) ON CONFLICT DO NOTHING`, c.Instance(), c.Session(), f.repo, f.branch, f.events, time.Now().Unix()+1)
	if err != nil {
		return err
	}
	if err = setDefault(c, tx, c.Instance(), c.Session(), f.repo); err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if changed == 0 {
		return c.Reply("已订阅：" + f.label())
	}
	return c.Reply("订阅成功：" + s.subscriptionLabel(f))
}

func (s *cards) unsubscribe(c *sdk.Context, args rawArgs) error {
	conditions := "instance=? AND session=?"
	values := []any{c.Instance(), c.Session()}
	if input := args.text(); input != "" {
		f, err := parseFilter(input)
		if err != nil {
			return c.Reply(err.Error())
		}
		conditions += " AND repo=? AND branch=? AND events=?"
		values = append(values, f.repo, f.branch, f.events)
	}
	res, err := s.db.ExecContext(c, "DELETE FROM subscriptions WHERE "+conditions, values...)
	if err != nil {
		return err
	}
	count, err := res.RowsAffected()
	if err != nil {
		return err
	}
	return c.Reply(fmt.Sprintf("已取消 %d 条订阅", count))
}

func (s *cards) subscribers(ctx context.Context, instance, session, repo string) ([]subscriber, error) {
	var where string
	var args []any
	switch {
	case instance != "":
		where = " WHERE instance=? AND session=?"
		args = []any{instance, session}
	case repo != "":
		where = " WHERE repo=?"
		args = []any{repo}
	}
	rows, err := s.db.QueryContext(ctx, "SELECT instance,session,repo,branch,events,since FROM subscriptions"+where+
		" ORDER BY repo,branch,events,instance,session", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []subscriber
	for rows.Next() {
		var sub subscriber
		if err := rows.Scan(&sub.instance, &sub.session, &sub.repo, &sub.branch, &sub.events, &sub.since); err != nil {
			return nil, err
		}
		out = append(out, sub)
	}
	return out, rows.Err()
}

func (s *cards) list(c *sdk.Context) error {
	subs, err := s.subscribers(c, c.Instance(), c.Session(), "")
	if err != nil {
		return err
	}
	if len(subs) == 0 {
		return c.Reply("当前会话没有 GitHub 订阅")
	}
	var lines []string
	for _, sub := range subs {
		lines = append(lines, s.subscriptionLabel(sub.filter))
	}
	return c.Reply("GitHub 订阅：\n" + strings.Join(lines, "\n"))
}

func (s *cards) notify(ctx context.Context, sub subscriber, id, text string) error {

	var sent bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM deliveries
   WHERE instance=? AND session=? AND id=?)`, sub.instance, sub.session, id).Scan(&sent); err != nil {
		return err
	}
	if sent {
		return nil
	}
	res, err := sdk.Send(ctx, sub.session, message.Text(text), sdk.Via(sub.instance))
	if err != nil && (res == nil || res.Ref() == "") {
		return err
	}
	_, err = s.db.ExecContext(context.WithoutCancel(ctx), "INSERT INTO deliveries(instance,session,id) VALUES(?,?,?)", sub.instance, sub.session, id)
	return err
}

func (s *cards) advance(ctx context.Context, sub subscriber, cutoff int64, prior error) error {
	if prior != nil {
		return prior
	}
	_, err := s.db.ExecContext(ctx, `UPDATE subscriptions SET since=?
  WHERE instance=? AND session=? AND repo=? AND branch=? AND events=? AND since=?`,
		cutoff, sub.instance, sub.session, sub.repo, sub.branch, sub.events, sub.since)
	if err != nil {
		return err
	}
	return nil
}

func (s *cards) subscriptionLabel(f filter) string {
	label := f.label()
	if s.webhook || f.events == "" {
		return label
	}
	var pending []string
	for _, kind := range strings.Split(f.events, ",") {
		if !slices.Contains([]string{"issues", "prs", "commits", "releases"}, kind) {
			pending = append(pending, kind)
		}
	}
	if len(pending) > 0 {
		label += "（" + strings.Join(pending, ",") + " 需 Webhook）"
	}
	return label
}
