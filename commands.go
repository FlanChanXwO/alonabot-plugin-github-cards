package plugin

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/AlonaBot/AlonaBot/sdk"
	"github.com/AlonaBot/AlonaBot/sdk/media"
	"github.com/AlonaBot/AlonaBot/sdk/message"
	"github.com/google/go-github/v93/github"
)

var (
	repoName   = regexp.MustCompile(`^[A-Za-z0-9-]+/[A-Za-z0-9_.-]+$`)
	githubLink = regexp.MustCompile(`https://github\.com/([A-Za-z0-9-]+/[A-Za-z0-9_.-]+(?:/(?:issues|pull)/[0-9]+)?)`)
	itemRef    = regexp.MustCompile(`^([A-Za-z0-9-]+/[A-Za-z0-9_.-]+)[#/\s]+([0-9]+)$`)
)

func cardImage(ref string) message.Segment {
	var nonce [16]byte
	rand.Read(nonce[:])
	return message.ImageURL("https://opengraph.githubassets.com/" + hex.EncodeToString(nonce[:]) + "/" + strings.ReplaceAll(url.PathEscape(ref), "%2F", "/"))
}

func (s *cards) card(c *sdk.Context, e *message.CreatedPayload) error {
	ref := githubLink.FindStringSubmatch(e.Chain.Text())
	if ref == nil {
		return nil
	}
	var enabled sql.NullBool
	err := s.db.QueryRowContext(c, "SELECT auto FROM chat_settings WHERE instance=? AND session=?", c.Instance(), c.Session()).Scan(&enabled)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if (enabled.Valid && !enabled.Bool) || (!enabled.Valid && !s.auto) {
		return nil
	}
	// 自动卡片是独立消息，避免附加平台的回复提醒、引用和前缀。
	_, err = sdk.Send(c, c.Session(), cardImage(strings.TrimRight(ref[1], ".")), sdk.Via(c.Instance()))
	return err
}

type rawArgs struct {
	Value *string `arg:"value,raw"`
}

func (a rawArgs) text() string {
	if a.Value == nil {
		return ""
	}
	return strings.TrimSpace(*a.Value)
}

func (s *cards) link(c *sdk.Context, args rawArgs) error {
	v := strings.ToLower(args.text())
	if v != "on" && v != "off" {
		return c.Reply("用法：/ghlink on/off")
	}
	_, err := s.db.ExecContext(c, `INSERT INTO chat_settings(instance,session,auto) VALUES(?,?,?)
 ON CONFLICT(instance,session) DO UPDATE SET auto=excluded.auto`, c.Instance(), c.Session(), v == "on")
	if err != nil {
		return err
	}
	return c.Reply("GitHub 链接卡片：" + v)
}

func publicRepo(ctx context.Context, api *github.Client, name string) (string, error) {
	if !repoName.MatchString(name) {
		return "", fmt.Errorf("invalid repository: %q", name)
	}
	owner, repo, _ := strings.Cut(name, "/")
	data, _, err := api.Repositories.Get(ctx, owner, repo)
	if err != nil {
		return "", err
	}
	if data.GetPrivate() {
		return "", fmt.Errorf("private repository access is disabled in chat")
	}
	return strings.ToLower(data.GetFullName()), nil
}

func (s *cards) defaultRepo(c *sdk.Context, args rawArgs) error {
	name := args.text()
	if name == "" {
		var repo sql.NullString
		err := s.db.QueryRowContext(c, "SELECT repo FROM chat_settings WHERE instance=? AND session=?", c.Instance(), c.Session()).Scan(&repo)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if !repo.Valid {
			return c.Reply("当前会话未设置默认仓库")
		}
		return c.Reply("默认仓库：" + repo.String)
	}
	name, err := publicRepo(c, s.api, name)
	if err != nil {
		return githubFailure(c, err)
	}
	if err := setDefault(c, s.db, c.Instance(), c.Session(), name); err != nil {
		return err
	}
	return c.Reply("默认仓库：" + name)
}

type execDB interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func setDefault(ctx context.Context, db execDB, instance, session, repo string) error {
	_, err := db.ExecContext(ctx, `INSERT INTO chat_settings(instance,session,repo) VALUES(?,?,?)
 ON CONFLICT(instance,session) DO UPDATE SET repo=excluded.repo`, instance, session, repo)
	return err
}

