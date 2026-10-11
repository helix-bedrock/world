<!-- TEMPLATE: fill in each section for this repo, then delete this line. -->

# AGENTS.md

Rules for AI coding agents working in this repo. Also read the org-wide rules in
`../.github/AGENT_RULES.md` (online:
<https://github.com/helix-bedrock/.github/blob/main/AGENT_RULES.md>). Where they
disagree, this file wins for this repo.

## What this repo is

<What it does, what it does not do, and who uses it.>

## Commands

```bash
go build ./...
go vet ./...
golangci-lint run
go test -race ./...
go test -run=^$ -bench=. -benchmem ./...   # benchmarks
```

## Layout

<Main packages and what each one owns.>

## Invariants: do not break these

<!-- Things that must stay true. Examples:
1. This package never imports the engine.
2. Decoding never allocates more than the input size.
3. The on-disk format matches BDS byte for byte. -->

1. <...>

## Gotchas

<Non-obvious traps: generated files, test data, platform quirks.>
