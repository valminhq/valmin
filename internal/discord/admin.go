package discord

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/instance/control"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/store"
)

// Bounds of the minutes /restart, /stop and /backup wait for players to leave.
const (
	defaultWaitMinutes = 5
	maxWaitMinutes     = 60
)

// adminPrompts are the bot admins' commands, a subcommand named after its command, with the
// reply that asks which server to act on.
var adminPrompts = map[string]string{
	cmdRestart:       "Choose which server to restart.",
	cmdStop:          "Choose which server to stop.",
	cmdBackup:        "Choose which server to back up.",
	"update test":    "Choose which server to test updates for.",
	"update apply":   "Choose which server to install the tested updates on.",
	"update discard": "Choose the server whose update test copy to discard.",
}

// admin answers a bot admin's /restart, /stop, /backup or /update on a linked server, and
// returns the job its reply follows. Anyone else is refused.
func (b *Bot) admin(ctx context.Context, in *interaction, servers []server) (reply, *followed) {
	settings, err := b.DB.DiscordBot(ctx)
	if err != nil || settings == nil {
		return reply{tryLater, true}, nil
	}
	if !isAdmin(settings.AdminIDs, in) {
		return reply{
			"Only the bot's admins can use this command. A panel admin adds them on the panel's Discord page.",
			true,
		}, nil
	}
	name, opts := in.Data.Name, in.Data.Options
	if name == cmdUpdate && len(opts) > 0 {
		name, opts = "update "+opts[0].Name, opts[0].Options
	}
	prompt, ok := adminPrompts[name]
	if !ok {
		return reply{"Unknown command.", true}, nil
	}
	target, r := pick(servers, stringOption(opts, "server"), prompt)
	if target == nil {
		return r, nil
	}
	inst, err := b.DB.InstanceByID(ctx, target.inst.ID)
	if err != nil || inst == nil {
		return reply{tryLater, true}, nil
	}
	return b.runAdmin(ctx, in, &server{inst: inst, name: target.name}, name, opts)
}

// runAdmin carries out the admin command name, with its options opts, on s.
func (b *Bot) runAdmin(ctx context.Context, in *interaction, s *server, name string, opts []option) (reply, *followed) {
	switch name {
	case cmdRestart:
		return b.cycle(ctx, in, s, control.StepRestart, waitMinutes(opts))
	case cmdStop:
		return b.cycle(ctx, in, s, control.StepStop, waitMinutes(opts))
	case cmdBackup:
		return b.backup(ctx, in, s, waitMinutes(opts))
	case "update test":
		return b.testUpdates(ctx, in, s, opts)
	case "update apply":
		return b.applyUpdates(ctx, in, s)
	default:
		return b.discardTest(ctx, in, s)
	}
}

// waitMinutes is the wait option, the default when absent, within its bounds.
func waitMinutes(opts []option) int {
	minutes, ok := intOption(opts, "wait")
	if !ok {
		return defaultWaitMinutes
	}
	return min(max(minutes, 0), maxWaitMinutes)
}

// stepWords are how replies speak of a step that stops a running server. Each text takes the
// server's name.
type stepWords struct {
	doing, done, failed, cancelled, past, action string
}

// cycleSteps are the steps cycle and backup run, by step type.
var cycleSteps = map[string]stepWords{
	control.StepRestart: {
		"Restarting **%s**", "**%s** has restarted.", "**%s** failed to restart.", "Restarting **%s** was cancelled.",
		"restarted", "instances.restart",
	},
	control.StepStop: {
		"Stopping **%s**", "**%s** is stopped.", "**%s** failed to stop.", "Stopping **%s** was cancelled.",
		"stopped", "instances.stop",
	},
	control.StepBackup: {
		"Backing up **%s**",
		"**%s** is backed up.",
		"The backup of **%s** failed.",
		"The backup of **%s** was cancelled.",
		"backed up",
		"instances.backups.create",
	},
}

