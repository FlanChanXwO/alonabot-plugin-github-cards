package plugin

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/AlonaBot/AlonaBot/sdk"
	"github.com/google/go-github/v93/github"
)

type update struct {
	kind, id, text string
	at             int64
}
type window struct {
	since  int64
	needed map[string]bool
}

func (s *cards) poll(c *sdk.Context) error {
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
	if s.webhook {
		return nil
	}
	now := time.Now()
	if now.Before(s.nextPoll) {
		return nil
	}
	s.nextPoll = now.Add(s.interval - time.Second)
	return s.pollUpdates(c, now.Unix())
}

func (s *cards) pollUpdates(ctx context.Context, cutoff int64) error {
	subs, err := s.subscribers(ctx, "", "", "")
	if err != nil {
		return err
	}
	var problems []error
	// 仓库级事件按仓库查询，提交按分支查询；会话各自推进游标。
	for len(subs) > 0 {
		end := 1
		for end < len(subs) && subs[end].repo == subs[0].repo {
			end++
		}
		group := subs[:end]
		subs = subs[end:]
		if err := s.pollRepo(ctx, group, cutoff); err != nil {
			problems = append(problems, fmt.Errorf("%s: %w", group[0].repo, err))
		}
	}
	return errors.Join(problems...)
}

func (s *cards) pollRepo(ctx context.Context, group []subscriber, cutoff int64) error {
	repo := group[0].repo
	if _, err := publicRepo(ctx, s.api, repo); err != nil {
		return err
	}
	owner, name, _ := strings.Cut(repo, "/")
	base := window{since: cutoff, needed: map[string]bool{}}
	branches := map[string]int64{}
	for _, sub := range group {
		if sub.since > cutoff {
			continue
		}
		base.since = min(base.since, sub.since)
		for _, kind := range []string{"issues", "prs", "releases"} {
			base.needed[kind] = base.needed[kind] || sub.allows(kind)
		}
		if sub.allows("commits") {
			if since, ok := branches[sub.branch]; !ok || sub.since < since {
				branches[sub.branch] = sub.since
			}
		}
	}
	var repoItems []update
	var issueErr, releaseErr error
	if base.needed["issues"] || base.needed["prs"] {
		repoItems, issueErr = s.issuesSince(ctx, owner, name, base.since)
	}
	if base.needed["releases"] {
		releases, err := s.releasesSince(ctx, owner, name, base.since)
		releaseErr = err
		repoItems = append(repoItems, releases...)
	}
	type branchResult struct {
		updates []update
		err     error
	}
	commits := map[string]branchResult{}
	for branch, since := range branches {
		items, err := s.commitsSince(ctx, owner, name, branch, since)
		commits[branch] = branchResult{items, err}
	}
	var errs []error
	for _, sub := range group {
		if sub.since > cutoff {
			continue
		}
		var prior error
		if sub.allows("issues") || sub.allows("prs") {
			prior = errors.Join(prior, issueErr)
		}
		if sub.allows("releases") {
			prior = errors.Join(prior, releaseErr)
		}
		items := slices.Clone(repoItems)
		if sub.allows("commits") {
			result := commits[sub.branch]
			prior = errors.Join(prior, result.err)
			items = append(items, result.updates...)
		}
		slices.SortStableFunc(items, func(a, b update) int {
			if a.at < b.at {
				return -1
			}
			if a.at > b.at {
				return 1
			}
			return 0
		})
		for _, item := range items {
			if item.at < sub.since || item.at > cutoff || !sub.allows(item.kind) {
				continue
			}
			if err := s.notify(ctx, sub, "poll:"+repo+":"+item.id, item.text); err != nil {
				prior = errors.Join(prior, err)
			}
		}
		if err := s.advance(ctx, sub, cutoff, prior); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (s *cards) issuesSince(ctx context.Context, owner, repo string, since int64) ([]update, error) {
	opts := &github.IssueListByRepoOptions{State: "all", Sort: "created", Direction: "desc", Since: time.Unix(since-1, 0).UTC(), ListOptions: github.ListOptions{PerPage: 100}}
	var out []update
	for {
		items, resp, err := s.api.Issues.ListByRepo(ctx, owner, repo, opts)
		if err != nil {
			return out, err
		}
		for _, item := range items {
			at := item.GetCreatedAt().Unix()
			if at < since {
				return out, nil
			}
			kind, label := "issues", "Issue"
			if item.IsPullRequest() {
				kind, label = "prs", "PR"
			}
			out = append(out, update{kind, fmt.Sprintf("%s:%d", kind, item.GetNumber()),
				fmt.Sprintf("GitHub · %s/%s\n新 %s #%d：%s\n%s", owner, repo, label, item.GetNumber(), item.GetTitle(), item.GetHTMLURL()), at})
		}
		if resp.NextPage == 0 {
			return out, nil
		}
		opts.ListOptions.Page = resp.NextPage
	}
}

func (s *cards) commitsSince(ctx context.Context, owner, repo, branch string, since int64) ([]update, error) {
	opts := &github.CommitsListOptions{SHA: branch, Since: time.Unix(since-1, 0).UTC(), ListOptions: github.ListOptions{PerPage: 100}}
	var out []update
	for {
		items, resp, err := s.api.Repositories.ListCommits(ctx, owner, repo, opts)
		if resp != nil && resp.StatusCode == 409 {
			return out, nil
		} // GitHub 对空仓库没有提交可列出。
		if err != nil {
			return out, err
		}
		for _, item := range items {
			commit := item.GetCommit()
			at := commit.GetCommitter().GetDate().Unix()
			title, _, _ := strings.Cut(commit.GetMessage(), "\n")
			out = append(out, update{"commits", "commit:" + item.GetSHA(),
				fmt.Sprintf("GitHub · %s/%s\n新提交：%s\n%s", owner, repo, title, item.GetHTMLURL()), at})
		}
		if resp.NextPage == 0 {
			return out, nil
		}
		opts.ListOptions.Page = resp.NextPage
	}
}

func (s *cards) releasesSince(ctx context.Context, owner, repo string, since int64) ([]update, error) {
	opts := &github.ListOptions{PerPage: 100}
	var out []update
	for {
		items, resp, err := s.api.Repositories.ListReleases(ctx, owner, repo, opts)
		if err != nil {
			return out, err
		}
		for _, item := range items {
			at := item.GetPublishedAt().Unix()
			if item.GetDraft() || at < since {
				continue
			}
			out = append(out, update{"releases", fmt.Sprintf("release:%d", item.GetID()),
				fmt.Sprintf("GitHub · %s/%s\n新 Release：%s %s\n%s", owner, repo, item.GetTagName(), item.GetName(), item.GetHTMLURL()), at})
		}
		if resp.NextPage == 0 {
			return out, nil
		}
		opts.Page = resp.NextPage
	}
}
