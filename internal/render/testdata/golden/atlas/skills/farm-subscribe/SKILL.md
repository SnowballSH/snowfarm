---
name: farm-subscribe
description: Subscribe a Kanban card to the Discord thread it was asked in, so its outcome is posted there in your voice
---

# farm-subscribe

A card you create through `kanban_create` reports nowhere until it is
subscribed to a thread. Subscribe every card you create to the thread you were
asked in, immediately after creating it.

## The command

```
hermes kanban notify-subscribe <task_id> \
  --platform discord \
  --chat-id <channel_id> \
  --thread-id <thread_id> \
  --chat-type thread \
  --notifier-profile <your name> \
  --delivery-mode notify+wake
```

`--notifier-profile` is your own profile name: you own the bot that posts, so
the outcome reaches the operator in your voice. `notify+wake` also gives you a
turn in that thread when the card ends, which is when you report or escalate.

## Finding the two ids

Both ids come from the `discord` toolset's `fetch_messages` result for the
conversation you are in:

- `--chat-id` is the **channel** id — the parent channel of the thread, the
  one the operator's top-level message was posted in.
- `--thread-id` is the **thread** id — the thread your reply is in.

When you were asked in a top-level channel message and Hermes opened a thread
for the exchange, the thread id is that new thread, not the channel.

## Notes

- Subscribe once per card. A card created by the operator's `/kanban create`
  inside a thread is already subscribed to it; do not add a second
  subscription for the same thread, or the operator sees the outcome twice.
- Subscribe a card you created outside the target thread explicitly: nothing
  else will.
