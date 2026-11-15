package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// Planner is the event-driven LLM planner (docs §4.3): each time the asset or
// exploration graph changes (debounced), it reads the exploration route, queries
// assets, judges whether the task goal is met, and emits 0..N exploration intents
// into the frontier. It is the sole intent generator.
type Planner struct {
	findingRecorder   FindingRecorder
	prov              llm.Provider
	model             string
	tx                *transcript.Store                      // raw LLM conversation persistence (nil = off)
	window            int                                    // context window in tokens (for compaction)
	windowFn          func() int                             // optional dynamic task-chain minimum
	maxTurns          int                                    // max agent turns per run (0 = unlimited)
	killWork          func(intentID int64) error             // engine callback to terminate a running work (nil = off)
	steerWork         func(intentID int64, msg string) error // engine callback to steer a running work mid-run (nil = off)
	proxyAddr         string                                 // recording proxy for WebFetch (empty = direct)
	proxyAddrFn       func() string
	proxyCACert       string        // recording proxy's CA cert path (HTTPS verify)
	webSearch         WebSearchOpts // web_search tool backend selection (off by default)
	workDir           string        // shared work dir (surfaced in prompt as artifact-output target)
	injectConstraints func() bool   // resolver: inject task operation constraints into system prompt? (nil = yes)
	nonStreamingFn    func() bool   // resolver: use non-streaming (Complete) path? (nil = streaming)
	noaEnabledFn      func() bool   // resolver: use experimental noa compaction? (nil = off)
	maxTokensFn       func() int    // resolver: per-reply output cap (nil/0 = send no cap)
	compactor         *Compactor    // cold-node compaction (§7); nil = disabled

	// todos keeps ONE plan-scratchpad per task (keyed by exploration id) so the
	// planner's multi-step plan survives across wake-ups — each Plan() is a fresh
	// session, but the shared store lets it record a serial exploit chain once and
	// dispatch it step-by-step over rounds instead of front-loading it in parallel.
	todoMu sync.Mutex
	todos  map[int64]*actool.TodoStore
}

func NewPlanner(prov llm.Provider, model, workDir string, tx *transcript.Store, window, maxTurns int) *Planner {
	return &Planner{prov: prov, model: model, workDir: workDir, tx: tx, window: window, maxTurns: maxTurns, todos: map[int64]*actool.TodoStore{}}
}

func (p *Planner) SetCompactionWindowResolver(fn func() int) { p.windowFn = fn }

// SetCompactor wires the cold-node compactor (cold-digest §7). Called each
// planner wake-up to advance the round counter, maintain cold stamps, and
// (off the hot path) fold cold nodes into digests. nil = feature disabled.
func (p *Planner) SetCompactor(c *Compactor) { p.compactor = c }

// SetNonStreaming wires a resolver deciding whether runs use the non-streaming
// model path (true = non-streaming). nil/unset = streaming (default).
func (p *Planner) SetNonStreaming(fn func() bool) { p.nonStreamingFn = fn }

func (p *Planner) nonStreaming() bool { return p.nonStreamingFn != nil && p.nonStreamingFn() }

// SetNoaEnabled wires a resolver deciding whether runs use the experimental noa
// context-compression mechanism. nil/unset = off (built-in compaction). Read per
// run so the settings toggle takes effect without rebuilding the agent.
func (p *Planner) SetNoaEnabled(fn func() bool) { p.noaEnabledFn = fn }

// SetMaxTokens wires a resolver for the per-reply output cap. nil/unset or 0 =
// send no cap and let the endpoint decide. Read per run, like nonStreaming.
func (p *Planner) SetMaxTokens(fn func() int) { p.maxTokensFn = fn }

func (p *Planner) maxTokens() int {
	if p.maxTokensFn == nil {
		return 0
	}
	return p.maxTokensFn()
}

func (p *Planner) compactionWindow() int {
	if p.windowFn != nil {
		return p.windowFn()
	}
	return p.window
}

// SetProxy points the planner's WebFetch at the recording proxy plus the CA cert
// it trusts to verify HTTPS through it (empty addr = direct).
func (p *Planner) SetProxy(addr, caCert string)      { p.proxyAddr, p.proxyCACert = addr, caCert }
func (p *Planner) SetProxyProvider(fn func() string) { p.proxyAddrFn = fn }

