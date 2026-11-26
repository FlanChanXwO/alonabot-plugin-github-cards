# GitHub Cards for AlonaBot

[English](README.md)

在聊天中发送 GitHub 仓库、Issue 或 PR 链接，机器人会发出 GitHub 原生 OpenGraph 卡片。插件也支持查询详情和按会话订阅 GitHub 更新。

## 安装

使用 AlonaBot 0.3.3，在 CLI 安装并确认信任来源：

```fish
alonabot install --source FlanChanXwO/alonabot-plugin-github-cards
```

## 使用

| 命令 | 示例与作用 |
| --- | --- |
| `/ghlink on/off` | 打开或关闭当前会话的链接卡片。 |
| `/ghissue`、`/ghpr` | `/ghissue owner/repo#123`、`/ghpr owner/repo 123`；有默认仓库或唯一订阅仓库时可只填编号。 |
| `/ghreadme` | `/ghreadme owner/repo`，优先发送 README 图片，渲染失败时发送全文。 |
| `/ghlimit` | 查看 GitHub API 限额和重置时间。 |
| `/ghdefault [owner/repo]` | 设置或查看当前会话的默认仓库；订阅成功时也会更新默认仓库。 |
| `/ghsub owner/repo [分支] [事件,事件]` | 如 `/ghsub owner/repo main issues,commits`，也可写 `/ghsub owner/repo/main issues,commits`。 |
| `/ghunsub [owner/repo] [分支] [事件,事件]` | 用订阅时相同的参数取消；省略参数则取消当前会话全部订阅。 |
| `/ghlist` | 列出当前会话的订阅。 |

`/ghdef`、`/ghis`、`/ghrate` 分别是默认仓库、Issue、限额命令的别名。命令前缀由 AlonaBot 配置。查询和订阅面向公开仓库，避免把插件凭证可见的私有仓库内容带进共享聊天。

## 配置通知

在 WebUI 的插件配置中设置：

- `auto_resolve_links`：默认自动发卡片；会话可以用 `/ghlink` 覆盖。
- `check_interval`：默认为 30 分钟。轮询窗口内新建的 `issues`、`prs`、`commits` 和已发布的 `releases`；分支筛选用于提交，按提交时间检查。要接收更早创建的提交随后推入分支的通知，请使用 Webhook。订阅从当前时间开始。
- `github_token`：可选，提升 API 限额。通过插件的 Secret 字段填写。
- `enable_webhook`、`webhook_secret`：切换至 Webhook 时，先设置 Secret，再启用并重载插件。轮询随之停止；切回时从当前时间继续。仅 Webhook 支持的事件在轮询模式下会标记为需 Webhook。

在 GitHub 仓库的 **Settings → Webhooks** 设定 HTTPS 地址
`https://<你的 AlonaBot 域名>/plugins/alonabot-plugin-github-cards/github/webhook`，
Content type 选 `application/json`，填写相同 Secret。按需选择 Issues、Issue comments、Pull requests、Pull request reviews、Pull request review comments/threads、Commit comments、Discussions、Discussion comments、Pushes、Releases、Forks、Stars、Branch or tag creation。订阅筛选名分别为 `issues`、`issue_comment`、`prs`、`pull_request_review`、`pull_request_review_comment`、`pull_request_review_thread`、`commit_comment`、`discussion`、`discussion_comment`、`commits`、`releases`、`fork`、`star`、`create`。分支筛选用于带分支的投递；仓库级事件保持生效。

## 开发与来源

使用 Go 1.27.3 和 AlonaBot CLI 0.3.3。运行 `go test ./...`、`alonabot check .` 和 `alonabot test .`；版本标签触发打包和 GitHub Release。

移植自 Soulter 的 [GitHub Cards for AstrBot](https://github.com/Soulter/astrbot_plugin_github_cards/tree/1ee29beafa7069b9fdf2602897344da07cbca131)，沿用 AGPL-3.0。README 渲染使用 [Noto Sans CJK SC](https://github.com/notofonts/noto-cjk/tree/Sans2.004)（Copyright 2014–2021 Adobe，SIL OFL 1.1；见 [字体许可](fonts/OFL.txt)）。