// cycle restarts, stops or backs up the running server s, as step says, all of which stop it.
// Given minutes, it warns the players first and waits up to that long for them to leave, as one
// job.
func (b *Bot) cycle(ctx context.Context, in *interaction, s *server, step string, minutes int) (reply, *followed) {
	words := cycleSteps[step]
	//nolint:exhaustive // every other state is refused below
	switch state := instance.State(s.inst.State); state {
	case instance.StateRunning:
	case instance.StateStopped:
		return reply{fmt.Sprintf("**%s** isn't running.", s.name), true}, nil
	default:
		return reply{fmt.Sprintf("**%s** can't be %s right now: %s.", s.name, words.past, stateWords(state)), true}, nil
	}

	final := control.ChainStep{Type: step}
	audit := discordAudit(in, words.action, s.inst.ID, map[string]string{"wait_minutes": strconv.Itoa(minutes)})
	text := fmt.Sprintf(words.doing, s.name) + "…"
	var job *store.Job
	var err error
	if minutes == 0 {
		job, err = b.Chainer.SubmitStep(ctx, s.inst, &final, control.StepOrigin{Audit: audit})
	} else {
		left := time.Duration(minutes) * time.Minute
		steps := []control.ChainStep{
			{Type: control.StepCommand, Command: "say " + control.PlayerWarning([]control.ChainStep{final}, left)},
			{Type: control.StepWaitFor, Condition: control.ConditionEmpty, Seconds: minutes * 60},
			final,
		}
		job, err = b.Chainer.Submit(ctx, &control.ChainSubmission{
			Instance: s.inst, Steps: steps, Kind: jobs.KindChain, From: instance.StateRunning,
			Audit: audit, Payload: control.ChainPayload{Steps: steps},
		})
		text = fmt.Sprintf(
			"%s once all players have left, or in %s at the latest… Players get an in-game warning if the server has an RCON mod.",
			fmt.Sprintf(words.doing, s.name),
			control.SpokenDuration(left),
		)
	}
	if err != nil {
		return submitRefusal(ctx, err, s.inst.ID, "**"+s.name+"**", words.past), nil
	}
	return reply{text, false}, &followed{
		jobID:     job.ID,
		done:      fmt.Sprintf(words.done, s.name),
		failed:    fmt.Sprintf(words.failed, s.name) + jobHistory,
		cancelled: fmt.Sprintf(words.cancelled, s.name),
	}
}

// jobHistory points to where a failed job's reason is, which stays in the panel because it names
// containers and paths.
const jobHistory = " The panel's job history says why."

// backup backs up the world of s: at once when it is stopped, and when it runs, which stops it
// for the backup, after warning players as cycle does.
func (b *Bot) backup(ctx context.Context, in *interaction, s *server, minutes int) (reply, *followed) {
	if instance.State(s.inst.State) == instance.StateRunning {
		return b.cycle(ctx, in, s, control.StepBackup, minutes)
	}
	if r, refused := refuseState(jobs.KindBackup, s, "backed up"); refused {
		return r, nil
	}
	job, err := b.Chainer.SubmitStep(ctx, s.inst, &control.ChainStep{Type: control.StepBackup}, control.StepOrigin{
		Audit: discordAudit(in, "instances.backups.create", s.inst.ID, nil),
	})
	if err != nil {
		return submitRefusal(ctx, err, s.inst.ID, "**"+s.name+"**", "backed up"), nil
	}
	words := cycleSteps[control.StepBackup]
	return reply{fmt.Sprintf(words.doing, s.name) + "…", false}, &followed{
		jobID:     job.ID,
		done:      fmt.Sprintf(words.done, s.name),
		failed:    fmt.Sprintf(words.failed, s.name) + jobHistory,
		cancelled: fmt.Sprintf(words.cancelled, s.name),
	}
}

// testUpdates tries the game update, the mod updates or both, as the what option says, on a copy
// of s. With apply, the test applies them when every mod loaded and nobody is on.
func (b *Bot) testUpdates(ctx context.Context, in *interaction, s *server, opts []option) (reply, *followed) {
	if r, refused := refuseState(jobs.KindUpdateTest, s, "tested"); refused {
		return r, nil
	}
	what := stringOption(opts, "what")
	game, mods := what != "mods", what != "game"
	apply := boolOption(opts, "apply")
	job, err := b.Updates.Submit(ctx, &control.UpdateTestSubmission{
		Instance: s.inst, Game: game, Mods: mods, Apply: apply,
		Audit: discordAudit(in, "instances.update_test.start", s.inst.ID, map[string]string{
			"game": strconv.FormatBool(game), "mods": strconv.FormatBool(mods), "apply": strconv.FormatBool(apply),
		}),
	})
	if err != nil {
		return updateRefusal(ctx, err, s, "tested"), nil
	}
	tested := "game and mod"
	if !mods {
		tested = "game"
	} else if !game {
		tested = "mod"
	}
	return reply{fmt.Sprintf("Testing %s updates for **%s** on a copy…", tested, s.name), false}, &followed{
		jobID:     job.ID,
		then:      func(ctx context.Context) string { return b.testReport(ctx, s, apply) },
		failed:    fmt.Sprintf("The update test of **%s** failed.", s.name) + jobHistory,
		cancelled: fmt.Sprintf("The update test of **%s** was cancelled.", s.name),
	}
}

