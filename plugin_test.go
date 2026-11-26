package plugin

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AlonaBot/AlonaBot/plugintest"
	"github.com/AlonaBot/AlonaBot/sdk"
	"github.com/AlonaBot/AlonaBot/sdk/message"
	"github.com/google/go-github/v93/github"
)

func githubFixture(t *testing.T) github.ClientOptionsFunc {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/org/repo":
			fmt.Fprint(w, `{"full_name":"org/repo"}`)
		case "/repos/org/repo/issues/7":
			fmt.Fprint(w, `{"title":"Example issue","state":"open","user":{"login":"alice"},"html_url":"https://github.com/org/repo/issues/7"}`)
		case "/repos/org/repo/readme":
			fmt.Fprint(w, `{"encoding":"base64","content":"IyBHaXRIdWIgUmVhZG1l"}`)
		case "/repos/org/repo/pulls/3":
			fmt.Fprint(w, `{"title":"Example PR","state":"open","base":{"ref":"main"},"head":{"ref":"test"}}`)
		case "/repos/org/repo/issues":
			fmt.Fprint(w, `[{"number":7,"title":"Example issue","created_at":"2020-01-02T00:00:00Z"},{"number":6,"title":"Old issue","created_at":"2019-12-01T00:00:00Z"}]`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	base := server.URL + "/"
	return github.WithURLs(&base, nil)
}

func TestCommandsAndCards(t *testing.T) {
	s := &cards{}
	p := newPlugin(s, githubFixture(t))
	p.Raw().ID("test.raw").Handle(func(*sdk.Context) error { return nil })
	h := plugintest.Start(t, plugintest.Plugin(p))
	alice, bob := plugintest.User{ID: "alice"}, plugintest.User{ID: "bob"}
	a, b := plugintest.Private(alice.ID), plugintest.Private(bob.ID)
	reply := func(chat plugintest.Chat, user plugintest.User, input, want string) {
		t.Helper()
		got := h.Text(chat, user, input)
		if len(got.Sends) != 1 || !strings.Contains(got.Sends[0].Text(), want) {
			t.Fatalf("%s: %+v, want %q", input, got, want)
		}
	}
	reply(a, alice, "/ghsub org/repo main issues,issue_comment", "订阅成功")
	reply(a, alice, "/ghlist", "org/repo · main · issue_comment,issues")
	reply(b, bob, "/ghlist", "没有 GitHub 订阅")
	reply(a, alice, "/ghdef", "org/repo")
	reply(a, alice, "/ghis 7", "Example issue")
	reply(a, alice, "/ghpr org/repo 3", "Example PR")
	readme := h.Text(a, alice, "/ghreadme org/repo")
	if len(readme.Sends) != 1 || len(readme.Sends[0].Chain) != 1 {
		t.Fatalf("README response: %+v", readme)
	}
	if _, ok := readme.Sends[0].Chain[0].(message.ImageSegment); !ok {
		t.Fatalf("README did not send a PNG: %+v", readme.Sends[0])
	}
	reply(a, alice, "/ghlink off", "off")
	ordinary := h.Text(b, bob, "普通聊天消息")
	if len(ordinary.Outcomes) != 1 || ordinary.Outcomes[0].RouteID != "test.raw" {
		t.Fatalf("ordinary message went through the link route: %+v", ordinary.Outcomes)
	}
	link := "https://github.com/org/repo/pull/3"
	if got := h.Text(a, alice, link); len(got.Sends) != 0 {
		t.Fatalf("card after /ghlink off: %+v", got.Sends)
	}
	got := h.Text(b, bob, link)
	if len(got.Sends) != 1 {
		t.Fatalf("card: %+v", got)
	}
	seg, ok := got.Sends[0].Chain[0].(message.ImageSegment)
	if !ok || !strings.HasSuffix(seg.Source.HTTPSURL, "/org/repo/pull/3") {
		t.Fatalf("card image: %+v", got.Sends[0])
	}
	reply(a, alice, "/ghunsub org/repo main issues,issue_comment", "已取消 1 条订阅")
}

func TestPollingUsesBaselineAndCursor(t *testing.T) {
	s := &cards{}
	p := newPlugin(s, githubFixture(t))
	var session context.Context
	p.OnReady(func(ctx context.Context) error { session = context.WithoutCancel(ctx); return nil })
	h := plugintest.Start(t, plugintest.Plugin(p))
	alice := plugintest.User{ID: "alice"}
	got := h.Text(plugintest.Private(alice.ID), alice, "/ghsub org/repo issues")
	if len(got.Sends) != 1 {
		t.Fatalf("subscribe: %+v", got)
	}
	ctx := t.Context()
	_, err := s.db.ExecContext(ctx, "UPDATE subscriptions SET since=?", time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC).Unix())
	if err != nil {
		t.Fatal(err)
	}
	if err = s.pollUpdates(session, time.Date(2020, 1, 3, 0, 0, 0, 0, time.UTC).Unix()); err != nil {
		t.Fatal(err)
	}
	if err = s.pollUpdates(session, time.Date(2020, 1, 4, 0, 0, 0, 0, time.UTC).Unix()); err != nil {
		t.Fatal(err)
	}
	sent := h.Background()
	if len(sent) != 1 || !strings.Contains(sent[0].Text(), "Example issue") {
		t.Fatalf("poll messages: %+v", sent)
	}
}

func TestWebhookSignatureFilterAndDeduplication(t *testing.T) {
	s := &cards{}
	p := newPlugin(s, githubFixture(t))
	var session context.Context
	p.OnReady(func(ctx context.Context) error { session = context.WithoutCancel(ctx); return nil })
	const secret = "test secret"
	h := plugintest.Start(t, plugintest.Plugin(p), plugintest.Config(p, `{"enable_webhook":true,"webhook_secret":"test secret"}`))
	alice := plugintest.User{ID: "alice"}
	a := plugintest.Private(alice.ID)
	h.Text(a, alice, "/ghsub org/repo issues")
	h.Text(a, alice, "/ghsub org/repo main issues")
	hook := func(kind, id, body, key string) int {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/github/webhook", strings.NewReader(body)).WithContext(session)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-GitHub-Event", kind)
		req.Header.Set("X-GitHub-Delivery", id)
		mac := hmac.New(sha256.New, []byte(key))
		mac.Write([]byte(body))
		req.Header.Set(github.SHA256SignatureHeader, "sha256="+hex.EncodeToString(mac.Sum(nil)))
		w := httptest.NewRecorder()
		s.hook(w, req)
		return w.Code
	}
	body := `{"repository":{"full_name":"org/repo"},"action":"opened","issue":{"number":1,"title":"Webhook example"}}`
	if got := hook("issues", "alternate", body, "wrong key"); got != 401 {
		t.Fatalf("invalid signature: %d", got)
	}
	for _, kind := range []string{"issues", "issues", "star"} {
		if got := hook(kind, "same", body, secret); got != 204 {
			t.Fatalf("%s: %d", kind, got)
		}
	}
	result := h.Trigger(p, "github-webhook-delivery")
	sent := result.Sends
	if len(sent) != 1 || !strings.Contains(sent[0].Text(), "Webhook example") {
		t.Fatalf("webhook messages: %+v", sent)
	}
}

func TestReadmeImage(t *testing.T) {
	pages, err := renderReadme("GitHub README\n汉字示例")
	if err != nil || len(pages) != 1 {
		t.Fatalf("render: %d images, %v", len(pages), err)
	}
	img, err := png.Decode(bytes.NewReader(pages[0]))
	if err != nil || img.Bounds().Dx() != 940 {
		t.Fatalf("decode image: %v", err)
	}
}
