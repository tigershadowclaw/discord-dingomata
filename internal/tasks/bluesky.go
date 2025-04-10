package tasks

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"snoozybot/internal/config"
	"snoozybot/internal/i18n"
	"strings"
	"text/template"
	"time"

	"github.com/bluesky-social/indigo/api/atproto"
	"github.com/bluesky-social/indigo/api/bsky"
	"github.com/bluesky-social/indigo/xrpc"
	"github.com/samber/lo"
)

const BSKY_HOST = "https://bsky.social"

var bskyClient *xrpc.Client
var userLastKnownPosts = make(map[string]time.Time)

func init() {
	username := lo.Must(os.LookupEnv("BSKY_USERNAME"))
	appPassword := lo.Must(os.LookupEnv("BSKY_APP_PASSWORD"))

	sess := lo.Must(atproto.ServerCreateSession(context.Background(), &xrpc.Client{Host: BSKY_HOST}, &atproto.ServerCreateSession_Input{
		Identifier: username,
		Password:   appPassword,
	}))

	bskyClient = &xrpc.Client{
		Host: BSKY_HOST,
		Auth: &xrpc.AuthInfo{
			Did:        sess.Did,
			AccessJwt:  sess.AccessJwt,
			RefreshJwt: sess.RefreshJwt,
		},
	}
}

func bskyRefreshSession(tctx *TaskData) error {
	resp, err := atproto.ServerRefreshSession(tctx.Context, &xrpc.Client{
		Host: BSKY_HOST,
		Auth: &xrpc.AuthInfo{
			Did:        bskyClient.Auth.Did,
			AccessJwt:  bskyClient.Auth.RefreshJwt,
			RefreshJwt: bskyClient.Auth.RefreshJwt,
		},
	})
	if err != nil {
		return err
	}
	bskyClient.Auth.AccessJwt = resp.AccessJwt
	bskyClient.Auth.RefreshJwt = resp.RefreshJwt
	return nil
}

var bskyNotificationTask = PeriodicTask{
	Name:     "bskyNotificationTask",
	Interval: 1 * time.Minute,
	TaskHandler: func(tctx *TaskData) error {
		if err := bskyRefreshSession(tctx); err != nil {
			return err
		}
		guildUsers := config.BskyNotifUsers.GetAll() // guildId -> users[]
		channels := config.BskyNotifChannelID.GetAll()
		templates := config.BskyNotifTemplate.GetAll()

		// Reverse map into user -> guildId[]
		// This way each user is only checked once even if they're in multiple guilds
		userGuilds := make(map[string][]string)
		for guildId, users := range guildUsers {
			users, err := users.Value()
			if err != nil {
				tctx.Logger.Error().Err(err).Str("guild_id", guildId).Msg("Failed to parse bluesky user IDs.")
				continue
			}
			for _, user := range users {
				userGuilds[user] = append(userGuilds[user], guildId)
			}
		}

		for user, guildIds := range userGuilds {
			// Get user's last 5 posts
			posts, err := bsky.FeedGetAuthorFeed(tctx.Context, bskyClient, user, "", "posts_no_replies", false, 5)
			if err != nil {
				tctx.Logger.Error().Err(err).Str("user", user).Msg("Failed to get user's latest posts.")
				continue
			}
			if len(posts.Feed) == 0 {
				tctx.Logger.Warn().Str("user", user).Msg("No posts found for user.")
				continue
			}
			tctx.Logger.Trace().Any("posts", posts.Feed).Msg("Got bluesky posts.")
			lastPostCreated, err := time.Parse(time.RFC3339, posts.Feed[0].Post.Record.Val.(*bsky.FeedPost).CreatedAt)
			if err != nil {
				tctx.Logger.Error().Err(err).Str("user", user).Msg("Failed to parse post creation time.")
				continue
			}
			lastKnownPost := userLastKnownPosts[user]
			if lastKnownPost.IsZero() {
				// first run; record the most recent post
				tctx.Logger.Info().Str("user", user).Time("created_at", lastPostCreated).Msg("Found initial bluesky post.")
				userLastKnownPosts[user] = lastPostCreated
				continue
			} else if lastPostCreated.After(lastKnownPost) {
				tctx.Logger.Debug().Str("user", user).Time("last_known_post", lastKnownPost).Time("new_posts", lastPostCreated).Msg("Found new bluesky post.")
				// new posts! find all new ones in case more than one
				newPosts := lo.Filter(posts.Feed, func(post *bsky.FeedDefs_FeedViewPost, _ int) bool {
					createdAt, err := time.Parse(time.RFC3339, post.Post.Record.Val.(*bsky.FeedPost).CreatedAt)
					if err != nil {
						tctx.Logger.Error().Err(err).Str("user", user).Any("post", post.Post.Record).Msg("Failed to parse post creation time.")
						return false
					}
					return createdAt.After(lastKnownPost)
				})
				userLastKnownPosts[user] = lastPostCreated.Add(5 * time.Second) // a buffer to guard against rounding issues?

				for _, post := range newPosts {
					for _, guildId := range guildIds {
						_sendBskyNotification(tctx, user, guildId, channels[guildId], templates[guildId], post)
					}
				}
			}
		}
		return nil
	},
}

func _sendBskyNotification(tctx *TaskData, user string, guildId string, channel *config.ConfigValue[json.Number], tmplConfig *config.ConfigValue[string], post *bsky.FeedDefs_FeedViewPost) {
	channelId, err := channel.Value()
	if err != nil {
		tctx.Logger.Error().Err(err).Str("guild_id", guildId).Msg("Failed to parse channel id.")
		return
	}
	tmplStr, err := tmplConfig.Value()
	if err != nil {
		tctx.Logger.Error().Err(err).Str("guild_id", guildId).Msg("Failed to parse template.")
		return
	}
	tmpl, err := template.New("bsky_notif").Parse(tmplStr)
	if err != nil {
		tctx.Logger.Error().Err(err).Str("guild_id", guildId).Msg("Failed to create template for bsky notification.")
		return
	}
	bot, ok := tctx.BotManager.GuildBots[guildId]
	if !ok {
		tctx.Logger.Error().Str("guild_id", guildId).Msg("No bot found for guild.")
		return
	}
	postId, ok := lo.Last(strings.Split(post.Post.Uri, "/"))
	if !ok {
		tctx.Logger.Error().Str("user", user).Str("post_uri", post.Post.Uri).Msg("Failed to parse post ID.")
		return
	}
	content := i18n.TemplateString(tmpl, &i18n.Vars{"url": fmt.Sprintf("https://bsky.app/profile/%s/post/%s", post.Post.Author.Handle, postId)})
	if _, err := bot.ChannelMessageSend(string(channelId), content); err != nil {
		tctx.Logger.Error().Err(err).Str("guild_id", guildId).Str("user", user).Msg("Failed to send bluesky notification.")
	} else {
		tctx.Logger.Info().Str("guild_id", guildId).Str("user", user).Str("content", content).Msg("Found new bluesky post, sent notification.")
	}
}
