# homestacks fork of mautrix-meta

This is [gpdl49/mautrix-meta](https://github.com/gpdl49/mautrix-meta), a fork of
[mautrix/meta](https://github.com/mautrix/meta) used by the
[homestacks](https://github.com/gpdl49/homestacks) GitOps repo, **for the Instagram bridge only**
(`pkg/igconnector`, binary `mautrix-instagram`). Facebook/Messenger (`pkg/connector`) is untouched. Read
this file first when an upstream merge conflicts.

## What the fork adds

One reliability fix, connection retries (below), which is being offered upstream. Once upstream ships an
equivalent, homestacks goes back to the upstream image and this fork is retired.

The fork used to carry held messages (`!ig schedule`, `!ig offline-hours`, `!ig timezone`), frozen chats
(`!ig freeze`/`unfreeze`) and deprecated chats (`!ig deprecate`). They were removed after
`v0.2610.0-hs.4`; that tag has the last code. Their tables (`homestacks_held_message`,
`homestacks_frozen_chat`, `homestacks_frozen_event`, `homestacks_deprecated_chat`,
`homestacks_deprecated_event`, plus the version tables `homestacks_heldmsg_version`,
`homestacks_freeze_version` and `homestacks_deprecate_version`) are no longer read or written and can be
dropped.
Old `timezone` and `offline_hours` keys in login and portal metadata are ignored.

### Connection retries after transient failures

Upstream's `connectWithRetry` retries only Meta `lserror` responses. Any other failure to load the inbox
that is not a known account problem sends `UNKNOWN_ERROR` and returns. It doesn't retry and doesn't
reschedule the periodic reconnect, so the login stays disconnected until the bridge restarts. On
2026-11-08 the 20h periodic reconnect hit six HTTP 503s from `instagram.com/direct/inbox/` and the bridge
stayed down for about 34 hours. Upstream `main` still had the same code on 2026-11-09.

The fork treats an error wrapping `httpclient.ErrMaxRetriesReached` as transient. The HTTP client only
returns that after retrying a 5xx, a 429 or a network error itself; 4xx and other permanent errors come
back unwrapped and keep upstream's behaviour. A transient failure sends `TRANSIENT_DISCONNECT` and calls
`connectWithRetry` again with upstream's backoff (2s doubling to 1024s, `MaxConnectRetries` = 10, about 34
minutes in all). When those run out, `UNKNOWN_ERROR` is sent as before, and a full reconnect is also
scheduled 30 minutes later (`transientRetryLaterDelay`). That reconnect starts a new round, so a long
Instagram outage is retried indefinitely at roughly one round per hour. The scheduled reconnect uses
`stopPeriodicReconnect`, so `Disconnect` cancels it. That includes bridgev2's own
`bridge.unknown_error_auto_reconnect`, which the homestacks config also sets as a second safety net.

The WhatsApp fork does not need this: whatsmeow already retries a failed first connect
(`initial_auto_reconnect`, on by default) and reconnects by itself after 503 stream errors and 500/503
connect failures.

## Branches and tags

| Ref | Meaning |
|---|---|
| `main` | Untouched mirror of upstream `main`. Never commit here. |
| `homestacks` | Default branch. Upstream release tag + fork commits. Advanced by **merging** (`--no-ff`) each new upstream tag, never rebased, so submodule pointers in homestacks stay valid. |
| `vX.Y.Z-hs.N` | Release tag: upstream tag `vX.Y.Z` plus fork revision `N`. `N` bumps only when fork code changes on the same upstream base. `-hs.0` is upstream plus CI only. |

Images: `ghcr.io/gpdl49/mautrix-meta:<tag>` (and `:homestacks` floating), built from upstream's unmodified
**`Dockerfile.ig`** by `.github/workflows/publish.yml`. The plain `Dockerfile` builds Facebook, which is not
what this fork is for.

## Automation

- `.github/workflows/upstream-sync.yml`: daily. Fetches upstream tags, merges the newest `v0.NNNN.N` release
  tag not yet in `homestacks`, builds, vets and tests, pushes, tags `<tag>-hs.1`, and calls `publish.yml`. On a
  merge conflict the run fails, lists the conflicting files in its summary and stops; GitHub's failed-run
  notification is the alert (issues are disabled on the fork). Resolve by hand (below).
- `.github/workflows/publish.yml`: builds and pushes the image for a `v*-hs.*` tag (on tag push, on dispatch,
  or called by the sync workflow).
- `.github/workflows/go.yml`: upstream's lint workflow, kept as is.


## Where the fork touches upstream code

One hook, tagged `// homestacks:` (`grep -rn 'homestacks:' pkg/` finds it):

| Upstream file | Hook |
|---|---|
| `pkg/igconnector/client.go` | `retryTransientConnectError` guard (an `if` with a `return`) at the top of the final `else` in `connectWithRetry`'s index-load error handling, before upstream's `MetaConnectError` `UNKNOWN_ERROR` |

New files:

| File | Purpose |
|---|---|
| `pkg/igconnector/reconnect.go` | transient-error check, retry with backoff, full reconnect scheduled after retries run out |
| `pkg/igconnector/reconnect_test.go` | error classification on the real error chains, short-circuit cases, cancellation of the scheduled reconnect |

`Dockerfile.ig` installs `/usr/bin/mautrix-instagram`, but upstream's `docker-run.sh` still calls
`/usr/bin/mautrix-meta`, so the image's default `CMD` does not work. The homestacks Deployment sets an
explicit command (`/usr/bin/mautrix-instagram`). The fork does not patch `docker-run.sh`.

## Resolving an upstream merge conflict by hand

```sh
git checkout homestacks
git fetch upstream --tags
git merge --no-ff vX.Y.Z
# fix conflicts: the fork's side is always the `// homestacks:` line or reconnect*.go
go build -tags goolm ./... && go vet -tags goolm ./... && go test -tags goolm ./pkg/...
git commit
git push origin homestacks
git tag -a vX.Y.Z-hs.1 -m "..." && git push origin vX.Y.Z-hs.1   # publish.yml builds the image
```

If upstream changed something the fix relies on (`connectWithRetry`/`MaxConnectRetries`,
`stopPeriodicReconnect`, `FullReconnect`, `httpclient.ErrMaxRetriesReached`), the build or `go vet` step
will say so; fix it in `reconnect*.go` rather than in upstream code. If upstream starts retrying these
errors itself, drop the hook and `reconnect*.go`, and switch homestacks to the upstream image.

## Building locally

```sh
go build -tags goolm ./...          # pure-Go olm, no libolm needed
go test -tags goolm ./pkg/...
docker build -f Dockerfile.ig -t mautrix-instagram:dev .
```
