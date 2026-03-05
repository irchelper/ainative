# Version Milestones

This document provides a concise, high-level summary of notable features by version milestone.

- Source of truth for deep details: `docs/ARCH.md` and `CHANGELOG.md`
- Scope: only fill known gaps (do not rewrite existing docs)

## V1–V6 (Early MVP)

> SubA (bootstrapping/MVP) backfill reference (historical diff): `341bf6d..40941ca` (roots: `d2730be`, `0656c4b`).

> SubA (Bootstrapping / MVP) is covered by early commits `d2730be..40941ca` (<= 8 commits).
> Remaining V1–V6 details can be expanded later from `d2730be..7610e5e`.

### Feature summary

- **V1 (Initial scaffold)** — Repository initialized and basic scaffolding in place.
- **V2 (MVP: F1–F6)** — Core task queue MVP implemented (create/dispatch/poll, basic workflow).
- **V3 (macOS launchd deploy)** — Service deployment via launchd, environment variables wiring.
- **V4 (Docs baseline)** — Initial `docs/ARCH.md`, `docs/PRD.md`, `docs/INTRO.md` added.
- **V5 (Failed + auto-retry + session notifications)** — Introduced `failed` state with `retry_assigned_to`, basic auto-retry wiring, and `SessionNotifier` for waking experts/CEO.
  - Key commits: `505a6a2`, `5689cc2`
- **V6 (Operational hardening on top of V5)** — Focused on persistence and production-ready notifications: DB persistence, simplified message formats, and dual-push for failed alerts (Discord + SessionNotifier).
  - Key commit: `7610e5e`

### Commit range

- `d2730be` → `40941ca` (SubA: bootstrap + MVP core)
- TBD: expand the remaining V1–V6 range up to `7610e5e` once Batch-1 SubB/SubC backfill is completed.

---

## V7

### Feature summary

- **`superseded_by` dependency field** — Tasks can declare a `superseded_by` reference; downstream is automatically blocked when a superseding task exists.
- **Dependency expansion** — `deps` field extended to support richer dependency graphs.
- **`blocked_downstream`** — New mechanism to propagate blocked status to dependent tasks.

### Commit range

- `6d56b5d` (single feat commit)

---

## V8

### Feature summary

- **Chain notify** — Completion of a chained task now triggers notifications up the chain.
- **`retry_routing` table** — Introduced persistent routing table for retry assignment; determines which agent retries a failed task based on failure type.
- **Triggered dispatch fix** — Fixed edge cases where triggered dispatches (autoRetry / autoAdvance) could silently drop.

### Commit range

- `a9dc0cd` (single feat commit)

---

## V9

### Feature summary

- **RetryQueue** — Dedicated in-memory retry queue decouples retry scheduling from the main dispatch path.
- **Stale ticker** — Background ticker periodically checks for stale (claimed but inactive) tasks and returns them to pending.

### Commit range

- `7d2a335` (single feat commit)

---

## V10

### Feature summary

- **Review-reject two-stage chain** — `autoRetry` now supports a two-stage chain: on review rejection, task is re-routed through a defined correction chain before re-submission.

### Commit range

- `1edb15f` (feat)

---

## V10.1

### Feature summary

- **`isReviewReject` + vision agent support** — Corrected review-reject detection logic; added vision agent to retry routing rules.
- **4 seed rules** — Seeded retry routing rules for vision, pm, ops, and related agents.

### Commit range

- `9f33672` (fix)

---

## V11

### Feature summary

- **`agent_channel_map` webhook routing** — Per-agent Discord webhook routing; each agent's notifications go to its own channel instead of a shared channel.
- **Stale `max_dispatches` limit** — Stale tasks now cap re-dispatch attempts; tasks exceeding the limit are moved to `failed` rather than looping indefinitely.
- **`cancelled` terminal state** — Added `cancelled` as a proper terminal state; `failed`/`pending` tasks can be cancelled without triggering retry or unlocking downstream.
- **E2E test isolation** — E2E scripts use `[TEST]` prefix + cleanup to avoid polluting production data; updated test spec in ARCH.md.
- **`OnTaskComplete` single-task CEO notify** — `notify_ceo_on_complete` now works for individual tasks (not just chains); configurable per-task.
- **`verify-channelid.sh`** — Utility script to validate channel ID list, webhook reachability, plist config, and generate config templates.

### Commit range

- `19535b2` → `4049435` (cancelled state, e2e isolation)
- `e2f142a` (OnTaskComplete single-task notify)
- `2a501f8` (verify-channelid.sh)

---

## V12

### Feature summary

- **AI-native Workbench architecture** — Documented and designed the AI Workbench (web UI) architecture: embed.FS serving, config package, schema layer, new API endpoints. Renamed project `agent-queue` → `ainative`.
- **P1 – Workbench scaffold** — Web directory scaffold (`web/`), embed.FS integration, config package, schema package, 3–4 new API endpoints.
- **P2 – Core pages** — Core workbench pages implemented; human approval timeout ticker added.
- **P3 – Open-source readiness** — OpenAPI spec + generated TypeScript types; full README rewrite + CONTRIBUTING + agent onboarding guide + config reference; GitHub Actions CI/CD workflows (test/build/release).

