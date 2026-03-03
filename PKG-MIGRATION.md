# PKG Migration Guide

This document explains the convention used in this fork and the steps to apply whenever upstream goclaw is updated.

## Background

Go's `internal/` directory prevents external projects from importing those packages.
To make every goclaw feature importable as a standalone Go package, all code was moved from `internal/` to `pkg/`.

The full module path is:
```
github.com/nextlevelbuilder/goclaw
```

All importable packages now live under `pkg/`:
```
pkg/agent          – agent loop, input guard, tool loop, history, pruning
pkg/bootstrap      – system prompt seeding, template files
pkg/bus            – in-process event bus
pkg/channels       – Telegram, Feishu, Discord, WhatsApp, Zalo channel managers
pkg/config         – JSON5 config loading + env var overlay
pkg/cron           – cron scheduling (at/every/cron expr)
pkg/crypto         – AES-256-GCM key encryption
pkg/gateway        – WebSocket server + RPC method router
pkg/heartbeat      – periodic health check service
pkg/hooks          – agent/command hook engine
pkg/http           – HTTP API (/v1/chat/completions, /v1/agents, etc.)
pkg/mcp            – MCP bridge + tool manager
pkg/memory         – memory system (SQLite FTS5 / pgvector)
pkg/pairing        – browser pairing (8-char codes)
pkg/permissions    – RBAC (admin/operator/viewer)
pkg/providers      – LLM providers (Anthropic, OpenAI-compat, DashScope) + retry
pkg/sandbox        – Docker-based code sandbox
pkg/scheduler      – lane-based concurrency (main/subagent/cron)
pkg/sessions       – session key helpers + manager
pkg/skills         – SKILL.md loader + BM25 search
pkg/store          – store interfaces + file/ and pg/ implementations
pkg/tools          – tool registry, filesystem, shell, web, memory, subagent, MCP
pkg/tracing        – LLM call tracing + optional OTel export
pkg/tts            – text-to-speech (OpenAI, ElevenLabs, Edge, MiniMax)
pkg/upgrade        – upgrade checker + data hooks
```

Also already public (were never in `internal/`):
```
pkg/browser        – browser automation (Rod + CDP)
pkg/protocol       – wire types (frames, methods, errors, events)
```

---

## How to Use as a Go Package

In your project's `go.mod`:
```
require github.com/nextlevelbuilder/goclaw v0.0.0-<commit>
```

Then import whichever packages you need:
```go
import (
    "github.com/nextlevelbuilder/goclaw/pkg/providers"
    "github.com/nextlevelbuilder/goclaw/pkg/agent"
    "github.com/nextlevelbuilder/goclaw/pkg/store"
)
```

---

## When Upstream goclaw Is Updated

When upstream adds new packages, moves files, or renames packages, follow these steps:

### Step 1 — Fetch upstream changes

```bash
git fetch upstream main
git merge upstream/main --no-commit
```

> If you don't have the upstream remote yet:
> ```bash
> git remote add upstream https://github.com/nextlevelbuilder/goclaw.git
> ```

### Step 2 — Check if any new `internal/` packages were added

```bash
# List any internal/ dirs that are not yet mirrored in pkg/
find internal -mindepth 1 -maxdepth 1 -type d 2>/dev/null | while read d; do
    name=$(basename "$d")
    [ ! -d "pkg/$name" ] && echo "NEW: $d  →  pkg/$name"
done
```

If the command prints nothing, skip to Step 4.

### Step 3 — Copy new packages to `pkg/`

For each new directory printed by Step 2:

```bash
cp -r internal/<newpackage> pkg/<newpackage>
```

Then update import paths **inside the copied files**:

```bash
find pkg/<newpackage> -name "*.go" | \
    xargs sed -i 's|github\.com/nextlevelbuilder/goclaw/internal/|github.com/nextlevelbuilder/goclaw/pkg/|g'
```

### Step 4 — Update all import paths across the whole codebase

Run this once after any merge to catch every file (both old and new):

```bash
find . -name "*.go" -not -path "*/vendor/*" | \
    xargs sed -i 's|github\.com/nextlevelbuilder/goclaw/internal/|github.com/nextlevelbuilder/goclaw/pkg/|g'
```

### Step 5 — Remove `internal/` if it came back

```bash
[ -d internal ] && rm -rf internal && echo "internal/ removed"
```

### Step 6 — Verify no stale references remain

```bash
grep -r "goclaw/internal/" . --include="*.go" | wc -l
# Expected output: 0
```

If the count is non-zero, the grep output will show exactly which files still reference `internal/`. Fix them manually or re-run the sed from Step 4.

### Step 7 — Build check

```bash
GOTOOLCHAIN=local go build ./...
```

Fix any compilation errors, then:

### Step 8 — Commit

```bash
git add -A
git commit -m "chore: sync upstream changes, keep all packages under pkg/"
```

---

## Rules for New Code in This Fork

1. **Never create packages under `internal/`.** Always put new packages directly in `pkg/<name>/`.
2. **Never import** `github.com/nextlevelbuilder/goclaw/internal/...` anywhere.
3. When adding a file to an existing package, the import path stays the same — no extra steps needed.
4. Keep `pkg/protocol` and `pkg/browser` as they are; they were already public.

---

## Quick Reference — One-Liner Migration

If you need to redo the full migration from scratch (e.g. after a large upstream rebase):

```bash
# 1. Copy everything
for d in internal/*/; do
    name=$(basename "$d")
    [ ! -d "pkg/$name" ] && cp -r "$d" "pkg/$name"
done

# 2. Replace all internal/ imports
find . -name "*.go" -not -path "*/vendor/*" | \
    xargs sed -i 's|github\.com/nextlevelbuilder/goclaw/internal/|github.com/nextlevelbuilder/goclaw/pkg/|g'

# 3. Remove internal/
rm -rf internal

# 4. Verify
grep -r "goclaw/internal/" . --include="*.go" | wc -l   # must be 0
```
