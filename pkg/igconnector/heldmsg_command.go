// mautrix-meta - A Matrix-Facebook Messenger and Instagram DM puppeting bridge.
// Copyright (C) 2026 Tulir Asokan
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package igconnector

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/commands"
	"maunium.net/go/mautrix/event"

	"go.mau.fi/mautrix-meta/pkg/metaid"
)

// cmdSchedule, cmdOfflineHours and cmdTimezone manage the homestacks held
// messages (see heldmsg.go and HOMESTACKS.md).
var cmdSchedule = &commands.FullHandler{
	Func: fnSchedule,
	Name: "schedule",
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionGeneral,
		Description: "Send your next message in this chat (or the given text) at a later time.",
		Args:        "<_time_ [_text_]|list|now [_n_|all]|cancel [_n_|all]>",
	},
	RequiresPortal: true,
	RequiresLogin:  true,
}

var cmdOfflineHours = &commands.FullHandler{
	Func: fnOfflineHours,
	Name: "offline-hours",
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionGeneral,
		Description: "Hold messages you send in this chat during these hours until they end.",
		Args:        "<show|off|_start_-_end_>",
	},
	RequiresPortal: true,
	RequiresLogin:  true,
}

var cmdTimezone = &commands.FullHandler{
	Func: fnTimezone,
	Name: "timezone",
	Help: commands.HelpMeta{
		Section:     commands.HelpSectionGeneral,
		Description: "Show or set the timezone used by schedule and offline-hours.",
		Args:        "[_zone_, e.g. Europe/London]",
	},
	RequiresLogin: true,
}

const scheduleUsage = "Usage: `$cmdprefix schedule <time> [text]`\n\n" +
	"* `time` is a time of day (`18:30`, `6:30pm`, `7am`) — the next time the clock reads it — or a delay (`+45m`, `+2h30m`, `in 2h`), at most 24 hours.\n" +
	"* With `text`, that text is sent then. Without, the next message you send here within 10 minutes is held until then — any kind, including media.\n" +
	"* `list` shows held messages here, `now [n|all]` sends them now, `cancel [n|all]` drops them. Deleting a held message also cancels it."

const offlineHoursUsage = "Usage: `$cmdprefix offline-hours <show|off|start-end>`, e.g. `22:00-08:00` or `10pm-8am`.\n\n" +
	"Messages you send here during those hours are held and sent when they end, in order. " +
	"`$cmdprefix schedule list|now|cancel` manage them. Times are in your `$cmdprefix timezone`."

func preferredLogin(ce *commands.Event) *bridgev2.UserLogin {
	login, _, err := ce.Portal.FindPreferredLogin(ce.Ctx, ce.User, false)
	if err != nil || login == nil {
		ce.Reply("You're not logged in to the Instagram account of this chat.")
		return nil
	}
	return login
}

// heldPreview is a short description of a held message for `schedule list`.
func heldPreview(h *heldMessage) string {
	evt, err := decodeHeldEvent(h.Event)
	if err != nil {
		return "(unreadable)"
	}
	msg := evt.Content.AsMessage()
	if msg.MsgType != event.MsgText && msg.MsgType != event.MsgEmote && msg.MsgType != event.MsgNotice {
		return "(" + strings.TrimPrefix(string(msg.MsgType), "m.") + ")"
	}
	body := strings.Join(strings.Fields(msg.Body), " ")
	if r := []rune(body); len(r) > 40 {
		body = string(r[:40]) + "…"
	}
	return fmt.Sprintf("%q", body)
}

// selectHeld picks `all`, nothing (= all), or the 1-based index n from list.
func selectHeld(list []*heldMessage, args []string) ([]*heldMessage, error) {
	if len(args) == 0 || strings.EqualFold(args[0], "all") {
		return list, nil
	}
	n, err := strconv.Atoi(args[0])
	if err != nil || n < 1 || n > len(list) {
		return nil, fmt.Errorf("pick a number from `$cmdprefix schedule list` (1–%d) or `all`", len(list))
	}
	return list[n-1 : n], nil
}

