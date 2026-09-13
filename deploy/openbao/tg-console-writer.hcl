# OpenBao policy for the CONSOLE SECRET WRITER (TG-252).
#
# The console is a static nginx bundle and cannot hold a credential; the GROUNDER performs the write on
# its behalf, under this identity. It is deliberately NOT TG's own "tg" role, which holds broad read
# access to every operational secret — a settings dialog must not inherit the ability to READ the estate's
# credentials just because it needs to SET one.
#
# SCOPED TO THE MODULE PREFIX, NOT TO NAMES. An earlier version of this policy enumerated each module's
# path, which meant the configuration dialog worked for exactly the modules somebody had remembered to add
# to this file and returned 403 for every other one. A Save button that fails because an operator has not
# hand-edited an HCL is the defect the whole surface exists to remove. Provision this ONCE and every
# module — present and future — can have its credential rotated from the UI with no further OpenBao work.
#
# WRITE-ONLY. Note the absence of "read": KV v2 permits create/update without it, so this identity can
# rotate any module's secret and cannot exfiltrate one — including the one it just wrote. Compromise of
# the console yields the ability to BREAK a connector, never to steal a credential. That is also why the
# dialog shows set/unset and a rotation timestamp rather than a value: it could not show one if it tried.

path "secret/data/tg/modules/*" {
  capabilities = ["create", "update"]
}

# KV v2 needs metadata write for versioning. Metadata READ would leak the fact and timing of every
# rotation, so it is not granted.
path "secret/metadata/tg/modules/*" {
  capabilities = ["update"]
}

# TG's OWN operational secrets live directly under secret/data/tg/ and are untouchable from here. Denied
# explicitly so a future broadening of a parent path cannot silently grant them.
path "secret/data/tg/*" {
  capabilities = ["deny"]
}
path "secret/metadata/tg/*" {
  capabilities = ["deny"]
}
path "sys/*" {
  capabilities = ["deny"]
}
path "auth/*" {
  capabilities = ["deny"]
}