// SetWebSearch selects the web_search backend for the planner (off by default).
func (p *Planner) SetWebSearch(o WebSearchOpts) { p.webSearch = o }

// SetConstraintInject wires a resolver deciding whether this task's operation
// constraints get injected into the planner system prompt. Read per round so the
// settings toggle takes effect without rebuilding the agent. nil = inject (default).
func (p *Planner) SetConstraintInject(fn func() bool) { p.injectConstraints = fn }

// wantConstraints reports whether constraint injection is enabled (default yes).
func (p *Planner) wantConstraints() bool { return p.injectConstraints == nil || p.injectConstraints() }

// todoFor returns the task's persistent planning todo store, creating it on first
// use. Shared across all of this task's planner wake-ups.
func (p *Planner) todoFor(expID int64) *actool.TodoStore {
	p.todoMu.Lock()
	defer p.todoMu.Unlock()
	s := p.todos[expID]
	if s == nil {
		s = actool.NewTodoStore()
		p.todos[expID] = s
	}
	return s
}

// SetKillWork wires the engine's per-work terminate callback so the planner's
// kill_work tool can stop a single running worker.
func (p *Planner) SetKillWork(fn func(intentID int64) error) { p.killWork = fn }

// SetSteerWork wires the engine's per-work steering callback so the planner's
// steer_work tool can inject a mid-run course-correction into a running worker.
func (p *Planner) SetSteerWork(fn func(intentID int64, msg string) error) { p.steerWork = fn }

// renderPlannerTodos formats the persistent planning todo for injection into the
// wake-up prompt (empty when there are no todos yet — first wake-up).
func renderPlannerTodos(items []actool.Todo) string {
	if len(items) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n【你的规划待办（跨唤醒保留，上一轮你写的）】：\n")
	for _, it := range items {
		mark := map[actool.TodoStatus]string{actool.TodoPending: "☐", actool.TodoInProgress: "▶", actool.TodoCompleted: "✔"}[it.Status]
		if mark == "" {
			mark = "☐"
		}
		b.WriteString(fmt.Sprintf("  %s %s\n", mark, it.Content))
	}
	b.WriteString("据此推进：只对【前置步骤已完成 / 其依赖的 fact 已存在】的下一步派意图；用 TodoWrite 更新清单（把已被 fact 满足的步骤标 completed）。不要重复派已在清单里 pending/in_progress 的步骤。")
	return b.String()
}

// TriggerEvent describes what concretely caused this planning round to fire, so
// the planner looks first at the actual change instead of re-scanning the whole
// overview. Kind:
//
//	"done"    — a worker finished intent IntentID (its output conclusion is fetched).
//	"finding" — a worker reported a finding on intent IntentID (Detail = 摘要).
//	"goal"    — the human (via 主 agent 的 set_goals) added one OR MORE goals in a
//	            single call (Goals = 本次新增的目标文本，1+ 条；set_goals 支持批量).
//	"goal_deleted" — the human deleted a goal from 总览的目标管理 (Detail = 被删目标文本).
//	"goal_edited"  — the human edited a goal from 总览的目标管理 (OldGoal→NewGoal 文本).
//	"cancelled" — the human deleted intent IntentID (Detail = 删除原因). The intent is
//	            stopped (not deleted) and the reason is attached to it as a fact.
type TriggerEvent struct {
	Kind     string
	IntentID int64
	Detail   string
	Summary  string   // Kind=="cancelled" 专用：删除前捕获的意图摘要（真删除后节点已不存在，无法再查）
	Goals    []string // Kind=="goal" 专用：本次 set_goals 新增的目标文本（1 条或多条）
	OldGoal  string   // Kind=="goal_edited" 专用：修改前的目标文本
	NewGoal  string   // Kind=="goal_edited" 专用：修改后的目标文本
	Hints    []string // Kind=="hint" 专用：本次 add_hint 新增的提示文本（1 条或多条）
}

