#!/bin/sh
# Egress audit for the host this platform runs on. Read-only; run as the
# Gitea user on the PRODUCTION host (a dev laptop tells you nothing about it).
#
# It answers four questions an operator on a locked-down network needs to be
# able to answer at any time, without reading code:
#   1. what is listening (the inbound surface)
#   2. what the Gitea process is connected to RIGHT NOW (live egress)
#   3. whether the host firewall actually blocks outbound
#   4. whether the destinations policy forbids are, in fact, unreachable
#
# Usage: docs/company/scripts/check-egress.sh [app.ini path]
INI="${1:-custom/conf/app.ini}"
fail=0
say() { printf '%s\n' "$*"; }
hdr() { say; say "## $*"; }

hdr "1) Listening sockets (inbound surface — expect only Gitea's port, ideally on localhost behind a reverse proxy)"
out=""
command -v ss >/dev/null 2>&1 && out=$(ss -ltnp 2>/dev/null | tail -n +2)
[ -z "$out" ] && command -v lsof >/dev/null 2>&1 && out=$(lsof -nP -iTCP -sTCP:LISTEN 2>/dev/null | tail -n +2)
[ -z "$out" ] && command -v netstat >/dev/null 2>&1 && out=$(netstat -an 2>/dev/null | grep -i LISTEN)
[ -n "$out" ] && printf '%s\n' "$out" || say "  (nothing listening, or no tool could read sockets)"

hdr "2) Live outbound connections from Gitea and its apps (expect none to the outside)"
for pid in $(pgrep -f 'gitea web' 2>/dev/null; pgrep -f 'uvicorn' 2>/dev/null); do
  if command -v lsof >/dev/null 2>&1; then lsof -nP -i -a -p "$pid" 2>/dev/null | grep ESTABLISHED | grep -v '127.0.0.1\|\[::1\]' || true
  elif command -v ss >/dev/null 2>&1; then ss -tnp 2>/dev/null | grep "pid=$pid," | grep -v '127.0.0.1\|::1' || true; fi
done

hdr "3) Host firewall — is OUTBOUND filtered at all?"
if command -v nft >/dev/null 2>&1 && nft list ruleset >/dev/null 2>&1; then
  nft list ruleset 2>/dev/null | grep -iE 'chain (output|forward)|policy' | head -10
elif command -v iptables >/dev/null 2>&1; then
  iptables -S OUTPUT 2>/dev/null | head -15
elif command -v ufw >/dev/null 2>&1; then ufw status verbose 2>/dev/null | head -15
elif command -v pfctl >/dev/null 2>&1; then r=$(pfctl -s rules 2>/dev/null | head -15); [ -n "$r" ] && printf '%s\n' "$r" || say "  (pfctl printed nothing — it needs root; rerun with sudo to see the outbound rules)"
else say "  (no firewall tool found — outbound is NOT filtered by this host)"; fail=1; fi
say "  NOTE: a default of ACCEPT on OUTPUT means the host itself blocks nothing outbound."

hdr "4) Forbidden destinations — each MUST fail to connect under policy"
for host in api.anthropic.com pypi.org files.pythonhosted.org gravatar.com github.com; do
  if command -v curl >/dev/null 2>&1; then
    code=$(curl -s -o /dev/null -m 5 -w '%{http_code}' "https://$host/" 2>/dev/null)
    if [ -n "$code" ] && [ "$code" != "000" ]; then say "  REACHABLE  $host (HTTP $code)  <-- policy violation path is OPEN"; fail=1
    else say "  blocked    $host"; fi
  fi
done

hdr "5) app.ini egress keys (each closes one path; 'missing' means Gitea's permissive default)"
chk() { v=$(grep -E "^[[:space:]]*$1[[:space:]]*=" "$INI" 2>/dev/null | head -1 | sed 's/^[^=]*=[[:space:]]*//;s/[[:space:]]*$//'); if [ -z "$v" ]; then say "  MISSING   $1  (expected: $2)"; fail=1; elif [ "$v" != "$2" ]; then say "  DIFFERS   $1=$v  (expected: $2)"; else say "  ok        $1=$v"; fi; }
chk AI_ENABLED false
chk PIP_ALLOW_PUBLIC_INDEX false
# Avatars: NOT an app.ini key any more. Gitea moved these to the admin panel
# and stores them in system_setting; an app.ini line is ignored (and logged as
# a deprecation error). So the effective value is read from the database. The
# built-in default for disable_gravatar is true, but a default is not a
# policy: "absent" is reported, so an operator sees it was never stated.
DB="${GITEA_DB:-data/gitea.db}"
dbchk() {
  if ! command -v sqlite3 >/dev/null 2>&1 || [ ! -f "$DB" ]; then say "  SKIP      $1  (no sqlite3 or no $DB — check admin panel > Configuration)"; return; fi
  v=$(sqlite3 "$DB" "SELECT setting_value FROM system_setting WHERE setting_key='$1';" 2>/dev/null)
  if [ -z "$v" ]; then say "  ABSENT    $1  (running on the built-in default of $3; state it in admin panel > Configuration)"; [ "$3" = "$2" ] || fail=1
  elif [ "$v" != "$2" ]; then say "  DIFFERS   $1=$v  (expected: $2)"; fail=1
  else say "  ok        $1=$v"; fi
}
dbchk picture.disable_gravatar true true
dbchk picture.enable_federated_avatar false false
chk DISABLE_NEW_PULL true
chk DISABLE_NEW_PUSH true
chk DISABLE_MIGRATIONS true
v=$(grep -E "^[[:space:]]*PIP_INDEX_URL[[:space:]]*=" "$INI" 2>/dev/null | sed 's/^[^=]*=[[:space:]]*//;s/[[:space:]]*$//'); [ -z "$v" ] && { say "  BLANK     PIP_INDEX_URL  (deploys will be refused until set, or will reach public PyPI if PIP_ALLOW_PUBLIC_INDEX=true)"; }
grep -qE '^[[:space:]]*ALLOWED_HOST_LIST[[:space:]]*=[[:space:]]*$' "$INI" 2>/dev/null && say "  ok        ALLOWED_HOST_LIST is empty (no external webhook host)" || { say "  CHECK     ALLOWED_HOST_LIST allows external webhook targets"; fail=1; }

say; [ "$fail" = 0 ] && say "RESULT: no open egress path found" || say "RESULT: open egress paths found above — fix before this host is trusted on the network"
exit $fail