func fnSchedule(ce *commands.Event) {
	if heldStore == nil {
		ce.Reply("Held messages are unavailable: the bridge failed to set them up.")
		return
	}
	login := preferredLogin(ce)
	if login == nil {
		return
	}
	wc := ce.Bridge.Network.(*IGConnector)
	now := time.Now().In(userLocation(login))
	sub := ""
	if len(ce.Args) > 0 {
		sub = strings.ToLower(ce.Args[0])
	}
	switch sub {
	case "":
		ce.Reply(scheduleUsage)
	case "list":
		list, err := heldStore.listRoom(ce.Ctx, ce.Portal.MXID)
		if err != nil {
			ce.Reply("Failed to read held messages: %v", err)
			return
		}
		var lines []string
		if at, ok := held.peekArmed(ce.Portal.MXID, ce.User.MXID, now); ok {
			lines = append(lines, fmt.Sprintf("Your next message will be held until %s.", formatSendAt(at.In(now.Location()), now)))
		}
		for i, h := range list {
			lines = append(lines, fmt.Sprintf("%d. %s · %s · %s", i+1, formatSendAt(h.SendAt.In(now.Location()), now), h.Reason, heldPreview(h)))
		}
		if len(lines) == 0 {
			lines = append(lines, "Nothing held in this chat.")
		}
		ce.Reply(strings.Join(lines, "\n"))
	case "now", "cancel":
		list, err := heldStore.listRoom(ce.Ctx, ce.Portal.MXID)
		if err != nil {
			ce.Reply("Failed to read held messages: %v", err)
			return
		}
		if sub == "cancel" && len(ce.Args) == 1 {
			if _, armed := held.takeArmed(ce.Portal.MXID, ce.User.MXID, now); armed {
				ce.Reply("Your next message will be sent normally.")
				return
			}
		}
		if len(list) == 0 {
			ce.Reply("Nothing held in this chat.")
			return
		}
		picked, err := selectHeld(list, ce.Args[1:])
		if err != nil {
			ce.Reply(err.Error())
			return
		}
		if sub == "now" {
			if ready, _ := wc.heldLoginReady(ce.Ctx, picked[0]); !ready {
				ce.Reply("Instagram isn't connected; held messages will go out once it is.")
				return
			}
			// Not sent from here: the release loop sends them paced, so
			// `now all` on a long backlog doesn't burst.
			for _, h := range picked {
				if err = heldStore.makeDue(ce.Ctx, h.EventID, time.Now()); err != nil {
					ce.Reply("Failed to release: %v", err)
					return
				}
			}
			ce.Reply("Sending %d held message(s) now, a few at a time (within about %s).",
				len(picked), (heldReleaseInterval * time.Duration(1+len(picked)/heldReleaseBatch)).Round(time.Second))
			return
		}
		for _, h := range picked {
			if err = heldStore.delete(ce.Ctx, h.EventID); err != nil {
				ce.Reply("Failed to cancel: %v", err)
				return
			}
			wc.redactHeldReaction(ce.Ctx, h)
		}
		ce.Reply("Cancelled %d held message(s). They stay in this room but won't be sent to Instagram.", len(picked))
	default:
		args := ce.Args
		if sub == "in" && len(args) > 1 {
			args = append([]string{"+" + args[1]}, args[2:]...)
		}
		sendAt, err := parseSendTime(args[0], now)
		if err != nil {
			ce.Reply("%v\n\n%s", err, scheduleUsage)
			return
		}
		when := formatSendAt(sendAt, now)
		text := strings.TrimSpace(strings.Join(args[1:], " "))
		if text == "" {
			held.arm(ce.Portal.MXID, ce.User.MXID, sendAt, now)
			ce.Reply("Your next message here will be sent at %s. (`$cmdprefix schedule cancel` to undo.)", when)
			return
		}
		// The command event itself becomes the message, so the Instagram
		// message maps to it and deleting the command cancels it.
		content := &event.MessageEventContent{MsgType: event.MsgText, Body: text}
		if ce.ReplyTo != "" {
			content.RelatesTo = (&event.RelatesTo{}).SetReplyTo(ce.ReplyTo)
		}
		evt := &event.Event{
			Type: event.EventMessage, ID: ce.EventID, RoomID: ce.Portal.MXID, Sender: ce.User.MXID,
			Timestamp: now.UnixMilli(), Content: event.Content{Parsed: content},
		}
		if err = wc.holdEvent(ce.Ctx, login.ID, evt, heldReasonSchedule, sendAt); err != nil {
			ce.Reply("Failed to schedule: %v", err)
			return
		}
		ce.Reply("Scheduled for %s.", when)
	}
}

