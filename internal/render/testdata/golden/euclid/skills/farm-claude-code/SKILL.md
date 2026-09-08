---
name: farm-claude-code
description: Delegate long or important coding, configuration and software work to Claude Code through the farm wrapper
---

# farm-claude-code

`farm-claude` runs Claude Code on this host as you, through the farm's
wrapper. It is the farm's way of doing software work, and it is a better
engineer than you are on anything long.

## When to use it

Use it for any long or important software, configuration or coding work, as
much as possible: implementing a feature, changing configuration, refactoring,
writing or fixing tests, tracking down a bug, reviewing a diff. Do that work
through `farm-claude` rather than by editing files yourself.

Do it yourself only for things too small to describe: reading a file, one
`git status`, a one-line edit you have already decided on.

## How to call it

Run it from the root of the repository you are working in:

```
farm-claude -p "<self-contained task, with its acceptance criteria>" \
  [--model claude-fable-5-1|claude-opus-5] \
  [--effort low|medium|high|xhigh|max] \
  [--max-turns N]
```

- The prompt must stand alone. It is the whole context Claude Code gets:
  what to change, in which files, and how you will judge that it worked.
  A prompt that says "continue" or "fix the tests" without naming them wastes
  a run.
- `--model` defaults to `claude-opus-5`. Ask for `claude-fable-5-1` when the
  task is genuinely hard to reason about rather than merely long.
- `--effort` defaults to the farm's setting; raise it for design work, lower
  it for mechanical edits.

## After the call

1. Read the result it prints. It is the run's own account of what it did.
2. Verify with the repository's own tests and checks — the ones the project
   already has, run the way the project runs them. The result text is a claim;
   the test output is the evidence.
3. Iterate with another `farm-claude -p` that names what is still wrong, and
   quotes the failing output. Do not hand-edit the files it wrote: the next
   run will not know what you changed.

## Limits

`farm-claude` exits non-zero and prints

```
Claude Code limit: <kind>; resets <time>
```

when the subscription's limit is reached (exit 4 when the farm is already in a
limit window, exit 5 when this run hit one). Exit 3 means no slot came free
within twenty minutes.

When you see any of those, stop calling it. Block your card with
`kanban_block --kind transient` and put the reset time in the reason. Retrying
in a loop opens no window early and spends the limit for every other agent on
the farm.
