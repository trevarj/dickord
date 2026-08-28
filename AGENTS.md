# AGENTS.md

## Scope

Dickord is a personal Discord account to Ergo IRC bridge. Keep changes small,
fail closed, and preserve existing configuration and Compose compatibility.

## Architecture

- `bridge.go`: Ergo and rdircd connections, authorization, relay, catch-up,
  reactions, redactions, and autojoin.
- `mapping.go`: Discord source selection and safe IRC channel/nick mapping.
- `config.go`: strict JSON loading, defaults, validation, TLS, and secret files.
- `compose.yaml` and Dockerfiles: isolated runtime services and pinned builds.
- `rdircd-ircv3-reactions.patch`: bounded IRCv3 extensions to the pinned
  upstream rdircd revision.

Keep Discord protocol handling in pinned upstream rdircd. Patch it only when a
small, reviewed extension cannot live in the Go sidecar.

## Invariants

- Never commit tokens, passwords, certificates, personal hosts,
  accounts, guilds, channels, `.env`, local config, secrets, or Compose
  overrides. Track generic examples only.
- Authorize IRC-to-Discord traffic from configured IRCv3 account tags. Never
  trust nicknames or unauthenticated messages.
- Missing authorization, capabilities, mappings, or correlated message IDs must
  fail closed.
- Preserve `/discord` RELAYMSG attribution and native IRCv3 reaction/redaction
  behavior.
- Keep the rdircd IRC port internal to Compose.
- Keep the Ergo OPER role least-privilege, limited to required commands such as
  `RELAYMSG` and `SAJOIN`.
- Do not break existing JSON fields, template paths, secret mounts, or Compose
  environment overrides without an explicit migration.

## Workflow

1. Trace the affected flow and inspect every caller before fixing a bug.
2. Make the smallest shared root-cause change; avoid speculative abstractions
   and new dependencies.
3. Add focused regression coverage for non-trivial behavior.
4. Run the relevant checks below.
5. Never commit, push, rewrite history, or deploy unless explicitly requested.

## Validation

```sh
gofmt -w *.go
go vet ./...
go test -race ./...
go test -tags integration -run TestErgoIntegration -v .
nix flake check
docker compose config --quiet
docker compose build
```

The integration test requires Docker and starts the pinned Ergo image with host
networking. Before publishing, scan tracked files and Git history for personal
identifiers, credentials, private keys, and certificates.