func fnOfflineHours(ce *commands.Event) {
	meta, ok := ce.Portal.Metadata.(*metaid.PortalMetadata)
	if !ok {
		ce.Reply("Unexpected portal metadata")
		return
	}
	login := preferredLogin(ce)
	if login == nil {
		return
	}
	loc := userLocation(login)
	sub := "show"
	if len(ce.Args) > 0 {
		sub = strings.ToLower(ce.Args[0])
	}
	switch sub {
	case "show":
		if meta.OfflineHours == nil {
			ce.Reply("No offline hours in this chat.")
			return
		}
		state := "online now"
		now := time.Now().In(loc)
		if until, inside := offlineUntil(meta.OfflineHours, now); inside {
			state = "offline now, until " + formatSendAt(until, now)
		}
		ce.Reply("Offline hours here: %s %s (%s).", describeOfflineHours(meta.OfflineHours), loc, state)
	case "off":
		meta.OfflineHours = nil
		if err := ce.Portal.Save(ce.Ctx); err != nil {
			ce.Reply("Failed to save: %v", err)
			return
		}
		ce.Reply("Offline hours removed for this chat. Messages already held still go out as planned (`$cmdprefix schedule list`).")
	default:
		oh, err := parseOfflineHours(strings.Join(ce.Args, ""))
		if err != nil {
			ce.Reply("%v\n\n%s", err, offlineHoursUsage)
			return
		}
		meta.OfflineHours = oh
		if err = ce.Portal.Save(ce.Ctx); err != nil {
			ce.Reply("Failed to save: %v", err)
			return
		}
		ce.Reply("Offline hours set for this chat: %s %s.", describeOfflineHours(oh), loc)
	}
}

func fnTimezone(ce *commands.Event) {
	logins := ce.User.GetUserLogins()
	if len(ce.Args) == 0 {
		var lines []string
		for _, login := range logins {
			lines = append(lines, fmt.Sprintf("* %s: %s", login.RemoteName, userLocation(login)))
		}
		ce.Reply("Timezone used by `schedule` and `offline-hours`:\n%s\n\nSet it with `$cmdprefix timezone <zone>`, e.g. `Europe/London`.",
			strings.Join(lines, "\n"))
		return
	}
	loc, err := time.LoadLocation(ce.Args[0])
	if err != nil || ce.Args[0] == "" || strings.EqualFold(ce.Args[0], "local") {
		ce.Reply("Unknown timezone %q. Use an IANA name like `Europe/London` or `Asia/Kolkata`.", ce.Args[0])
		return
	}
	for _, login := range logins {
		meta, ok := login.Metadata.(*metaid.UserLoginMetadata)
		if !ok {
			continue
		}
		meta.Timezone = loc.String()
		if err = login.Save(ce.Ctx); err != nil {
			ce.Reply("Failed to save for %s: %v", login.RemoteName, err)
			return
		}
	}
	ce.Reply("Timezone set to %s for all your Instagram logins. It's %s there now.", loc, time.Now().In(loc).Format("15:04"))
}