### Commit range

- `44bc956` (V12 arch doc)
- `451e9d2` → `687c04a` (P1: workbench scaffold)
- `7b4e408` (P2: core pages)
- `3eade04`, `9c8795f`, `fb61180` (P3: OpenAPI, docs, CI)

---

## V13

### Feature summary

- **`autoAdvance`** — Success-path automatic dispatch: on task completion with a `next_agent` result, the system automatically dispatches a follow-up task (symmetric to `autoRetry`).

### Commit range

- `319f948` (single feat commit)

---

## V14

### Feature summary

- **Result routing** — When a task result contains a `next_agent` field (JSON), the system automatically dispatches a follow-up task to the specified agent, enabling dynamic multi-agent chains driven by task results.

### Commit range

- `8e9b5a3` (single feat commit)

---

## V15

### Feature summary

- **Task templates** — `/templates` CRUD API; `/dispatch/from-template/:name` endpoint for dispatching tasks from named templates with parameter substitution.

### Commit range

- `9fe2104` (single feat commit)

---

## V16

### Feature summary

- **Agent task timeout auto-fail** — Tasks that exceed their agent-defined timeout are automatically transitioned to `failed`, freeing downstream and triggering retry routing.

### Commit range

- `2e36f38` (single feat commit)

---

## V17

### Feature summary

- **SSE real-time updates** — Server-Sent Events endpoint for pushing live task state changes to the web UI without polling.
- **Human approval node (frontend UI)** — Web UI support for human-in-the-loop approval steps: tasks requiring human sign-off surface a dedicated approval action in the workbench.

### Commit range

- `9d6b3c9` (SSE)
- `116e355` (human approval UI)

---

## V18

### Feature summary

- **`/dispatch/graph` endpoint** — Returns the full task dependency graph (DAG) as structured data for visualization.
- **Task search and filtering** — API and UI support for filtering tasks by status, agent, keyword, and other attributes.

### Commit range

- `b82b369` (single feat commit)

---

## V19

### Feature summary

- **`/summary` `assigned_to` filter** — Task summary endpoint now supports filtering by `assigned_to`, enabling per-agent dashboards.
- **Dynamic priority** — Priority recalculation based on wait time and task age; older high-priority tasks surface first.

### Commit range

- `57c8fe1` (single fix/feat commit)

---

## V20

### Feature summary

- **Scalar API docs** — Integrated Scalar (OpenAPI UI) for interactive in-browser API documentation.
- **DAG visualization** — Web UI renders the task dependency graph visually; nodes show task status and relationships.

### Commit range

- `30eefb3` (single feat commit)

---

## V21

### Feature summary

- **Search UI** — Full-text search interface in the workbench for finding tasks by description, agent, or result.
- **Agent statistics panel** — Dashboard panel showing per-agent task counts, completion rates, and active task status.

### Commit range

- `db19a8b` (single feat commit)

---

## V22

### Feature summary

- **Batch operations** — UI support for selecting multiple tasks and applying bulk actions (cancel, retry, export).
- **Mobile responsive layout** — Workbench UI adapted for mobile screen sizes; key pages (kanban, task list) reflow correctly on narrow viewports.

### Commit range

- `d2d33b4` (single feat commit)

---

## V23

### Feature summary

- **V23-A – Outbound webhook notifications** — Task completion/failure events now push outbound webhook notifications to configured external URLs.
- **V23-B – Timeline enhancements** — Task timeline view enhanced with duration display and inline chain step visualization.

### Commit range

- `205cb09` (V23-A: webhook outbound)
- `4ba8934` (V23-B: timeline)

---

## V24

### Feature summary

- **V24-A – i18n (bilingual UI)** — Web UI internationalized with Chinese/English toggle; all UI strings externalized.
- **V24-B – Task comments** — Tasks support a comment thread; agents and users can leave notes attached to a task record.

### Commit range

- `a334cbf` (V24-A: i18n)
- `d84c705` (V24-B: comments)

---

## V25

### Feature summary

- **FSM: allow cancel from `claimed`/`in_progress`** — State machine extended to permit cancellation of tasks that are actively claimed or in-progress, not just pending ones.

### Commit range

- `61b7cde` (single fix commit)

---

## V26

> V26 is inferred as the batch of changes after v1.0.0 release and before the V27 timeout hotfix series.

### Feature summary

- **Task export** — Export tasks as CSV/JSON.
- **Keyboard shortcuts (first pass)** — Initial keyboard navigation and shortcut help UI.
- **UI polish** — Small fixes around navigation mounting and page behaviors.

### Commit range

- `5be5357` → `e79a662`

---

## V27

### Feature summary