// testReport says what the update test copy of s runs that s does not, how its mods loaded, and
// what happens next.
func (b *Bot) testReport(ctx context.Context, s *server, apply bool) string {
	text := fmt.Sprintf("The update test of **%s** finished.", s.name)
	testCopy, err := b.DB.InstanceTestCopy(ctx, s.inst.ID)
	if err != nil || testCopy == nil {
		return text + " The copy could not be checked; the panel shows the result."
	}
	if changes, err := b.Updates.Changes(ctx, s.inst, testCopy); err == nil {
		if summary := changeSummary(&changes); summary != "" {
			text += " " + summary
		} else {
			return text + " The copy runs nothing newer than the server. `/update discard` removes it."
		}
	}
	verdict, loaded := b.verdict(ctx, testCopy)
	text += " " + verdict
	switch {
	case apply && loaded:
		return text + " The updates are being applied if nobody is playing; otherwise `/update apply` installs them later."
	case apply:
		return text + " The updates were not applied. `/update apply` installs them anyway, or `/update discard` removes the copy."
	case loaded:
		return text + " `/update apply` installs them."
	}
	return text + " `/update apply` installs them anyway, or `/update discard` removes the copy."
}

// changeSummary names what the copy runs newer than its server, "" when nothing is newer.
func changeSummary(c *control.TestChanges) string {
	var parts []string
	if c.GameNewer() {
		parts = append(parts, fmt.Sprintf("Game build %s → %s.", c.SourceBuildID, c.CopyBuildID))
	}
	if n := len(c.Mods); n > 0 {
		parts = append(parts, fmt.Sprintf("%d %s updated.", n, plural(n, "mod", "mods")))
	}
	return strings.Join(parts, " ")
}

// verdict describes how the mods loaded on testCopy, with each mod name in code so Discord
// leaves it as written, and reports whether every one did. It never quotes an error, which can
// name paths.
func (b *Bot) verdict(ctx context.Context, testCopy *store.Instance) (string, bool) {
	v, err := b.Updates.Verify(ctx, testCopy)
	switch {
	case err != nil:
		return "Its mods could not be checked.", false
	case len(v.Problems()) == 0:
		return "Every mod loaded.", true
	case v.Load == nil:
		return "No mod log was found, so no mod was checked.", false
	}
	text := "Not every mod loaded."
	if !v.Load.Complete {
		text += " The mod loader had not finished."
	}
	failed := make([]string, 0, len(v.Load.Failed))
	for _, f := range v.Load.Failed {
		failed = append(failed, f.Name)
	}
	if len(failed) > 0 {
		text += " Failed: " + codeList(failed) + "."
	}
	if v.Load.Complete && len(v.NotSeen) > 0 {
		text += " Not mentioned in the mod log: " + codeList(v.NotSeen) + "."
	}
	return text, false
}

// codeList joins names, each in inline code.
func codeList(names []string) string {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = fmt.Sprintf("%#q", n)
	}
	return strings.Join(quoted, ", ")
}

// applyUpdates installs what the update test copy of s runs onto s and deletes the copy.
func (b *Bot) applyUpdates(ctx context.Context, in *interaction, s *server) (reply, *followed) {
	if r, refused := refuseState(jobs.KindUpdateApply, s, "updated"); refused {
		return r, nil
	}
	job, err := b.Updates.SubmitApply(ctx, &control.UpdateApplySubmission{
		Instance: s.inst, Audit: discordAudit(in, "instances.update_test.apply", s.inst.ID, nil),
	})
	if err != nil {
		return updateRefusal(ctx, err, s, "updated"), nil
	}
	return reply{fmt.Sprintf("Installing the tested updates on **%s**…", s.name), false}, &followed{
		jobID:     job.ID,
		done:      fmt.Sprintf("**%s** is updated.", s.name),
		failed:    fmt.Sprintf("Updating **%s** failed.", s.name) + jobHistory,
		cancelled: fmt.Sprintf("Updating **%s** was cancelled.", s.name),
	}
}