func (s *cards) reference(c *sdk.Context, ref string) (owner, repo string, n int, err error) {
	var name string
	if m := itemRef.FindStringSubmatch(ref); m != nil {
		name, ref = m[1], m[2]
	} else if n, err = strconv.Atoi(ref); err == nil && n > 0 {
		err = s.db.QueryRowContext(c, `SELECT COALESCE(
   (SELECT repo FROM chat_settings WHERE instance=?1 AND session=?2),
   (SELECT MIN(repo) FROM subscriptions WHERE instance=?1 AND session=?2 HAVING COUNT(DISTINCT repo)=1),'')`, c.Instance(), c.Session()).Scan(&name)
		if err != nil {
			return "", "", 0, err
		}
	}
	n, err = strconv.Atoi(ref)
	if n < 1 || err != nil || name == "" {
		return "", "", 0, fmt.Errorf("specify owner/repo#number or set a default repository first")
	}
	name, err = publicRepo(c, s.api, name)
	owner, repo, _ = strings.Cut(name, "/")
	return owner, repo, n, err
}

func githubFailure(c *sdk.Context, err error) error {
	c.Logger().Warn("GitHub request failed", "error", err)
	return c.Reply("GitHub 请求失败：" + err.Error())
}

func (s *cards) issue(c *sdk.Context, args rawArgs) error {
	o, r, n, err := s.reference(c, args.text())
	if err != nil {
		return githubFailure(c, err)
	}
	issue, _, err := s.api.Issues.Get(c, o, r, n)
	if err != nil {
		return githubFailure(c, err)
	}
	text := fmt.Sprintf("Issue #%d · %s\n状态：%s · @%s · 评论：%d\n%s\n%s", n, issue.GetTitle(), issue.GetState(), issue.GetUser().GetLogin(), issue.GetComments(), issue.GetHTMLURL(), issue.GetBody())
	return c.Reply(text, cardImage(fmt.Sprintf("%s/%s/issues/%d", o, r, n)))
}

func (s *cards) pr(c *sdk.Context, args rawArgs) error {
	o, r, n, err := s.reference(c, args.text())
	if err != nil {
		return githubFailure(c, err)
	}
	pr, _, err := s.api.PullRequests.Get(c, o, r, n)
	if err != nil {
		return githubFailure(c, err)
	}
	state := pr.GetState()
	if pr.GetMerged() {
		state = "merged"
	}
	text := fmt.Sprintf("PR #%d · %s\n状态：%s · @%s\n%s → %s · +%d −%d\n%s\n%s", n, pr.GetTitle(), state, pr.GetUser().GetLogin(), pr.GetHead().GetRef(), pr.GetBase().GetRef(), pr.GetAdditions(), pr.GetDeletions(), pr.GetHTMLURL(), pr.GetBody())
	return c.Reply(text, cardImage(fmt.Sprintf("%s/%s/pull/%d", o, r, n)))
}

func (s *cards) readme(c *sdk.Context, args rawArgs) error {
	name := strings.FieldsFunc(args.text(), func(r rune) bool { return r == '#' || r == ' ' || r == '\t' })
	if len(name) == 0 {
		return c.Reply("用法：/ghreadme owner/repo")
	}
	repo, err := publicRepo(c, s.api, name[0])
	if err != nil {
		return githubFailure(c, err)
	}
	o, r, _ := strings.Cut(repo, "/")
	content, _, err := s.api.Repositories.GetReadme(c, o, r, nil)
	if err != nil {
		return githubFailure(c, err)
	}
	body, err := content.GetContent()
	if err != nil {
		return githubFailure(c, err)
	}
	full := repo + " README\n\n" + body
	var images message.Chain
	pages, err := renderReadme(full)
	if err == nil {
		for _, page := range pages {
			var image message.Segment
			image, err = media.Image(c, page)
			if err != nil {
				break
			}
			images = append(images, image)
		}
	}
	if err != nil {
		c.Logger().Warn("render README image", "error", err)
		return c.Reply(full)
	}
	_, err = c.Send(images)
	return err
}

func (s *cards) limit(c *sdk.Context) error {
	limits, _, err := s.api.RateLimit.Get(c)
	if err != nil {
		return githubFailure(c, err)
	}
	var lines []string
	for _, item := range []struct {
		name string
		rate *github.Rate
	}{{"Core", limits.Core}, {"Search", limits.Search}, {"GraphQL", limits.GraphQL}} {
		if item.rate != nil {
			lines = append(lines, fmt.Sprintf("%s：%d/%d · %s 重置", item.name, item.rate.Remaining, item.rate.Limit, item.rate.Reset.UTC().Format(time.RFC3339)))
		}
	}
	return c.Reply(strings.Join(lines, "\n"))
}