// renderTriggers spells out the change(s) that fired this round: for a finished
// worker — which intent + its output conclusion; for a finding — which intent +
// what was found. Empty for time/heartbeat wakes. Reads the store (best-effort;
// a blank field never blocks the round).
func renderTriggers(ts *db.ExplorationStore, evs []TriggerEvent) string {
	if len(evs) == 0 || ts == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n【本次触发本轮的实际变动（先看这里，再决定是否补方向）】：")
	for _, ev := range evs {
		switch ev.Kind {
		case "goal":
			if len(ev.Goals) == 1 {
				b.WriteString(fmt.Sprintf("\n- 人（主 agent）新增了一个目标：%s —— 新的待达成目标，请据此补充探索方向（若尚无对应意图）。", ev.Goals[0]))
			} else {
				b.WriteString(fmt.Sprintf("\n- 人（主 agent）新增了 %d 个目标：%s —— 均为新的待达成目标，请逐一为尚无对应意图的目标补充探索方向。", len(ev.Goals), strings.Join(ev.Goals, "；")))
			}
		case "hint":
			if len(ev.Hints) == 1 {
				b.WriteString(fmt.Sprintf("\n- 人（主 agent）新增了一条战略提示：%s —— 已挂到探索图上，请据此调整/补充探索方向（若尚无对应意图）。", ev.Hints[0]))
			} else {
				b.WriteString(fmt.Sprintf("\n- 人（主 agent）新增了 %d 条战略提示：%s —— 均已挂到探索图上，请逐一据此调整/补充探索方向。", len(ev.Hints), strings.Join(ev.Hints, "；")))
			}
		case "goal_deleted":
			b.WriteString(fmt.Sprintf("\n- 人删除了该目标：%s —— 该目标已移除，请据此重判剩余目标/方向（不必再为它派意图）。", ev.Detail))
		case "goal_edited":
			b.WriteString(fmt.Sprintf("\n- 人修改了目标，由「%s」变为「%s」—— 请据新目标调整探索方向（原方向若已不适用请停派）。", ev.OldGoal, ev.NewGoal))
		case "finding":
			b.WriteString(fmt.Sprintf("\n- 意图 #%d（%s）的 worker 报告了一个 finding：%s", ev.IntentID, intentSummary(ts, ev.IntentID), ev.Detail))
		case "cancelled":
			// 意图内容优先用删除时捕获的 Summary（真删除后节点已不存在，intentSummary 查不到）。
			sm := ev.Summary
			if sm == "" {
				sm = intentSummary(ts, ev.IntentID)
			}
			b.WriteString(fmt.Sprintf("\n- 意图 #%d 由用户删除，意图内容是：%s、删除原因是：%s。该意图已删除（不再执行）；请据此重新规划。", ev.IntentID, sm, ev.Detail))
		default: // "done"
			b.WriteString(fmt.Sprintf("\n- 意图 #%d（%s）的 worker 结束，输出结论：%s", ev.IntentID, intentSummary(ts, ev.IntentID), workerOutput(ts, ev.IntentID)))
			if fids := factIDsYielded(ts, ev.IntentID); fids != "" {
				b.WriteString(fmt.Sprintf("；本意图新产生的事实 id：%s ", fids))
			}
		}
	}
	b.WriteString("\n（完整细节可 node_detail / get_worker_output / list_findings 再查。）")
	return b.String()
}

// factIDsYielded lists the fact ids an intent produced this run as "#12、#15", so the
// planner can jump straight to the round's incremental facts. Empty (best-effort) when
// the intent yielded no facts or the lookup fails.
func factIDsYielded(ts *db.ExplorationStore, id int64) string {
	ids, err := ts.FactsYielded(id)
	if err != nil || len(ids) == 0 {
		return ""
	}
	parts := make([]string, len(ids))
	for i, fid := range ids {
		parts[i] = fmt.Sprintf("#%d", fid)
	}
	return strings.Join(parts, "、")
}

// intentSummary reads an intent node's one-line summary (best-effort, "?" on miss).
func intentSummary(ts *db.ExplorationStore, id int64) string {
	n, err := ts.GetNode(id)
	if err != nil || n == nil {
		return "?"
	}
	var p map[string]any
	if json.Unmarshal(n.Payload, &p) == nil {
		if s, ok := p["summary"].(string); ok && s != "" {
			return s
		}
	}
	return "?"
}

// workerOutput returns the finished worker's conclusion for an intent — the last
// 'result' (else 'text') activity's full detail, truncated. Same source get_worker_output uses.
func workerOutput(ts *db.ExplorationStore, id int64) string {
	acts, _, err := ts.ActivityList(&id, 0, 1000)
	if err != nil {
		return "(取输出失败)"
	}
	var pick *db.Activity
	for i := range acts {
		if acts[i].Kind == "result" {
			pick = &acts[i]
		} else if acts[i].Kind == "text" && pick == nil {
			pick = &acts[i]
		}
	}
	if pick == nil {
		return "(该 work 尚无输出记录)"
	}
	out, _ := ts.ActivityDetail(pick.ID)
	if out == "" {
		out = pick.Summary
	}
	return truncOutput(out, 800)
}

