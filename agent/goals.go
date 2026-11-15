package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	acperm "github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// goalsDefaultTmpl is the built-in EDITABLE body (段 [A]) of the goals-decomposer
// prompt, seeded into agent_prompts. No template vars are used today.
const goalsDefaultTmpl = `You are the objective decomposer of a penetration-testing platform. Your job is to identify the FINAL OUTCOME the user wants from their input, not to plan attack steps.

FIRST STEP (do this before splitting objectives): extract the operation constraints
From the "task goal / task description", identify the operator's explicit rules about what may and may not be done, and register each one with set_constraints (if the goal or description carries no operation constraints, skip this step):
- type=deny: forbidden operations (e.g. "do not scan ports", "no write/delete against production", "no brute force", "do not touch subdomain X").
- type=allow: explicitly allowed or bounded scope (e.g. "passive recon only", "only this domain").
- A constraint is neither an objective nor an attack step: it bounds operational behavior.
- Constraints MUST be self-contained and name concrete targets: replace pronouns such as "the current target / current port / current IP / current domain / this site" with the concrete values from the goal or description. Constraints are injected into the execution stage on their own, stripped of context, where a pronoun cannot be resolved.
  Example: goal is https://abc.example.net -> write "only test abc.example.net", not "only test the current target"; "only test port 443 of the target, do not scan other ports", not "only test the current port". If the text only says "the current target" but the address is given, put the address in.
- Register ONLY constraints explicitly written or emphasized in the goal/description; never invent any. When the type is unclear, use deny (the conservative choice).
- If the goal/description truly contains no operation constraint, do NOT call set_constraints.
After registering the constraints (if any), proceed to the objective split below.

AN OBJECTIVE = a final, deliverable, verifiable result.

WHAT IS NOT AN OBJECTIVE (never list as a sub-objective):
- information gathering, reconnaissance, endpoint scanning
- vulnerability analysis and verification work
- attack steps or exploitation techniques
- result verification steps

SPLITTING PRINCIPLES:
- The user described a single final outcome -> output one.
- There are several INDEPENDENT final deliverables -> list each of them.
- When one maps to a clear vulnerability class, set vulnclass; leave it empty for information-gathering or business-logic outcomes.
- Never invent objectives the user did not mention.

Call set_goals to submit the result.`

// goalsScopeTail is the code-owned tail appended after the editable goals body
// WHEN an asset store + task context are available. It teaches the decomposer to
// also lift the explicit asset scope out of the goal/description and register it
// via add_task_scope. Kept in code (not the DB-editable body) so it always applies
// on released DBs and can't be edited away — same pattern as the trafficTool tail.
const goalsScopeTail = `
**Extra duty: register the asset scope under test**
Besides splitting objectives, identify the EXPLICIT asset scope given in the "task goal / task description" and register it with add_task_scope (this task's authorization boundary, and the denominator for asset test coverage). MINIMAL SCOPE RULE: register only the target the user explicitly named; never widen it on your own.
- If the goal is a URL or an address with a hostname (e.g. https://xxx.example.com/path, app.example.com) -> take its FULL hostname, kind=subdomain, value=full hostname.
  Example: goal https://a1b2c3.lab.example.net/path -> kind=subdomain, value=a1b2c3.lab.example.net (NOT example.net).
  NEVER collapse a hostname with a subdomain into the root domain — turning xxx.example.com into example.com would extend the scope beyond the user's target and violates the minimal-scope rule.
- ONLY when the user gave a BARE root domain with no subdomain (e.g. example.com written directly), or explicitly said "the whole site / all subdomains / the entire domain" -> use kind=root_domain, value=example.com.
- A bare IP or a network range -> kind=ip / cidr, value=the IP or CIDR.
- Do NOT register a company scope — the task has just been created and the asset system usually does not know this company yet, so registration would fail; company-level scope is handled later, in the planning stage.
Other rules:
- Register only what is EXPLICITLY written in the goal/description; never invent or infer a domain or IP that was not mentioned.
- In reason, briefly state which sentence the scope came from, so it can be audited.
- If the goal/description carries no explicit asset scope, do NOT call add_task_scope.
Register the scope with add_task_scope first (if any), then call set_goals to submit the objectives.`

// GoalSpec is one decomposed objective.
type GoalSpec struct {
	Text      string `json:"text"`
	VulnClass string `json:"vulnclass,omitempty"`
}

// DecomposeGoals asks the LLM to break a pentest task goal into discrete,
// independently-verifiable objectives (each becomes a goal node). Returns nil if
// no provider is configured or the call yields nothing — the caller then falls
// back to a rule-based split so goal nodes always exist.
//
// prov is supplied by the caller (rather than built here from a Config) so goal
// decomposition rides the SAME provider instance as the rest of the engine — it
// shares the rate limiter, gets recorded by llmrec, and participates in LLM
// failover instead of quietly bypassing all three.
//
// desc is the task's free-text description (背景：靶标范围/数量要求/交战说明等).
// It is fed alongside the goal so the decomposer no longer splits blind — the
// prompt still forbids inventing anything the two texts don't state.
//
// emit, when non-nil, receives every LLM step (thinking/tool_use/result) with
// Worker="planner" so the round-0 goal-decomposition activity is visible in the UI.
//
// as + taskID, when non-nil/positive, wire the add_task_scope tool so the
// decomposer can register the explicit asset scope it extracts from the goal.
//
// ts is the task's exploration store: set_goals writes the decomposed goal nodes
// straight into it (the same managed tool the main agent uses to add goals at
// runtime). The returned specs are read back from the store so callers can emit
// per-goal activity and detect the "LLM produced nothing" case for their fallback.
func DecomposeGoals(ctx context.Context, prov llm.Provider, dataDir, goalText, desc string, as *db.AssetStore, ts *db.ExplorationStore, taskID int64, emit func(db.Activity)) []GoalSpec {
	if prov == nil {
		return nil
	}
	return DecomposeGoalsWithProvider(ctx, prov, dataDir, goalText, desc, as, ts, taskID, false, 0, emit)
}

