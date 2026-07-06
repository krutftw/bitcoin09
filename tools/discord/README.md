# Bitcoin 09 Discord Bot Setup

This folder contains a dependency-free Node script for setting up the Bitcoin 09 Discord server through a bot.

The script is idempotent: it reads existing roles/channels first, creates only missing items, updates topics/parents where needed, and skips seed messages already posted by the bot.

## 1. Create and Invite the Bot

Create a Discord application in the Developer Portal, add a bot user, and copy the bot token. Keep the token secret.

Generate an invite URL:

```powershell
$env:DISCORD_CLIENT_ID = "your_application_client_id"
$env:DISCORD_GUILD_ID = "your_bitcoin09_server_id"
node tools/discord/setup-server.mjs --invite
```

Open the printed URL and authorize the bot into the Bitcoin 09 server.

The invite requests only the permissions this setup script needs: manage channels, manage roles, view/send messages, embed links, and read message history.

## 2. Dry Run

Either export env vars:

```powershell
$env:DISCORD_BOT_TOKEN = "your_bot_token"
$env:DISCORD_GUILD_ID = "your_bitcoin09_server_id"
node tools/discord/setup-server.mjs
```

Or create `tools/discord/.env` from `.env.example`. Do not commit `.env`.

## 3. Apply Setup

```powershell
node tools/discord/setup-server.mjs --apply
```

To also seed starter messages:

```powershell
node tools/discord/setup-server.mjs --apply --seed
```

After use, clear the token from the current PowerShell process if you exported it:

```powershell
Remove-Item Env:DISCORD_BOT_TOKEN
```

## What It Creates

Roles:

- `⛏ Miner`
- `🧱 Node Operator`
- `🏊 Pool Operator`
- `🛠 Developer`
- `🛡 Moderator`
- `📣 Announcements`
- `🤝 Contributor`
- `🧪 Tester`

Categories, text channels, and voice channels:

- `📌 INFO`: `#📣-announcements`, `#👋-start-here`, `#📜-rules`, `#🔗-resources`
- `💬 COMMUNITY`: `#💬-general`
- `⛏ MINING`: `#⛏-mining-help`, `#📈-hashrate`
- `🌐 NETWORK`: `#🏊-pools-and-nodes`, `#🧱-node-operators`
- `🛠 DEVELOPMENT`: `#🛠-dev-log`, `#🐞-bug-reports`, `#💡-ideas`
- `🔊 VOICE`: `🔊-lobby`, `⛏-mining-room`, `🛠-dev-sync`

The info channels are configured so `@everyone` cannot send messages. The script keeps aliases for the original plain names, so existing channels are renamed/reused instead of duplicated.

## Official Docs

- Discord OAuth2: https://docs.discord.com/developers/topics/oauth2
- Discord OAuth2 and bot permissions: https://docs.discord.com/developers/platform/oauth2-and-permissions
- Discord guild channel API: https://docs.discord.com/developers/resources/guild#create-guild-channel