// truncOutput caps a worker-output blob so the trigger context doesn't bloat the
// system prompt every round; full text is one get_worker_output call away.
func truncOutput(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + " …（已截断，完整见 get_worker_output）"
}

// renderGraphOverview folds the pre-computed graph_overview snapshot into the
// wake-up prompt so the planner starts each round with the full situation in
// hand — saving the round-trip it would otherwise spend calling the tool. It is
// the exact same JSON graph_overview would return; deeper detail is still one
// tool call away (node_detail / list_facts / …).
func renderGraphOverview(data map[string]any) string {
	b, err := json.Marshal(data)
	if err != nil {
		return "" // fall back to the model calling graph_overview itself
	}
	return "\n\n【本轮态势（graph_overview 预取，等同你调用该工具的返回；需要细节再按需调 node_detail/list_facts 等）】：\n" + string(b)
}

// plannerDefaultTmpl is the built-in EDITABLE body (段 [A]) of the planner prompt,
// seeded into agent_prompts. Goal is a {{.Goal}} template var; the 中间产物输出规约
// tail is code-owned (artifactSpec) and appended by plannerSystem after rendering.
const plannerDefaultTmpl = `You are the "planner" of an authorized penetration-testing platform, woken frequently (any change to the graph wakes you). Your job: read the situation -> judge the objectives -> **add exploration intents only where a genuinely uncovered new direction exists**. You plan, you do not execute: in this round your output may only be [generate or articulate intents] or [judge objectives] — never do the work inside a plan.

Task objective: {{.Goal}}

**How many intents this round (settle this first)**:
- **Hard floor (top priority)**: whenever [the objective is not met] AND [there is no open or running intent] (frontier_open=0 and running_intents empty), you MUST produce at least one intent that advances the objective this round — with nothing running to wait for and nothing queued, producing 0 intents means the task stalls. Even if every known direction only appears in recent_done, open a new one or continue one according to the done/exhausted/blocked judgment below.
- Above the hard floor, **producing 0 intents is a normal outcome, but it needs a valid reason** (not a default of "fewer is safer"): (1) **already covered** — every direction you thought of is already handled by an open/running intent (regenerating an existing intent in different words is a serious error); (2) **waiting on a dependency** — the next step depends on the output of work still running and it has not landed yet (forcing it now would leave the downstream spinning without its precondition; wait for the next wake-up after the graph updates).
- Conversely: when an uncovered new direction truly exists that does not depend on running work, or the objective is unmet and untested surface remains inside scope, you should dispatch — do not treat 0 intents as the lazy default.

**Decision flow on each wake-up**:

1. **The full situation is already attached below this prompt** (it is graph_overview's return; no need to call it again): task (original title + objective/root node), asset counts, goals and status, open/running/recent_done intents, sites_without_endpoints (sites with no endpoints, hinting at directions still to explore), facts (exploration fact count, a different category from findings), recent_facts ({id,summary,confidence?}).
   - **Scope**: exploration nodes (goals/intents/facts/findings) cover this task only; **assets are isolated per task by default** (only this task and explicitly related tasks; once the user enables "share global assets" the whole store is queryable) — ignore assets that belong to other tasks.
   - **Lineage**: each intent carries parents (upstream: which facts/intents it came from) and yields (downstream: which facts/findings it produced), and every recent_facts entry carries from_intent; use that to understand which facts came from which direction and whether a new direction can be synthesized.
   - **Negative or doubtful observations** (recent_facts entries such as "port closed / not injectable") are the worker's observations, not verdicts: before accepting one, call node_detail(id) to inspect the evidence — only when the evidence is solid, confidence=observed and the means were exhausted should the direction be treated as temporarily closed; when evidence is missing, it was "only probed once", or confidence=inferred, treat it as **not yet established** and, if it is in scope and no other intent covers it, dispatch one re-check intent by default to confirm or refute it (**at most one re-check per negative direction**; if it is still negative afterwards with reasonable evidence, respect the conclusion and stop dispatching).
   - **Call for deeper detail only when needed**: list_facts (paginated, newest first, 20 by default, supports q filter and before paging, returns total/has_more), list_findings (all findings), node_detail(id) (full evidence/detail; lists and recent_facts only give summaries), list_assets (pull: q search, type/company_id/task_id filters, pagination, or direct id/ids lookup), asset_neighbors. Whether assets are shared is the user's setting and you cannot enable it yourself; sharing does not mean wider testing authorization.

2. **Judge the objectives (your core duty)**: the goals field already carries each objective and its status; for an unmet objective that some finding or fact has proven, call prove_goal(goal_id, evidence_id, reason) to mark it met. **When the one you mark is the last unfinished objective, the system automatically declares the whole task complete** — completion is driven only by individual prove_goal calls; there is no other "one-click finish".
   - WARNING — **quantitative acceptance check (never rubber-stamp)**: when an objective carries a measurable condition (coverage reaching X%, a specified count, obtaining a given privilege), you MUST check the measured values in the graph_overview above (coverage.pct, findings_total counts, etc.) before prove_goal: if not met, prove_goal is FORBIDDEN and you dispatch intents to close the gap; never mark it met on the grounds that "it is mostly there / the core is done". Example: the requirement is 100% coverage while the measurement is coverage.pct=40% -> not met, keep dispatching gap-closing intents.

3. **(Optional, opening only, extremely light) probing to understand**: only when the graph has almost no facts yet (recent_facts essentially empty, the task just started) and the situation alone cannot make the initial intent concrete, may you run a very small amount of read-only probing against the target with Bash (e.g. one or two curls of the home page or a fingerprint). **The only legitimate output is a more precise intent description** — never a vulnerability found/verified/exploited, and never the results of enumerating endpoints, directories or parameters (that is the worker's job; express it as an intent and dispatch it). Three hard boundaries:
   - If a worker-produced fact already exists in the graph (facts>0 / recent_facts not empty) -> probing yourself is FORBIDDEN; every judgment rests on existing facts and this round's output may only be "dispatch a new intent" or "stop"; to dig into a lead, dispatch an intent for the worker instead of curling yourself.
   - Even at the opening, probe at most 3 times and stop — only enough to make the initial intent concrete; the moment you notice you are "investigating deeply" rather than "quickly fixing a direction" (enumerating endpoints or directories one by one, trying ids one by one, decoding chains, probing the same interface repeatedly, any injection/authorization/vulnerability testing — all worker work), stop immediately and write it as an intent.
   - Whatever can be decided from existing facts or the situation needs no probing at all.

4. **Decide which new directions to add**: "restraint" here means only **[not duplicating an intent that already exists]**, not "dispatch as little as possible" — while the objective is unmet, the default question is "what deeper, harder, still uncovered approach brings us closer to the goal", not "can we wrap up". Intents are **open exploration directions** (not fixed types or a menu); decide each one from known facts, assets and the objective, and compare it against open + running + recent_done:
   - Already covered by open/running -> do not generate (it is being handled).
   - Already appeared in recent_done -> **first read that intent's state (each entry carries it) to see how it stopped, then decide**:
     * **done (ran to completion)**: covered -> do not re-dispatch as-is; whether it is a dead end follows from the fact conclusions in its yields, not from the state; re-dispatch only when a **[materially new mechanism]** appears (a new fact, asset or parameter, a clearly different approach), and state in the summary how it differs from last time; re-phrasing or "maybe it works this time" does not count, and retrying is forbidden.
     * **exhausted (budget ran out mid-way, only partial output) / blocked (model or network failure, barely probed)**: both ended without a clean finish and with incomplete information — first use get_worker_trace / get_worker_output to see how far it actually got and where it stuck, then choose: it was cut by the budget near a breakthrough -> dispatch "continue from where the last run stopped"; it never really ran due to a pure external failure (often blocked) -> re-dispatch the same direction; it sticks at the same point every time -> change the approach or direction. The basis is always the real progress in the trace, never the state alone.
   - A brand-new direction covered by no intent at all -> generate it.
   - Every known direction is covered by intents still open/running -> do not generate, end the round (there is running or queued work, wait for it to advance); but if only recent_done covers them, nothing is open or running and the objective is unmet, the hard floor at the top requires opening or continuing one.
   - **Depth beats coverage**: coverage is a floor and an acceptance item, not the exploration goal itself; after finding a high-value entry point (one that may lead to RCE, privilege escalation or data exfiltration), prefer dispatching an intent that drives that path **[all the way through]** over spreading wide to level coverage by shallow-testing each asset.
   - **Keep the routes diverse and do not converge early**: while the objective is unmet, if every existing intent crowds the same route or entry point and a **fundamentally different** uncovered direction exists (another entry surface, another asset class, another exploit chain), prefer that divergent direction over adding a synonymous intent on the same line (judge the substance, not the wording); if the divergent direction is already covered by an existing intent, still do not generate. The ideal is 2-3 mechanically different routes coexisting (e.g. "hit through the upload chain" and "hit through auth bypass"), concentrating resources on one only after it produces evidence of moving toward the objective. **But diversity always yields to the operation constraints at the top**: an entry surface, port, host or operation excluded by a constraint must never become an intent, however fundamentally different it is.

   **Sequential exploit chains: dispatch step by step, do not split into parallel work.** For a strongly sequential chain (1->2->3, where each step depends on the actual output of the previous one): do not dispatch them all at once in parallel (a downstream step without its precondition only repeats or spins); record the whole chain as todos with TodoWrite (one item per step) and dispatch only the step whose precondition is satisfied this round (usually the first), then on the next wake-up after it yields a fact (the prompt carries the todo list) dispatch the next step and mark the satisfied ones completed. Do not split "the same thing" into two intents ("confirm the trigger point" and "trigger the trigger point" are one step); use parallel intents only for **[parallel, mutually independent]** dimensions (such as enumerating several unrelated endpoints).

5. **Submit**: use **[a single]** add_intent call to submit the selected new directions in a batch (an intents array, at most 4 of the highest value — do not call it once per item):
   - **summary**: one natural-language sentence describing the direction (the full address of the test target + what to do + why), with no fixed classification; deduplication relies mainly on comparing it with existing intents.
   - **asset_ids**: the ids of the target assets this direction tests or attacks (pass them whenever possible, 0/1/several, from list_assets) — whenever the direction revolves around concrete assets (site/endpoint/parameter/host) pass them, which drives coverage deduplication and links the asset chain; pass all of them when several are involved; leave it empty only for pure global reconnaissance with no concrete asset.
   - **parent_ids**: which upstream nodes this direction synthesized (optional, 0/1/several) — pass all of them when several facts combine into one intent, and also pass the upstream intent's or finding's id when the direction derives from one; leave it empty for a top-level brand-new direction.

Do not duplicate and do not pad; but when the objective is unmet and a deeper uncovered approach exists, dispatch it. Concise, focused, effective.`

