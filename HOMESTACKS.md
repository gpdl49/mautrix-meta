# homestacks fork of mautrix-meta

This is [gpdl49/mautrix-meta](https://github.com/gpdl49/mautrix-meta), a fork of
[mautrix/meta](https://github.com/mautrix/meta) used by the
[homestacks](https://github.com/gpdl49/homestacks) GitOps repo, **for the Instagram bridge only**
(`pkg/igconnector`, binary `mautrix-instagram`). Facebook/Messenger (`pkg/connector`) is untouched. It
mirrors [gpdl49/mautrix-whatsapp](https://github.com/gpdl49/mautrix-whatsapp)'s `HOMESTACKS.md`; read
this file first when an upstream merge conflicts.

## What the fork adds

Three features, all driven from the chat's own room with the `!ig` prefix. The code was copied from the
WhatsApp fork (no shared module) and adapted; differences are listed at the end.

### Held messages: scheduling and offline hours

Matrix messages the bridge keeps back and sends to Instagram later, managed from the chat's room:

```
!ig schedule 18:30                 # hold your next message here (within 10 min) until 18:30
!ig schedule +45m Running late     # send this text in 45 minutes (also `in 45m`, `6:30pm`, `7am`)
!ig schedule list | now [n|all] | cancel [n|all]
!ig offline-hours 22:00-08:00      # messages sent 22:00–08:00 wait until 08:00
!ig offline-hours show | off
!ig timezone Europe/London         # zone for both; sets it on all your logins
```

Times are a time of day (the next time the clock reads it) or a `+` delay, at most 24 hours ahead. A
held message gets a ⏰ reaction from the bot, removed when it is sent; the first message of an
offline-hours batch also gets a notice. Messages sent while an offline batch is still waiting join it, even
just after the window ends, so the chat never sees them out of order. An explicit schedule wins over offline
hours. Any message type is held (media, replies), but edits are not: delete the held message and send
again. **Deleting a held message in Matrix cancels it**; the bridge checks for a redaction just before
sending. Instagram has no polls.

How: `checkHold` refuses the message with a `PENDING` status before it is converted (nothing is uploaded)
and stores the decrypted event in `homestacks_held_message`. Every 15s due rows are deleted and the same
event is queued back with `Bridge.QueueMatrixEvent`; `checkHold` lets it through once, so the Instagram
message maps to the original Matrix event. Rows wait while their login is disconnected (the whole room, to
keep order) and are dropped if the login is deleted. A bare `!ig schedule <time>` is in memory only and is
lost on restart.

**Pacing.** Instagram's ban thresholds are unknown, so a large batch is not sent in a burst: each 15s pass
releases at most 4 messages, 2–4s apart (`heldReleaseBatch`, `heldReleaseGapMin/Max`). `schedule now`
does not send itself; it makes the messages due and the same paced loop sends them. Forty held messages
therefore take about ten minutes. The constants are in `heldmsg.go`; a test checks a full pass fits inside
the loop interval.

The timezone is `metaid.UserLoginMetadata.Timezone` (new; upstream has none), falling back to the bridge's
local zone (UTC in the image). Offline hours live in portal metadata (`metaid.PortalMetadata.OfflineHours`).
`UserLoginMetadata.CopyFrom` runs on re-login and used to overwrite everything; it now keeps `Timezone`.

### Frozen chats

Stop bridging a chat for a while, then catch up on it in one go:

```
!ig freeze                  # in the chat's room
!ig freeze list
!ig unfreeze                # in the chat's room
```

While a chat is frozen, Instagram events that would create, edit or delete a message, or add or remove a
reaction (including ones from your phone), are not bridged. They are stored in `homestacks_frozen_event`
and `unfreeze` replays them in arrival order through upstream's own `handleDelta`, as if they had just
arrived, with their original timestamps. Matrix messages sent into a frozen chat are refused with a notice,
and held messages due while it is frozen fail the same way. Reactions, edits and redactions from Matrix still
go through. Read receipts, typing, mute, pins, name/image and member changes are not frozen.

How: `checkFrozen` runs in `handleDelta` after the portal is known and stores the delta's raw JSON
(`slidetypes.Delta.Raw`, which marshals back to itself). Replay unmarshals it and calls `handleDelta` with a
context flag that skips the check. It calls `handleDelta`, not the socket handler, so Instagram sequence
IDs and the saved socket position are not touched. The chat stays frozen while it replays in batches; the
locked check that finds nothing left is what unfreezes it, and two unfreezes of one chat cannot run at
once. If an event cannot be replayed (its login is disconnected, or `handleDelta` fails) the chat stays
frozen with that event and everything after it still held. If storing fails the event is bridged straight
away rather than lost. The bridge database holds the events in plaintext while frozen, as on WhatsApp.

**Rows are keyed by a digest of the delta, ordered by arrival, not by `UQSeqID`.** `UQSeqID` is kept in a
column for debugging. It looked like the natural key, but `socket.go` sets the bridge's copy back to 0 on a
resnapshot and reloads it from the mailbox, so the code itself does not guarantee the number keeps counting
up, and a collision would silently drop a message. A redelivery after a reconnect is the same JSON, so the
digest dedupes it, and two different deltas that share a number are both kept. This is reasoning from the
code, not something observed live.

**Guard 1: a resync must not leak frozen messages.** `ResnapshotRequired` drops the saved sequence ID,
the mailbox reloads, and each thread's `ChatResync` forward-backfills anything newer than the last bridged
message from the server, which for a frozen chat is exactly what is being held. So `wrapChatResync` sets
`CheckNeedsBackfillFunc` to `frozenBackfillCheck`, which says no for a frozen chat and otherwise repeats
upstream's default comparison. `FetchMessages` also returns nothing for a forward backfill of a frozen
chat, as a backstop for any other caller. `unfreeze` ends by fetching the thread and queueing a normal
resync, so messages the socket never delivered during the freeze (a resnapshot discards the position) are
backfilled after the replayed ones.

**Guard 2: expired media.** Instagram's CDN URLs carry an expiry (`oe`, see `mediadl.ParseExpiryFromURL`)
and a download after it fails with `ErrForbidden`, which upstream turns into a permanent "Failed to transfer
attachment" notice. Before each batch is replayed, any message whose *content* has a media URL expired or
within 15 minutes of expiry (profile pictures are ignored) is looked up again with `GetThread`, then
`PaginateMessages` back through the thread (at most 25 pages of 20, stopping at the oldest wanted message),
and the stale copy is swapped for the fresh one. A message that cannot be refreshed (unsent meanwhile,
view-once media already opened, too far back, or still expired after the refresh) is bridged as it is, so it
shows Instagram's usual failed-attachment notice, and the `unfreeze` reply says how many such messages
there were. Nothing is dropped silently.

**Targeting.** Only by room. The WhatsApp fork's `!wa freeze +number` form needs a resolver from a
username to a thread; Instagram's lives in `startchat.go` and creates the chat. It was left out; to add it,
write a read-only lookup rather than reusing the chat-starting path. A chat that was deleted while frozen
is not handled: its next event would recreate the portal through the usual resync.

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
  merge conflict it opens an issue titled `Upstream merge conflict: <tag>` (label `upstream-sync`) and stops;
  resolve by hand (below).
- `.github/workflows/publish.yml`: builds and pushes the image for a `v*-hs.*` tag (on tag push, on dispatch,
  or called by the sync workflow).
- `.github/workflows/go.yml`: upstream's lint workflow, kept as is.

## Where the fork touches upstream code

All feature code lives in **new files**. Every hook into an upstream file is a single line tagged
`// homestacks:`: `grep -rn 'homestacks:' pkg/ | grep -v _test` lists them all.

| Upstream file | Hook |
|---|---|
| `pkg/igconnector/connector.go` | two `AddHandlers` lines in `Init` (`cmdSchedule`, `cmdOfflineHours`, `cmdTimezone`; `cmdFreeze`, `cmdUnfreeze`); `startHeldMessages` and `startFreeze` guards (an `if` with a `return`) in `Start`, right after `DB.Upgrade` |
| `pkg/igconnector/handlematrix.go` | `checkFrozenSend` then `checkHold` guards (each an `if` with a `return`) in `HandleMatrixMessage`, after the login check and before `ensureIGID` |
| `pkg/igconnector/handlemeta.go` | `checkFrozen` guard in `handleDelta`, right after the portal is ensured; `CheckNeedsBackfillFunc` field in `wrapChatResync`, set apart by a blank line so gofmt does not re-align upstream's fields |
| `pkg/igconnector/backfill.go` | `frozenPortal` guard at the top of `FetchMessages` |
| `pkg/metaid/dbmeta.go` | `Timezone` field on `UserLoginMetadata`; one line in `CopyFrom` keeping it; `OfflineHours` field on `PortalMetadata` |

New files:

| File | Purpose |
|---|---|
| `pkg/igconnector/heldmsg.go` | held messages: hold guard, `homestacks_held_message` table, paced release loop, time parsing |
| `pkg/igconnector/heldmsg_command.go` | `!ig schedule`, `!ig offline-hours`, `!ig timezone` |
| `pkg/igconnector/heldmsg_test.go` | time parsing, offline windows, event round trip, SQLite round trip of the store, pacing constants |
| `pkg/igconnector/freeze.go` | frozen chats: guard, `homestacks_frozen_chat`/`homestacks_frozen_event` tables, replay, resync guard, media refresh |
| `pkg/igconnector/freeze_command.go` | `!ig freeze`, `!ig unfreeze` |
| `pkg/igconnector/freeze_test.go` | delta round trip, digest dedup and ordering, hold/skip rules, resync guard, expired-media detection |
| `pkg/metaid/offlinehours.go` | `OfflineHours` type stored in portal metadata |
| `pkg/metaid/homestacks.go`, `homestacks_test.go` | `UserLoginMetadata.LoadTimezone`; `CopyFrom` keeps the timezone |
| `pkg/instameow/homestacks.go` | `Client.IsConnected()`, the accessor held messages need (`connected` is private) |

## Differences from the WhatsApp fork

- Call auto-reply, call links, `!ig call` and the per-chat rate limit are **not** ported, on purpose; there
  is no stub or config for them. So a held message's release has no rate-limit interaction.
- Held messages are paced (above); WhatsApp's loop sends every due row each pass.
- Freeze stores Instagram `Delta` JSON, not a whatsmeow message proto, keyed by digest with arrival order.
- Freeze has the two guards above (resync leak, expired media); WhatsApp has neither problem.
- Freeze targets by room only (no `+number` form).
- Instagram has no `HandleMatrixPollStart`, so there is a single hold/freeze-send guard site.
- The login-ready check uses `IGClient.heldReady`, because `IGClient.IsLoggedIn` dereferences `Client`,
  which `Disconnect` sets to nil.
- `Dockerfile.ig` installs `/usr/bin/mautrix-instagram` but upstream's `docker-run.sh` still calls
  `/usr/bin/mautrix-meta`, so the image's default `CMD` does not work. The homestacks Deployment sets an
  explicit command (`/usr/bin/mautrix-instagram`). The fork does not patch `docker-run.sh`.
- Sync tags use upstream's `v0.NNNN.N` scheme, as on WhatsApp.

## Resolving an upstream merge conflict by hand

```sh
git checkout homestacks
git fetch upstream --tags
git merge --no-ff vX.Y.Z
# fix conflicts: the fork's side is always one `// homestacks:` line or one of the new files above
go build -tags goolm ./... && go vet -tags goolm ./... && go test -tags goolm ./pkg/...
git commit
git push origin homestacks
git tag -a vX.Y.Z-hs.1 -m "..." && git push origin vX.Y.Z-hs.1   # publish.yml builds the image
```

If upstream changed something a feature relies on (`handleDelta`, `slidetypes.Delta` and its `Raw`,
`wrapChatResync`/`simplevent.ChatResync.CheckNeedsBackfillFunc`, `PaginateMessages`/`GetThread`,
`mediadl.ParseExpiryFromURL`, `Bridge.QueueMatrixEvent`, `matrix.ASIntent`, `commands.FullHandler`,
`dbutil` upgrade tables, `UserLoginMetadata.CopyFrom`), the build or `go vet` step will say so; fix it in
the `held*`/`freeze*` files rather than in upstream code.

## Building locally

```sh
go build -tags goolm ./...          # pure-Go olm, no libolm needed
go test -tags goolm ./pkg/...
docker build -f Dockerfile.ig -t mautrix-instagram:dev .
```
