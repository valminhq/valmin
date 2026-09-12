#!/bin/sh
# The panel's entrypoint exists for one line: umask 002, so every file and directory the
# panel creates under a bind mount stays group-writable for the gid 10000 the operator's
# login account is in (08 §2.1). That is what keeps `cp` working for the human who wants
# their world, which is ADR-006's whole point.
set -e
umask 002
exec "$@"