func plannerSystem(goal, dataDir, workDir string) string {
	body := renderSystem("planner", plannerDefaultTmpl, PlannerVars{Goal: goal, DataDir: dataDir, Now: nowStr()})
	return body + artifactSpec(workDir)
}

// Plan runs one planning round. emit, if non-nil, receives the planner's execution
// steps (so users can see how it reads the situation and judges goals — the
// planner is the intent generator and was previously a black box). Returns whether
// the planner judged the goal met.
// triggers carries the concrete change(s) that fired this round — worker(s) done
// and/or finding(s) reported (may be several — the engine debounces a burst; empty
// for time/heartbeat wakes). They are spelled out at the top of the prompt so the
// planner looks first at the actual change (which intent, its output/finding).
func (p *Planner) Plan(ctx context.Context, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, goal string, triggers []TriggerEvent, emit func(db.Activity)) (met bool, reason string, err error) {
	// cold-digest §2.3/§7: advance this task's planner-round counter, maintain the
	// cold_since_round stamps, and (if a threshold is hit) kick off background
	// compaction. Synchronous part is cheap (a few queries); the LLM compaction
	// runs in a detached goroutine so it never adds latency to this round.
	p.compactor.OnPlannerRound(ctx, ts)
	tsx := NewToolSet(ts, "planner")
	tsx.SetFindingRecorder(p.findingRecorder)
	if as != nil {
		tsx.SetAssetStore(as, as.Companies())
	}
	tsx.SetTaskID(taskID)
	tsx.SetCoverageEnabled(as == nil || as.CoverageEnabled(taskID))
	tsx.killWork = p.killWork   // enable kill_work tool (nil = unavailable)
	tsx.steerWork = p.steerWork // enable steer_work tool (nil = unavailable)
	if origin, _ := ts.OriginFactID(); origin > 0 {
		tsx.SetOwnerNode(origin) // planner-side anchors default to the task root (origin fact)
	}
	// 领域工具 + 基础默认工具集（Read/Write/Edit/MultiEdit/LS/Glob/Grep/Bash）
	// 资产覆盖度功能关闭时剔除 add_task_scope/list_untested_assets（不入 prompt）。
	base := append(tsx.DropCoverageTools(tsx.PlannerTools()), actool.DefaultTools()...)
	ctx = WithRunInfo(ctx, RunInfo{TaskID: taskID, ExplorationID: explorationID(ts)})
	tools, def, cleanup := AugmentTools(ctx, "planner", base)
	defer cleanup()
	// 关键态势（刚完成的意图 + 预取的完整图）改放【本轮 user 输入】(见下方 input)，system
	// 只留静态规划正文。move-out 让 system 每轮稳定、更利于缓存；代价是若单轮变长，态势可能
	// 被 compaction 压缩（planner 单轮通常短，风险低）。situational 会拼进下方 input。
	situational := renderTriggers(ts, triggers) + renderGraphOverview(tsx.graphOverviewData())
	// 任务级 deadline / 终局模式(经 ctx 注入,见 taskclock.go)。终局那一轮把任务超时
	// planner 收尾词作为【本轮操作指令】拼进本轮 user 输入(随 situational),让它只做最后
	// 目标判定、不产新意图。
	tc := taskClockFrom(ctx)
	if tc.Final {
		situational += "\n\n【任务终局收尾（本轮特殊指令，覆盖上面的常规规划流程）】：" + resolveTaskTimeoutWrapup("planner")
	}
	// 本任务的工作目录 <workDir>/tasks/<taskID>，先建好。
	taskDir := ensureRunDir(p.workDir, taskID, 0)
	ctx = intercept.WithReviewContext(ctx, taskDir, intercept.ReviewBackground{})
	sysBody := plannerSystem(goal, p.workDir, taskDir)
	if p.wantConstraints() {
		sysBody += constraintBlock(ts) // 操作约束(若有)注入系统提示,框定探索边界
	}
	system, boundary := deferredSystem(sysBody, def)
	// planner 无自身墙钟预算;有 deadline 时把 MaxDuration 夹逼到剩余,让在跑的规划轮在
	// 任务到点时进收尾(因超时→任务超时词,因步数→per-run 词)。
	maxDur, clamped := clampMaxDuration(tc.DeadlineUnix, 0)
	settle := wrapupSettlement("planner", nil)
	if tc.DeadlineUnix > 0 {
		settle = wrapupSettlementForTask("planner", nil, clamped)
	}
	proxyAddr := p.proxyAddr
	if p.proxyAddrFn != nil {
		proxyAddr = p.proxyAddrFn()
	}
	opts := agentcore.Options{
		Provider:        p.prov,
		SystemPrompt:    system,
		DynamicBoundary: boundary,
		Tools:           append(tools, deepSeekTools(p.webSearch)...),
		DeferredTools:   def.Deferred,
		UnlockSet:       def.Unlock,
		PermissionMode:  permission.ModeBypass,
		EnableWebFetch:  true, // 走记录代理留痕；载入代理 CA 验证 MITM 重签的 HTTPS 证书
		WebFetchProxy:   proxyAddr,
		WebFetchCACert:  p.proxyCACert,
		// 联网搜索(可选)。ddgs 无需 key；brave-free 需 BraveKey；tavily 需 TavilyKey。
		// WebSearchProxy 是独立出口代理(http/https/socks5)，与记录流量的 MITM 代理无关；空则直连。
		EnableWebSearch:    p.webSearch.Enabled && p.webSearch.Backend != "deepseek",
		WebSearchBackend:   p.webSearch.Backend,
		BraveSearchAPIKey:  p.webSearch.BraveKey,
		TavilySearchAPIKey: p.webSearch.TavilyKey,
		WebSearchProxy:     p.webSearch.Proxy,
		BashEnv:            proxyEnv(proxyAddr, p.proxyCACert), // Bash 子命令默认走代理+信任 CA
		WorkingDir:         taskDir,                            // 本任务工作目录 <workDir>/tasks/<taskID>
		ToolOutputDir:      cmdOutDir(taskDir),
		MaxTurns:           p.maxTurns, // 0 = unlimited (configurable in agent management)
		MaxDuration:        maxDur,     // 0=不限;有 deadline 时=距 deadline 剩余
		Compaction:         compactionConfig(p.compactionWindow()),
		// 跨唤醒共享的规划待办：让串行链在多轮之间保留（session 是新的，store 不是）。
		Todos: p.todoFor(ts.ID()),
		// 命中【本轮】步数预算→ SDK 跑收尾:把本轮已想清楚的结论落地(该派的 add_intent、
		// 能证的 prove_goal、串行链记 TodoWrite),而非停止规划——planner 之后仍会被反复唤醒。
		// clamped(被任务 deadline 夹逼)时改用 PromptByReason(见 wrapupSettlementForTask)。
		Settlement:   settle,
		NonStreaming: p.nonStreaming(), // 该 profile 选非流式时走 Provider.Complete
		MaxTokens:    p.maxTokens(),    // 0 = 不发上限,由服务端默认值决定
	}
	if p.tx != nil { // persist raw LLM conversation; one accumulating file per task's planner
		opts.Transcript = p.tx
		opts.SessionID = fmt.Sprintf("exp%d-planner", ts.ID())
	}
	// 实验功能:开启后由 noa 接管上下文压缩(归档集中在 <workDir>/noa/<SessionID> 下,持久)。
	noaSession := fmt.Sprintf("exp%d-planner", ts.ID())
	enableNoa(&opts, p.noaEnabledFn, p.workDir, noaSession, noaWarn(noaSession))
	// 态势（刚完成的意图 + 完整图）现在拼进本轮 user 输入（见下方 input）。user 里还有
	// 指令 + 跨唤醒待办（todo 是模型自己的规划便签，可再生，放 user 即可）。
	// 开场白按「本轮有无具体变动」分两种：有变动 → 指向下方【实际变动】块；无变动
	// (心跳定时巡检 / hint / 恢复等) → 别谎称"图发生了变化",转而提示顺带复查在跑意图。
	lead := "刚有具体变动（见下面的【本次触发本轮的实际变动】），据此规划下一步："
	if len(triggers) == 0 {
		lead = "本轮是**定时巡检（心跳到点）/无具体变动信号**的唤醒——图不一定有新变动。顺带复查在跑意图：长时间无进展或跑偏的用 steer_work 纠偏、方向整个错的用 kill_work 止损；再判定目标、决定是否补方向："
		// 心跳/无变动唤醒时,若全图已无任何 open 或 running 意图 → 探索已停摆(没 worker 在跑、
		// 也没排队方向)。明确告知 planner 并强制其本轮补出新方向,别只复查在跑意图后空转一轮。
		if active, err := ts.HasActiveIntent(); err == nil && !active {
			lead = "本轮是**定时巡检（心跳到点）**的唤醒,且当前**已没有任何 open 或 running 的意图**——没有 worker 在跑、也没有排队中的方向,探索已停摆。你**必须**在本轮产出一个或多个向目标推进、且与图中既有意图**互不重复**的新意图(不得产出 0 意图);先据下面的态势判定目标是否已达成,未达成则立即补方向："
		}
	}
	input := lead + situational + "\n\n据上面的态势，判定目标。目标已【真正达成】（已拿到目标成果/已确认目标漏洞）时用 prove_goal 逐个标记。**硬底线：只要目标尚未达成、且当前没有任何 open 或 running 意图（frontier_open=0 且 running_intents 为空），本轮就必须产出至少一个向目标推进的意图——此时没有在跑的 work 可等、也没有在排队的方向，产出 0 意图=任务停摆。仅当已有 open/running 意图在推进、或目标已达成时，本轮才可以不产出新意图。**" +
		renderPlannerTodos(opts.Todos.List())
	// MaxDuration 现在会在墙钟到点打断在跑工具并就地进收尾(在活 ctx 上),单轮卡死不再
	// 绕过收尾,无需外部硬 ctx 兜底。ctx 只承载 pause / kill / shutdown。
	_, _, err = captureRun(ctx, opts, input,
		func(r db.Activity) {
			if emit != nil {
				r.Worker = "planner" // planner activity has no intent_id (it generates them)
				emit(r)
			}
		})
	return tsx.GoalMet, tsx.Reason, err
}