// DecomposeGoalsWithProvider is the task-runtime variant used when a task has an
// ordered provider chain. It preserves the same tools and write behavior while
// letting the caller own provider selection/failover. maxTokens is the profile's
// per-reply output cap (0 = send none).
func DecomposeGoalsWithProvider(ctx context.Context, prov llm.Provider, dataDir, goalText, desc string, as *db.AssetStore, ts *db.ExplorationStore, taskID int64, nonStreaming bool, maxTokens int, emit func(db.Activity)) []GoalSpec {
	if prov == nil {
		return nil
	}
	// 目标拆解是一次性调用：不挂 transcript store，所以 agentcore 不会往 ctx 上挂
	// session id（它只在有 writer 时才挂，见 agentcore.Prompt）。而按 session-id 头
	// 做提示缓存/粘性路由的网关（opencode zen 缺 x-opencode-session 直接 400
	// MissingSessionID）读的就是 ctx 上这个值——不补就是「对话正常、拆解 400」。
	// 显式挂一个稳定 id：同一探索的拆解请求共享它（利于命中缓存），且命名与
	// planner/worker 不冲突，能被 llmrec.parseSession 正确归因。
	if ts != nil {
		ctx = transcript.WithSessionID(ctx, fmt.Sprintf("exp%d-goals", ts.ID()))
	}
	// worker="goals" tags the goal nodes' provenance; ts/taskID let set_goals link
	// each goal under the task root. This is the catalog's real set_goals tool, so a
	// web-edited description/schema on it applies here too.
	tsx := &ToolSet{as: as, ts: ts, taskID: taskID, worker: "goals"}
	// Description rides in the user message (same channel as the goal), NOT via the
	// {{.EngagementDescription}} template var — else a prompt that references the var
	// would inject the description twice. System prompt stays pure static instructions.
	sys := renderSystem("goals", goalsDefaultTmpl, GoalsVars{DataDir: dataDir, Now: nowStr()})
	// set_constraints 始终可用(不依赖 asset store):正文已含「先抽操作约束再拆目标」这步
	// (可在 agent 编辑页改措辞),这里只需接上工具。
	tools := []actool.CoreTool{tsx.setGoals(), tsx.setConstraints()}
	// Wire add_task_scope only when we have a real asset store + task to write to.
	// The scope-extraction tail is appended in lockstep so the prompt never asks for
	// a tool that isn't present.
	if as != nil && taskID > 0 {
		tools = append(tools, tsx.addTaskScope())
		sys += goalsScopeTail
	}
	userMsg := "任务目标：\n" + goalText
	if d := strings.TrimSpace(desc); d != "" {
		userMsg += "\n\n任务描述（背景信息，可能含靶标范围/数量要求/交战说明；仅供参考，不要臆造其中未提及的内容）：\n" + d
	}
	// Use captureRun so every LLM step is emitted as an activity record (visible in
	// the plan tab under the round-0 marker). Falls back gracefully when emit is nil.
	captureEmit := func(r db.Activity) {
		if emit != nil {
			r.Worker = "planner"
			emit(r)
		}
	}
	captureRun(ctx, agentcore.Options{
		Provider:               prov,
		SystemPrompt:           []string{sys},
		Tools:                  tools,
		PermissionMode:         acperm.ModeBypass,
		DisableBackgroundTasks: true,
		// 3 步(抽约束 → 登记范围 → 拆目标)各需一次工具调用,给足回合避免收尾前漏调 set_goals。
		MaxTurns:     8,
		NonStreaming: nonStreaming, // 该 profile 选非流式时走 Provider.Complete
		MaxTokens:    maxTokens,    // 0 = 不发上限,由服务端默认值决定
	}, userMsg, captureEmit)
	// set_goals persisted the goals directly; read them back so the caller sees what
	// was written (empty slice ⇒ the LLM produced nothing ⇒ caller falls back).
	if ts == nil {
		return nil
	}
	nodes, _ := ts.ListByKind(db.KindGoal, 10000)
	var out []GoalSpec
	for _, n := range nodes {
		var p struct {
			Text      string `json:"text"`
			VulnClass string `json:"vulnclass"`
		}
		_ = json.Unmarshal(n.Payload, &p)
		if strings.TrimSpace(p.Text) != "" {
			out = append(out, GoalSpec{Text: p.Text, VulnClass: p.VulnClass})
		}
	}
	return out
}
