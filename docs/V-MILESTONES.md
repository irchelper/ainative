# Version Milestones

This document provides a concise, high-level summary of notable features by version milestone.

- Source of truth for deep details: `docs/ARCH.md` and `CHANGELOG.md`
- Scope: only fill known gaps (do not rewrite existing docs)

## V1–V6 (Early MVP)

> TBD: feature summary consolidated from commits `d2730be..7610e5e`.

### Feature summary

- **V1 (Initial scaffold)** — Repository initialized and basic scaffolding in place.
- **V2 (MVP: F1–F6)** — Core task queue MVP implemented (create/dispatch/poll, basic workflow).
- **V3 (macOS launchd deploy)** — Service deployment via launchd, environment variables wiring.
- **V4 (Docs baseline)** — Initial `docs/ARCH.md`, `docs/PRD.md`, `docs/INTRO.md` added.
- **V5 (Notifications)** — Richer webhook messages (duration/format) for better observability.
- **V6 (Stability hardening)** — DB persistence and notification refinements.

### Commit range

- `d2730be` (Initial commit)
- …
- `7610e5e` (db persistence, simplified messages, failed Discord+SessionNotifier dual push)

## V26

> V26 is inferred as the batch of changes after v1.0.0 release and before the V27 timeout hotfix series.

### Feature summary

- **Task export** — Export tasks as CSV/JSON.
- **Keyboard shortcuts (first pass)** — Initial keyboard navigation and shortcut help UI.
- **UI polish** — Small fixes around navigation mounting and page behaviors.

### Commit range

- `5be5357` → `e79a662`

## V28

> V28 is inferred from the config version bump commit.

### Feature summary

- **Config version bump** — `/api/config` version bumped to `v28`.

### Commit range

- `d6bf806`
