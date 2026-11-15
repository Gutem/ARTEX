package agent

import (
	"context"
	"errors"
	"fmt"
)

// AbortCause names why an agent run's context was cancelled. Every cancellation
// site should attach one so the activity trace can report the real initiator.
type AbortCause struct {
	Code  string
	Short string
	Text  string
}

func (c *AbortCause) Error() string { return c.Text }

func cause(code, short, text string) *AbortCause {
	return &AbortCause{Code: code, Short: short, Text: text}
}

// Causef builds a cause that includes runtime-specific detail.
func Causef(code, short, format string, args ...any) *AbortCause {
	return &AbortCause{Code: code, Short: short, Text: fmt.Sprintf(format, args...)}
}

var (
	// Task-level execution context.
	AbortPausedBySchedule = cause("paused_by_schedule", "planning window closed, task paused", "the task resumes in the next planning window; this run has stopped")
	AbortPausedByProxy    = cause("paused_by_proxy", "global proxy failure, task paused", "the global egress proxy could not be reached and the task is paused; it resumes automatically once the proxy recovers")
	AbortPausedByUser     = cause("paused_by_user", "the user paused the task",
		"the user paused the task through the task control endpoint (POST /api/tasks/{id}/control, action=pause). This Planner/Worker run was cancelled deliberately; intents that were running return to the frontier (open) and, once the task resumes, are claimed again and executed from the start")
	AbortPausedByOrchestrator = cause("paused_by_orchestrator", "the orchestration agent paused the task",
		"the orchestration agent paused this task with the pause_task tool. This Planner/Worker run was cancelled deliberately; intents that were running return to the frontier (open) and execute again after resume")
	AbortTaskDeleted = cause("task_deleted", "the task was deleted",
		"the task is being deleted (DELETE /api/tasks/{id}) and the deletion barrier cancelled its running Planner, Worker and main agent; the result of this run will not be used")
	AbortPausedOnReload = cause("paused_on_reload", "task pause restored on reload",
		"on startup the backend restored the task's pause from the state persisted in the database. This run was cancelled; normally no agent is running during the restore phase")
	AbortGoalMet = cause("goal_met", "planner judged goals met",
		"the planner judged the task objectives met and marked the task done, then cancelled the Workers still running; those intents are marked stopped, not failed")
	AbortNoResultStop = cause("no_result_stop", "no-result stop grace expired",
		"the task reached the no-result checkpoint without meeting the finding count and severity requirements, so it was hard-cancelled after a 90-second wrap-up grace period; intents are marked exhausted and the facts and assets already written are kept")
	AbortSettleDrainTimeout = cause("settle_drain_timeout", "timeout wrap-up grace expired",
		"after the task hit its timeout it waited for the running Worker to wrap up gracefully, but the 90-second drain grace was still not enough, so it was hard-cancelled; intents are marked exhausted and the facts and assets written during wrap-up are kept")

	// Per-work context.
	AbortKilledByPlanner = cause("killed_by_planner", "the planner terminated this intent",
		"the planner terminated this intent deliberately with kill_work, usually meaning the direction went off track or is no longer worth pursuing; the intent is marked stopped and is not reclaimed automatically")
	AbortWorkPausedByUser = cause("work_paused_by_user", "the user paused this Worker intent",
		"the user paused the running Worker. This run was cancelled and the intent moved to paused; the intents, facts, findings and activity already recorded are all kept, and execution restarts from the beginning after resume")
	AbortWorkCancelledByUser = cause("work_cancelled_by_user", "the user deleted this Worker intent",
		"the user deleted the running Worker. This run was cancelled; once the Worker leaves its write section the server applies the deletion mode the user chose — a soft delete only marks the intent as deleted and keeps all its output, while a hard delete cascades to the intent and to downstream nodes only it supported")
	AbortWorkFinished = cause("work_finished", "Worker finished normally",
		"the Worker finished normally and the engine released its context resources in detachWork. This is not an interruption; if it shows up in an interruption message, a cancellation and a wrap-up event raced")
	AbortPausedRaceGuard = cause("paused_race_guard", "start refused: task paused",
		"while the task is paused the engine refuses to hand out a new execution context, preventing a race between claim and pause from starting another Worker; intents already claimed return to the frontier")

	// Main Agent and standalone conversation contexts.
	AbortChatStoppedByUser = cause("chat_stopped_by_user", "the user stopped this conversation turn",
		"the user pressed stop, deliberately aborting this run of the main agent or session agent. Activity already recorded is kept and the next message can be sent")
	AbortChatPausedWithTask = cause("chat_paused_with_task", "task paused, chat aborted",
		"pausing the task also cancelled the running main agent conversation. Activity already recorded is kept; after resuming, this turn is not replayed automatically")
	AbortChatTurnFinished = cause("chat_turn_finished", "chat turn finished",
		"this conversation turn ended normally and the server is releasing its context resources. This is not an interruption; if it shows up in an interruption message, a cancellation and a wrap-up event raced")

	// Process-level and per-run hard backstop.
	AbortShutdown = cause("shutdown", "the backend process is shutting down",
		"the backend received SIGINT or SIGTERM and is restarting, updating or shutting down. Every running agent is cancelled; after the restart, intents left in running are reset to open and execute again")
	AbortRunHardTimeout = cause("run_hard_timeout", "run hard timeout fired",
		"a single run exceeded its soft wall-clock budget plus the extra grace, meaning a model request or a tool did not return for a long time and the normal turn-boundary wrap-up could not run. Check above all the last tool call that never returned before the interruption")
)

// AbortReason resolves the named cause attached to a cancelled run context.
func AbortReason(ctx context.Context) (code, short, text string, ok bool) {
	c := context.Cause(ctx)
	if c == nil {
		return "", "", "", false
	}
	var ac *AbortCause
	if errors.As(c, &ac) {
		return ac.Code, ac.Short, ac.Text, true
	}
	switch {
	case errors.Is(c, context.DeadlineExceeded):
		return "deadline_exceeded", "the upstream context reached its deadline",
			"the upstream context reached its deadline, but whoever set it did not attach a named cause through WithTimeoutCause: " + c.Error(), true
	case errors.Is(c, context.Canceled):
		return "canceled_no_cause", "the canceller attached no named cause",
			"the upstream context was cancelled but the canceller attached no named cause through context.WithCancelCause; register the cause in agent/cancelcause.go and wire it into that cancellation point", true
	default:
		return "other", firstLine(c.Error(), 80), c.Error(), true
	}
}
