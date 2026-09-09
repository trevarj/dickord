# Dickord

Dickord bridges selected channels from a personal Discord account into an
[Ergo](https://ergo.chat/) IRC server. It combines a small Go sidecar with the
vendored [`rdircd`](https://github.com/mk-fg/reliable-discord-client-irc-daemon)
fork under `rdircd/`, which handles Discord's user-client protocol. `rdircd/UPSTREAM`
records the pinned upstream commit.

> [!WARNING]
> Automating a personal Discord account can violate Discord's terms of service
> and may result in account termination. Use at your own risk.

## Features

- Selected guild channels and dynamic threads
- Direct messages and group DMs
- Automatic Ergo channel creation and owner autojoin
- Discord history catch-up and mirrored channel topics
- Discord author attribution using `/discord` relay identities
- Self-authored Discord messages attributed to the configured owner
- Owner-only IRC-to-Discord messages and actions
- Native IRCv3 reactions and message redactions in both directions
- Native Motd-to-Discord replies and optional typing notifications
- Discord photo, video, and file attachments rehosted through Ergo's FILEHOST
- FILEHOST uploads sent as Discord attachments and Ogg Opus voice messages

## Requirements

- Docker with Compose support
- An Ergo server with TLS, SASL, `account-tag`, IRCv3 message tags, and
  `draft/metadata-2` enabled for channel avatar metadata
- A registered Ergo account for the bridge
- A restricted Ergo OPER role permitting only the commands Dickord needs,
  including `RELAYMSG`, `SAJOIN`, and the `metadata` role capability

The `rdircd` IRC listener stays inside the Compose network and is not published
to the host.

## Setup

1. Create local configuration from the public templates:

   ```sh
   cp .env.example .env
   cp config/dickord.example.json config/dickord.json
   cp config/rdircd.example.ini config/rdircd.ini
   cp secrets/discord.example.ini secrets/discord.ini
   cp secrets/ergo-sasl.example secrets/ergo-sasl
   cp secrets/ergo-oper.example secrets/ergo-oper
   chmod 0600 .env config/dickord.json config/rdircd.ini secrets/*
   ```

2. Set the Discord token in `secrets/discord.ini`, the Ergo SASL password in
   `secrets/ergo-sasl`, and the restricted OPER password in
   `secrets/ergo-oper`.

3. Edit `config/dickord.json`:

   - `ergo.address` and `ergo.tls_server_name` select the Ergo endpoint.
   - `ergo.owner_accounts` lists the only Ergo accounts allowed to send to
     Discord.
   - `channels.guild_include_globs` selects guild channels and threads.
   - `channels.dm_include_pattern` selects DMs and group DMs.
   - `channels.channel_aliases` gives rdircd source names friendlier aliases.

   A guild thread is normally matched by adding `.=*` after its parent channel
   pattern. The default `me.*` pattern includes Discord DMs.

   To forward Motd typing notifications to Discord, set
   `typing-send-enabled = yes` under `[irc]` in `config/rdircd.ini`. This is
   deliberately off by default because typing status is presence information.
   The project defaults also leave Discord presence unchanged and do not mark
   DMs read; enable `status-set` or `msg-ack` explicitly if wanted.

4. For an Ergo server using a private CA, install its public certificate and
   enable the optional mount:

   ```sh
   cp /path/to/ergo-ca.pem config/ergo-ca.pem
   cp compose.override.example.yaml compose.override.yaml
   chmod 0600 config/ergo-ca.pem compose.override.yaml
   ```

   Then set `ergo.ca_file` to `/run/config/ergo-ca.pem`. Leave it empty when
   using normal system trust roots.

5. Start the bridge:

   ```sh
   docker compose up -d --build
   docker compose logs -f dickord rdircd
   ```

## Channel and identity mapping

rdircd exposes Discord sources as IRC channels such as `guild.channel`, thread
names beneath their parent channel, and `me.*` DM channels. Dickord filters
those names with the configured globs, applies aliases, encodes unsupported IRC
characters, and places them under `#discord.` by default.

Discord authors appear through Ergo `RELAYMSG` identities ending in `/discord`.
Messages authored by the Discord account itself use the first configured owner
name with the same suffix. This avoids collisions with real Ergo users.

Motd replies to recently correlated Discord messages become native Discord
replies. Correlation is kept in a bounded 4,096-message in-memory cache; after a
restart or cache eviction, the same action safely sends a normal message instead.
Long messages are split as before, with only the first Discord chunk attached
to the reply.

## Attachments

Dickord discovers the HTTPS upload service advertised by Ergo's `FILEHOST`,
`draft/FILEHOST`, or `soju.im/FILEHOST` ISUPPORT token. It reuploads actual Discord
attachments and relays the returned URL as plain text, preserving `/discord`
attribution, message order, replies, and redactions. Clients that preview image
and video URLs can then display the hosted media. Pasted links are not uploaded.

In the other direction, an authorized message consisting only of a URL on the
advertised FILEHOST origin is downloaded and uploaded to Discord as an
attachment. Discord then serves it from its media CDN. URLs on other origins
remain ordinary text.

Ergo advertises the service; it does not provide HTTP file storage itself.
Configure a compatible [FILEHOST service](https://codeberg.org/emersion/soju/src/branch/master/doc/ext/filehost.md)
and advertise its upload endpoint in Ergo's `ircd.yaml`:

```yaml
server:
  additional-isupport:
    "soju.im/FILEHOST": "https://files.example.com/upload"
```

No new Dickord JSON fields, secret mounts, or Compose services are required.
Uploads use the bridge's existing Ergo account and SASL password as HTTP Basic
credentials, so advertise only a service you trust with those credentials.
The endpoint must use HTTPS with a valid hostname certificate; HTTPS uploads
use `ergo.ca_file` when configured, otherwise system trust roots. Discord CDN
downloads always use system trust roots and never receive IRC credentials.
Neither downloads nor uploads follow redirects.

Attachments stream without local storage, with a 100 MiB limit and a 60-second
download/upload deadline. Unknown sizes, expired Discord URLs, rejected uploads,
or absent FILEHOST support fall back to the existing Discord-link rendering.
Catch-up uses the same path; replaying an attachment can upload another copy.
Hosted file access and retention belong to the filehost: redacting an IRC
message does not delete the uploaded file.

## Voice messages

Upload an Ogg Opus recording through the IRC client's normal FILEHOST flow and
send the returned URL. When FILEHOST serves it as `audio/ogg`, Dickord derives
its duration from the Ogg stream and sends it using Discord's native voice
message flag. No custom IRC command or tag is required.

Dickord uses a minimal flat waveform because IRC FILEHOST does not provide
decoded audio samples. A recorder integration can override the duration and
waveform with `+dickord/voice-duration` and `+dickord/voice-waveform` client
tags; waveform values are standard base64 encoding of 1–256 amplitude bytes.

Outbound Discord uploads must have a known size no larger than 25 MiB.
Downloads do not follow redirects or receive Discord credentials. Malformed
Ogg audio fails closed instead of becoming a generic attachment.

## Security model

Dickord authorizes outbound messages, typing, reactions, and redactions from the
IRCv3 `account` tag, not from nicknames. Only accounts listed in
`ergo.owner_accounts` are accepted. Missing account tags, OPER authorization,
required capabilities, message IDs, or channel mappings fail closed.

Keep `.env`, local configuration, private CA certificates, and all real secret
files untracked. The provided `.gitignore` already excludes their expected
paths. Do not place credentials in Compose arguments, image layers, JSON
configuration, or Git history.

## Development

A pinned Nix development shell supplies Go, Docker CLI, and supporting tools:

```sh
nix develop
gofmt -w *.go
go vet ./...
go test -race ./...
go test -tags integration -run TestErgoIntegration -v .
nix flake check
docker compose config --quiet
docker compose build
```

The integration test starts `ghcr.io/ergochat/ergo:v2.19.1` with Docker and
uses host networking.