- **Timeout false-kill fix (V27-A)** — Fixed a bug where the stale/timeout ticker would incorrectly kill tasks that had valid, recent heartbeats (P0-2 and P0-3 edge cases).
- **Config version bump** — `/api/config` version bumped to `v27`.

### Commit range

- `e010bb7` (V27-A: timeout fix)
- `9334255` (version bump)

---

## V28

> V28 is inferred from the config version bump commit.

### Feature summary

- **Config version bump** — `/api/config` version bumped to `v28`.
- **Badge semantics fixes** — Multiple badge-related adjustments and semantics alignment.
  - Key commit: `e4d9281`
- **Cleanup API improvements** — Test-task cleanup endpoint enhanced (e.g. `max_age` parameter).
  - Key commit: `8990dba`
- **Settings page enhancements** — Expanded system & integration visibility (version, agent list, DB path, PID/uptime/listen address), plus a test-data cleanup control and clearer webhook status display.
  - Key commit: `e4d9281`

### Commit range

- `e4d9281` → `d6bf806` (badge/settings/admin improvements → config version bump).
  - TBD: if ops defines V28 as a wider batch, adjust range accordingly.

---

## V29

### Feature summary

- **V29a – UX urgent fixes** — Fixed pending task filter display; added `cancelled` column to kanban view; various layout corrections.
- **V29b – TaskDetailPage SPA** — Task detail page rebuilt as a proper SPA route; exception/error panel enhanced; workbench copy/text updates.

### Commit range

- `0f88ad2` (V29a: UX fixes)
- `9879654` (V29b: TaskDetailPage SPA)

---

## V30

### Feature summary

- **V30-v1 – DAG redesign** — DAG visualization rebuilt with dagre + d3; tracking page gains Tab-based status filtering and pagination.
- **V30-v2 – Dashboard throttling + badge/progress fixes** — Dashboard API throttled to prevent overload; badge rendering fixed; progress bar now uses four-color coding; kanban done-column supports collapsed pagination.
- **V30-v3 – UX polish** — Tooltip (`title` attribute) support; description line-height fix; empty-state copy; DB path truncation in settings.
- **V30-v4 – `spec_file` field** — `POST /dispatch` accepts a `spec_file` path; server reads the local file and populates `description` from its contents, enabling large task specs without inline JSON.

### Commit range

- `2a48e96` → `3d56b5c`

---

## V31

### Feature summary

- **`failed → done` permission check** — Only the task's `assignee` or `system` may transition a task from `failed` to `done`; empty `changed_by` no longer bypasses this check (P1-C security fix).
- **Retry depth cap** — Auto-retry is capped at depth ≥ 3; tasks exceeding the cap are automatically `cancelled` and CEO is notified.
- **Coder timeout self-retry** — Coder agent timeout triggers a self-retry path before escalating.
- **BrowserRelay blocked intercept** — Tasks dispatched to BrowserRelay when no tab is attached are intercepted and set to `blocked` instead of failing silently.
- **Test-task failure silence** — Tasks identified as test tasks (via `[TEST]` prefix, `assigned_to=="test"`, or `failure_reason=="test"`) suppress CEO failure alerts and auto-cancel after failure.
- **Orphan retry cleanup** — On `failed → done` transitions, orphan retry tasks (pending retries with no live parent) are automatically cancelled to prevent ghost tasks accumulating.
- **Chain `spec_file` routing fix** — Chain dispatch correctly resolves `spec_file` paths relative to the chain's working directory.
- **Routing rule fix** — `coder-spec` routing rule updated to route to `thinker` instead of `coder` for spec-driven tasks.

### Commit range

- `7eba0d7` → `727c36c`

---

## Post-V31 / v1.1.0

> Stabilization, noise reduction, and minor feature additions after the V31 hardening sprint.

### Feature summary

- **Webhook notification delay fix** — Fixed race condition causing webhook notifications to be delayed or lost (methods A+B+C applied).
- **Failed task auto-cleanup** — Done tasks purged after 24h; failed tasks purged after configurable `max_age` (default 4h); `ceo_notified_at` field tracks notification status.
- **Retry depth cap notifications** — CEO notified when retry depth cap cancellations occur.
- **`TaskDetailPage` raw JSON panel** — Collapsible raw JSON panel added to task detail view for debugging.
- **Alert noise reduction** — `isTestTask` expanded to cover `unknown-agent-*` patterns and `assigned_to=="test"`; only explicitly test-marked tasks silence CEO alerts.
- **`acceptance[]` field (Phase 1)** — `acceptance` string array added to task schema with soft-warning validation (non-blocking).
- **Timezone fix** — Web UI `formatTime` uses `Asia/Kuala_Lumpur` timezone consistently.
- **Poll() terminal-state filter** — `Poll()` filters out tasks that transitioned to a terminal state between Phase 1 and Phase 2 of the polling window.

### Commit range

- `7eba0d7` (V31 boundary) → `HEAD` (ongoing)
- Key commits: `056a064`, `a840b15`, `68e4bbc`, `76934ec`, `9289b34`, `7839790`, `3ada14f`, `417349a`