// discardTest deletes the update test copy of s with its worlds, stopping it first when it runs.
func (b *Bot) discardTest(ctx context.Context, in *interaction, s *server) (reply, *followed) {
	testCopy, err := b.DB.InstanceTestCopy(ctx, s.inst.ID)
	if err != nil {
		return reply{tryLater, true}, nil
	}
	if testCopy == nil {
		return reply{fmt.Sprintf("**%s** has no update test copy.", s.name), true}, nil
	}
	subject := fmt.Sprintf("The update test copy of **%s**", s.name)
	discard := &followed{
		done:      subject + " is discarded.",
		failed:    "Discarding the update test copy of **" + s.name + "** failed." + jobHistory,
		cancelled: "Discarding the update test copy of **" + s.name + "** was cancelled.",
	}
	//nolint:exhaustive // every other state is refused below
	switch state := instance.State(testCopy.State); state {
	case instance.StateStopped, instance.StateError:
		job, err := b.deleteCopy(ctx, in, testCopy)
		if err != nil {
			return submitRefusal(ctx, err, testCopy.ID, subject, "discarded"), nil
		}
		discard.jobID = job.ID
		return reply{fmt.Sprintf("Discarding the update test copy of **%s**…", s.name), false}, discard
	case instance.StateRunning:
		job, err := b.Chainer.SubmitStep(ctx, testCopy, &control.ChainStep{Type: control.StepStop}, control.StepOrigin{
			Audit: discordAudit(in, "instances.stop", testCopy.ID, nil),
		})
		if err != nil {
			return submitRefusal(ctx, err, testCopy.ID, subject, "stopped"), nil
		}
		return reply{fmt.Sprintf("Stopping the update test copy of **%s** to discard it…", s.name), false}, &followed{
			jobID:     job.ID,
			then:      func(ctx context.Context) string { return b.discardStopped(ctx, in, testCopy.ID, subject, discard) },
			failed:    subject + " could not be stopped." + jobHistory,
			cancelled: discard.cancelled,
		}
	default:
		return reply{fmt.Sprintf("%s can't be discarded right now: %s.", subject, stateWords(state)), true}, nil
	}
}

// discardStopped deletes the test copy id once its stop has finished, waits for the delete, and
// returns what discard says about how it ended. subject names the copy.
func (b *Bot) discardStopped(ctx context.Context, in *interaction, id, subject string, discard *followed) string {
	testCopy, err := b.DB.InstanceByID(ctx, id)
	if err != nil || testCopy == nil {
		return tryLater
	}
	job, err := b.deleteCopy(ctx, in, testCopy)
	if err != nil {
		return submitRefusal(ctx, err, testCopy.ID, subject, "discarded").content
	}
	status, ok := b.await(ctx, job.ID)
	switch {
	case !ok:
		return subject + " is still being discarded. The panel shows how it ends."
	case status == jobs.StatusSucceeded:
		return discard.done
	case status == jobs.StatusFailed:
		return discard.failed
	}
	return discard.cancelled
}

// deleteCopy submits the delete of testCopy with its worlds.
func (b *Bot) deleteCopy(ctx context.Context, in *interaction, testCopy *store.Instance) (*store.Job, error) {
	job, err := b.Deleter.Submit(ctx, &control.DeleteSubmission{
		Instance: testCopy,
		Audit:    discordAudit(in, "instances.delete", testCopy.ID, map[string]string{"keep_worlds": "false"}),
	})
	if err != nil {
		return nil, fmt.Errorf("delete update test copy %s: %w", testCopy.ID, err)
	}
	return job, nil
}

// refuseState is the reply refusing s when kind cannot run from its state, and reports whether
// it refused. past says what s would have been.
func refuseState(kind jobs.Kind, s *server, past string) (reply, bool) {
	state := instance.State(s.inst.State)
	if slices.Contains(instance.AllowedFrom(kind), state) {
		return reply{}, false
	}
	return reply{fmt.Sprintf("**%s** can't be %s right now: %s.", s.name, past, stateWords(state)), true}, true
}

// stateWords says what state a server is in, as a clause that follows its name.
func stateWords(state instance.State) string {
	if state == instance.StateError {
		return "it is in an error state, which the panel shows"
	}
	return "it is " + string(state)
}

// updateRefusal is the reply to an update test or apply of s that was refused.
func updateRefusal(ctx context.Context, err error, s *server, past string) reply {
	var exists *control.UpdateTestExistsError
	var lowMemory *control.LowMemoryError
	switch {
	case errors.As(err, &exists):
		return reply{fmt.Sprintf(
			"**%s** already has an update test copy. Use `/update apply` or `/update discard` first.", s.name,
		), true}
	case errors.Is(err, control.ErrNestedUpdateTest):
		return reply{fmt.Sprintf("**%s** is an update test copy itself.", s.name), true}
	case errors.Is(err, control.ErrNoUpdateTest):
		return reply{fmt.Sprintf("**%s** has no update test copy. `/update test` makes one.", s.name), true}
	case errors.Is(err, control.ErrUpdateTestUnsettled):
		return reply{
			fmt.Sprintf(
				"The update test copy of **%s** isn't ready to install. Wait for its test to finish, or discard it and test again.",
				s.name,
			),
			true,
		}
	case errors.As(err, &lowMemory):
		return reply{
			fmt.Sprintf(
				"There isn't enough free memory to test **%s** on a copy: the host has %d MB free and the copy may need %d MB. "+
					"Stop another server and try again.",
				s.name,
				lowMemory.AvailableMB,
				lowMemory.NeededMB,
			),
			true,
		}
	}
	return submitRefusal(ctx, err, s.inst.ID, "**"+s.name+"**", past)
}
