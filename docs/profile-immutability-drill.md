# Profile immutability drill (host check)

`snowfarm apply` installs `config.yaml` and `SOUL.md` as `root:farm-<agent>`
0640 and sets the immutable attribute on **four** paths per agent:

| Path | Owner | Mode | `chattr` |
|---|---|---|---|
| `<home>/.hermes` | `farm-<agent>:farm-<agent>` | 0755 | `+i` |
| `<home>/.hermes/profiles` | `farm-<agent>:farm-<agent>` | 0755 | `+i` |
| `<home>/.hermes/profiles/<agent>/config.yaml` | `root:farm-<agent>` | 0640 | `+i` |
| `<home>/.hermes/profiles/<agent>/SOUL.md` | `root:farm-<agent>` | 0640 | `+i` |

The flags, not the ownership, are the control. Unlink and rename permission
comes from the containing directory, and `<home>` and `<hermes_home>` must
stay agent-writable — Claude Code writes `~/.claude.json`, Hermes writes
`state.db` — so without the flags an agent can delete or rename its way to a
profile of its own.

## What CI can and cannot prove

`internal/sysops` asserts that `Apply` issues `chattr +i` for all four paths,
that a cleared flag is drift in the next `Plan`, and that a failed apply
restores every flag it cleared. It cannot run the drill below: `chattr +i`
needs `CAP_LINUX_IMMUTABLE`, a tmpfs runner returns `EOPNOTSUPP` for file
flags, and no `farm-<agent>` user exists in the runner. The drill is therefore
a host check, run once per host after the first apply and after any change to
this code path.

## The drill

Run as the agent, on a host where `snowfarm apply` has completed. Substitute
the agent's name for `<agent>`.

```sh
sudo -u farm-<agent> sh -c '
  set -x
  cd /var/lib/farm/<agent>/.hermes/profiles/<agent>
  : > other.yaml                                            # must succeed
  rm -f config.yaml                                         # 1
  mv -f other.yaml config.yaml                              # 2
  echo x > config.yaml                                      # 3
  mv /var/lib/farm/<agent>/.hermes /var/lib/farm/<agent>/.hermes.bak   # 4
  mv /var/lib/farm/<agent>/.hermes/profiles/<agent> \
     /var/lib/farm/<agent>/.hermes/profiles/<agent>.old     # 5
  mkdir /var/lib/farm/<agent>/.hermes/profiles/intruder     # 6
  rm -f other.yaml
'
```

Stage the decoy inside `<hermes_home>`, which the agent owns, and never in
`/tmp`. `/tmp` is a tmpfs on this host, so `mv` out of it is not `rename(2)`
at all: it degrades to copy-then-unlink, which opens `config.yaml` for
writing and fails with `EACCES` on the 0640 `root:farm-<agent>` mode whether
or not the flag is set. A step that fails either way reports nothing. Step 2
must be a same-directory rename, which is the operation the flag refuses.
`-f` on 1 and 2 stops `rm` and `mv` prompting about a target the agent cannot
write, so each reaches the syscall instead of a question; it suppresses no
error the drill is looking for.

All six must fail, 1, 2, 4, 5 and 6 with `EPERM` (`Operation not permitted`)
and 3 with `EPERM`, or `EACCES` once the flag is cleared. Nothing may be
missing or renamed afterwards:

```sh
lsattr -d /var/lib/farm/<agent>/.hermes \
          /var/lib/farm/<agent>/.hermes/profiles \
          /var/lib/farm/<agent>/.hermes/profiles/<agent>/config.yaml \
          /var/lib/farm/<agent>/.hermes/profiles/<agent>/SOUL.md
```

Each line must carry `i`.

Operations 1 and 2 fail only because of the flag on the file: unlink
permission comes from `<hermes_home>`, which the agent owns. Operations 4, 5
and 6 fail only because of the flags on the two ancestors: `may_delete()`
tests `IS_IMMUTABLE` on the inode being renamed, never on what it contains,
and an immutable directory admits no new entries.

## The red side

The drill is falsifiable, which is the point of making the two ancestor
directories agent-owned rather than root-owned. On a scratch host only:

```sh
chattr -i /var/lib/farm/<agent>/.hermes/profiles
sudo -u farm-<agent> mkdir /var/lib/farm/<agent>/.hermes/profiles/intruder   # succeeds
rmdir /var/lib/farm/<agent>/.hermes/profiles/intruder
chattr +i /var/lib/farm/<agent>/.hermes/profiles

chattr -i /var/lib/farm/<agent>/.hermes/profiles/<agent>/config.yaml
sudo -u farm-<agent> sh -c '
  cd /var/lib/farm/<agent>/.hermes/profiles/<agent>
  : > other.yaml
  mv -f other.yaml config.yaml
'                                                                            # succeeds
snowfarm apply --only <agent>
```

Operations 2 and 6 must succeed with the flag cleared: 2 because the agent
owns `<hermes_home>` and a same-directory rename needs nothing but that, 6
because the two ancestors are agent-owned rather than root-owned. If either
fails with the flag gone, that flag is decorative and the drill proves
nothing. The red side for 2 destroys `config.yaml`, so the `snowfarm apply`
after it is not optional — it rewrites the file from the roster and sets the
flag again. Never leave a host in the cleared state; run `snowfarm plan`
afterwards and confirm it reports no drift.

## What the drill does not cover

- **Hermes must tolerate a read-only `config.yaml`.** `/reasoning --global`
  and `/model` persist there, which is why managers are told never to use
  them, but a startup path that expects the file writable would fail loudly.
  Confirm at the pin.
- **Nothing may write directly under `<home>/.hermes` or
  `<home>/.hermes/profiles`.** Every unit sets `HERMES_HOME` to the profile
  directory and writes are expected below it, but that is an inference from
  the documentation. If such a write is real, drop the flag from `.hermes`
  alone, keep it on `profiles` — which is what stops `<hermes_home>` from
  being renamed — and let the guard's hash sweep carry the residue.

The sweep is the fallback that runs whether or not the flags hold: the guard
hashes both files for every agent against the baseline `snowfarm apply` writes
to `/var/lib/snowfarm/profile-hashes.json`, and a read or hash *failure* is
drift, not health.
