package discord

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/valminhq/valmin/internal/scheduler"
	"github.com/valminhq/valmin/internal/store"
)

// shutdown answers /shutdown, which plans, lists or cancels a power cut. A power cut stops every
// game server on the panel, so only the bot's admins may use it.
func (b *Bot) shutdown(ctx context.Context, in *interaction) reply {
	settings, err := b.DB.DiscordBot(ctx)
	if err != nil || settings == nil {
		return reply{tryLater, true}
	}
	if !isAdmin(settings.AdminIDs, in) {
		return reply{
			"Only the bot's admins can plan power cuts. A panel admin adds them on the panel's Discord page.",
			true,
		}
	}
	if len(in.Data.Options) == 0 {
		return reply{"Unknown command.", true}
	}
	sub := in.Data.Options[0]
	switch sub.Name {
	case "add":
		return b.planShutdown(ctx, in, settings.Timezone, stringOption(sub.Options, "time"))
	case "list":
		return b.listShutdowns(ctx)
	case "cancel":
		return b.cancelShutdown(ctx, in, settings.Timezone, stringOption(sub.Options, "time"))
	default:
		return reply{"Unknown command.", true}
	}
}

// isAdmin reports whether the member behind in is one of ids, or holds a role that is.
func isAdmin(ids []string, in *interaction) bool {
	if in.Member == nil {
		return false
	}
	if slices.Contains(ids, in.Member.User.ID) {
		return true
	}
	return slices.ContainsFunc(in.Member.Roles, func(role string) bool { return slices.Contains(ids, role) })
}

// planShutdown stores a power cut at the time text names in zone.
func (b *Bot) planShutdown(ctx context.Context, in *interaction, zone, text string) reply {
	at, err := scheduler.PowerOffTime(text, zone, time.Now().UTC())
	switch {
	case errors.Is(err, scheduler.ErrPastPowerOffTime):
		return reply{"That time has already passed.", true}
	case err != nil:
		return reply{fmt.Sprintf("Type the time the power goes off as 14:00 or 2026-10-09 14:00 (%s).", zone), true}
	}
	row := &store.PlannedShutdown{ID: store.NewID(), PowerOffAt: at, CreatedByName: actorName(in)}
	audit := discordAudit(in, "shutdowns.create", "", map[string]string{"power_off_at": store.FormatTime(at)})
	if err := b.DB.CreatePlannedShutdown(ctx, row, audit); err != nil {
		slog.WarnContext(ctx, "discord power cut not planned", slog.Any("error", err))
		return reply{tryLater, true}
	}
	return reply{
		fmt.Sprintf(
			"⚡ Power cut planned for %s (%s). Every game server stops at %s, and players online are warned %d minutes before.",
			stamp(at, "F"),
			stamp(at, "R"),
			stamp(at.Add(-scheduler.ShutdownStopLead), "t"),
			int((scheduler.ShutdownWarnLead-scheduler.ShutdownStopLead)/time.Minute),
		),
		false,
	}
}

// listShutdowns lists the planned power cuts.
func (b *Bot) listShutdowns(ctx context.Context) reply {
	planned, err := b.DB.UpcomingShutdowns(ctx, time.Now().UTC())
	if err != nil {
		return reply{tryLater, true}
	}
	if len(planned) == 0 {
		return reply{"No power cuts are planned.", true}
	}
	var sb strings.Builder
	sb.WriteString("Planned power cuts:")
	for _, p := range planned {
		fmt.Fprintf(&sb, "\n⚡ %s (%s)", stamp(p.PowerOffAt, "F"), stamp(p.PowerOffAt, "R"))
	}
	return reply{sb.String(), true}
}

// cancelShutdown removes the planned power cut chosen by id, or named by its time in zone.
func (b *Bot) cancelShutdown(ctx context.Context, in *interaction, zone, chosen string) reply {
	planned, err := b.DB.UpcomingShutdowns(ctx, time.Now().UTC())
	if err != nil {
		return reply{tryLater, true}
	}
	typed, _ := scheduler.PowerOffTime(chosen, zone, time.Now().UTC())
	i := slices.IndexFunc(planned, func(p store.PlannedShutdown) bool {
		return p.ID == chosen || p.PowerOffAt.Equal(typed)
	})
	if i < 0 {
		return reply{"No planned power cut matches that. /shutdown list shows them.", true}
	}
	p := planned[i]
	audit := discordAudit(in, "shutdowns.delete", "", map[string]string{"id": p.ID})
	if _, err := b.DB.DeletePlannedShutdown(ctx, p.ID, audit); err != nil {
		slog.WarnContext(ctx, "discord power cut not cancelled", slog.Any("error", err))
		return reply{tryLater, true}
	}
	return reply{fmt.Sprintf("The power cut planned for %s is cancelled.", stamp(p.PowerOffAt, "F")), false}
}

// shutdownChoices offers the planned power cuts to an admin cancelling one, labelled in the
// bot's time zone.
func (b *Bot) shutdownChoices(ctx context.Context, in *interaction) []map[string]string {
	out := []map[string]string{}
	settings, err := b.DB.DiscordBot(ctx)
	if err != nil || settings == nil || !isAdmin(settings.AdminIDs, in) {
		return out
	}
	planned, err := b.DB.UpcomingShutdowns(ctx, time.Now().UTC())
	if err != nil {
		return out
	}
	loc, err := time.LoadLocation(settings.Timezone)
	if err != nil {
		loc = time.UTC
	}
	for _, p := range planned {
		if len(out) == maxChoices {
			break
		}
		out = append(out, map[string]string{
			"name": p.PowerOffAt.In(loc).Format("Mon 2 Jan 15:04 MST"), "value": p.ID,
		})
	}
	return out
}

// stamp is a Discord timestamp, which every reader sees in their own time zone.
func stamp(t time.Time, style string) string {
	return fmt.Sprintf("<t:%d:%s>", t.Unix(), style)
}
