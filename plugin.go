package plugin

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/AlonaBot/AlonaBot/sdk"
	"github.com/AlonaBot/AlonaBot/sdk/sqlitedb"
	"github.com/google/go-github/v93/github"
)

type cards struct {
	db           *sql.DB
	api          *github.Client
	auto         bool
	secret       string
	webhook      bool
	interval     time.Duration
	nextPoll     time.Time
	modeRecorded bool
	polls        sync.Mutex
}

func New() *sdk.Plugin { return newPlugin(&cards{}) }

func newPlugin(s *cards, options ...github.ClientOptionsFunc) *sdk.Plugin {
	p := sdk.NewPlugin()
	sqlitedb.Migrations(p, []sqlitedb.Migration{{ID: 1, Up: migrate}})
	p.OnStart(func(ctx context.Context) error {
		cfg := p.Config()
		s.auto = cfg.Bool("auto_resolve_links")
		s.webhook, s.secret = cfg.Bool("enable_webhook"), cfg.String("webhook_secret")
		if s.webhook && s.secret == "" {
			return fmt.Errorf("webhook_secret is required when enable_webhook is true")
		}
		minutes := cfg.Int("check_interval")
		if minutes < 1 || minutes > int64((1<<63-1)/time.Minute) {
			return fmt.Errorf("check_interval is outside the supported range")
		}
		s.interval = time.Duration(minutes) * time.Minute
		s.nextPoll = time.Now().Add(s.interval - time.Minute)
		clientOptions := slices.Clone(options)
		if token := cfg.String("github_token"); token != "" {
			clientOptions = append(clientOptions, github.WithAuthToken(token))
		}
		var err error
		s.api, err = github.NewClient(clientOptions...)
		if err != nil {
			return err
		}
		s.db, err = sqlitedb.Open(ctx)
		return err
	})
	// 链接卡片旁路发送；其他插件的 Raw 路由匹配后仍应识别链接。
	p.On(sdk.MessageCreated).Filter(sdk.MessageFilter{WakeWordsConfig: "github_link_trigger"}).Handle(s.card)
	p.Command("ghlink").Description("Turn automatic GitHub cards on or off").Handle(s.link)
	p.Command("ghdefault").Alias("ghdef").Description("Set or show the default repository").Handle(s.defaultRepo)
	p.Command("ghissue").Alias("ghis").Description("Look up a GitHub Issue").Handle(s.issue)
	p.Command("ghpr").Description("Look up a GitHub pull request").Handle(s.pr)
	p.Command("ghreadme").Description("Read a repository README").Handle(s.readme)
	p.Command("ghlimit").Alias("ghrate").Description("Show GitHub API rate limits").Handle(s.limit)
	p.Command("ghsub").Description("Subscribe to GitHub updates").Handle(s.subscribe)
	p.Command("ghunsub").Description("Unsubscribe from GitHub updates").Handle(s.unsubscribe)
	p.Command("ghlist").Description("List GitHub subscriptions").Handle(s.list)
	p.Route(http.MethodPost, "/github/webhook").Public(sdk.AuthCallbackSignature).HandleFunc(s.hook)
	p.Every("github-poll", time.Minute).Handle(s.poll)
	p.Every("github-webhook-delivery", 2*time.Second).Handle(s.deliverWebhook)
	return p
}

func migrate(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `
CREATE TABLE chat_settings (
 instance TEXT NOT NULL, session TEXT NOT NULL, auto INTEGER, repo TEXT,
 PRIMARY KEY (instance, session)
);
CREATE TABLE subscriptions (
 instance TEXT NOT NULL, session TEXT NOT NULL, repo TEXT NOT NULL,
 branch TEXT NOT NULL, events TEXT NOT NULL, since INTEGER NOT NULL,
 PRIMARY KEY (instance, session, repo, branch, events)
);
CREATE TABLE deliveries (
 instance TEXT NOT NULL, session TEXT NOT NULL, id TEXT NOT NULL,
 PRIMARY KEY (instance, session, id)
);
CREATE TABLE webhook_queue (
 instance TEXT NOT NULL, session TEXT NOT NULL, id TEXT NOT NULL,
 delivery TEXT NOT NULL, text TEXT NOT NULL, failed INTEGER NOT NULL DEFAULT 0,
 PRIMARY KEY (instance, session, id)
);
CREATE TABLE notification_mode (
 id INTEGER PRIMARY KEY CHECK (id=1), webhook INTEGER NOT NULL
);`)
	return err
}

func (s *cards) recordMode(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if !s.webhook {
		if _, err := tx.ExecContext(ctx, `UPDATE subscriptions SET since=?
   WHERE EXISTS(SELECT 1 FROM notification_mode WHERE webhook=1)`, time.Now().Unix()+1); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO notification_mode(id,webhook) VALUES(1,?)
  ON CONFLICT(id) DO UPDATE SET webhook=excluded.webhook`, s.webhook); err != nil {
		return err
	}
	return tx.Commit()
}
