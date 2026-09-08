# hygiene testdata

`profile-hashes.json` is a verbatim two-agent excerpt of the baseline
`internal/sysops` writes to `/var/lib/snowfarm/profile-hashes.json`
(`Applier.profileHashFile`): a top-level object keyed by agent name, each
holding the SHA-256 of that agent's rendered `config.yaml` and `SOUL.md` under
those file names. It pins the on-disk contract between the writer (C4's apply)
and the reader (the guard's hygiene sweep), which share no Go type.
