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

- `👑 Owner`
- `🤖 Bot`
- `⛏ Miner`
- `🧱 Node Operator`
- `🏊 Pool Operator`
- `🛠 Developer`
- `🛡 Moderator`
- `🔔 Updates`
- `🤝 Contributor`
- `🧪 Tester`

The owner, bot, pool operator, developer, and moderator roles are displayed separately in the Discord member list when members have those roles. The script assigns `👑 Owner` to the guild owner and `🤖 Bot` to the bot user.

Self-serve roles:

- `⛏ Miner`
- `🧱 Node Operator`
- `🔔 Updates`
- `🤝 Contributor`
- `🧪 Tester`

Manual roles:

- `👑 Owner`
- `🤖 Bot`
- `🏊 Pool Operator`
- `🛠 Developer`
- `🛡 Moderator`

Self-serve roles are chosen with buttons in `#🎭-roles`. `🔔 Updates` is only an opt-in ping role for releases, fork warnings, pool/node incidents, and important network notes. It does not give posting permissions.

Categories, text channels, and voice channels:

- `📌 INFO`: `#📣-announcements`, `#👋-start-here`, `#📜-rules`, `#🎭-roles`, `#🔗-resources`
- `💬 COMMUNITY`: `#💬-general`, `#💱-otc-trading`
- `⛏ MINING`: `#⛏-mining-help`, `#📈-hashrate`
- `🌐 NETWORK`: `#🏊-pools-and-nodes`, `#🧱-node-operators`
- `🛠 DEVELOPMENT`: `#🛠-dev-log`, `#🐞-bug-reports`, `#💡-suggestions`
- `🔊 VOICE`: `🔊-lobby`, `⛏-mining-room`, `🛠-dev-sync`

The info channels are configured so `@everyone` cannot send messages. The script keeps aliases for the original plain names, so existing channels are renamed/reused instead of duplicated.

`#💱-otc-trading` is for community buy/sell posts only. It is not an official exchange, it does not set an official 09C price, and staff does not provide official escrow.

`#💡-suggestions` is for practical improvements: miner UX, docs, explorer, wallets, pools, listings, and community setup. Bugs still belong in `#🐞-bug-reports`.

## Live Stats Bot

`stats-bot.mjs` reads the public pool API and explorer status, then formats the current miner count, hashrate, height, payouts, and top pool addresses.

Print the current stats locally:

```powershell
node tools/discord/stats-bot.mjs
```

Register the guild slash command:

```powershell
node tools/discord/stats-bot.mjs --register-commands
```

This registers `/stats`. Self-serve roles are handled by the clickable buttons in `#🎭-roles`, so the watch process must be running for the buttons to respond.

Post or update one bot-authored stats message in `#🏊-pools-and-nodes`:

```powershell
node tools/discord/stats-bot.mjs --post
```

Run the bot so `/stats` answers inside Discord:

```powershell
node tools/discord/stats-bot.mjs --watch
```

The slash command and role buttons only respond while the watch process is running. The one-shot `--post` mode is safe to run repeatedly because it edits the existing bot message when one is already present.

## Official Docs

- Discord OAuth2: https://docs.discord.com/developers/topics/oauth2
- Discord OAuth2 and bot permissions: https://docs.discord.com/developers/platform/oauth2-and-permissions
- Discord guild channel API: https://docs.discord.com/developers/resources/guild#create-guild-channel
- Discord application commands: https://docs.discord.com/developers/interactions/application-commands
- Discord interactions: https://docs.discord.com/developers/interactions/receiving-and-responding
