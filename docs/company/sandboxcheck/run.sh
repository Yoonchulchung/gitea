#!/bin/sh
# Verify the department-app sandbox on a deploy server, without deploying an
# app. See docs/company/app-platform.md for what is being verified and why.
#
#   ./run.sh /path/to/gitea /path/to/gitea/data
#
# Run it as the account Gitea runs as. The entire question is what *that*
# account can still reach from inside the sandbox, so running it as anyone
# else answers a different question.
#
# Two runs happen, in this order:
#
#   1. A control run, outside the sandbox. Every "must be denied" check is
#      expected to succeed. If it does not, the probe cannot tell a
#      sandboxed process from an unsandboxed one and proves nothing.
#   2. The real run, through `gitea deptapp-exec`, with the same restrictions
#      the platform applies to a real department app.
set -eu

usage() {
	echo "usage: run.sh /path/to/gitea /path/to/gitea/data" >&2
	exit 2
}

GITEA="${1:-}"
DATA="${2:-}"
[ -x "$GITEA" ] || usage
[ -d "$DATA" ] || usage

HERE="$(cd "$(dirname "$0")" && pwd)"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# The probe has to live somewhere the sandboxed process can read, so it goes
# in the one writable directory rather than being read from the repository.
# Laid out exactly like a real release: the code in app/ (read-only inside
# the sandbox, writable outside it, since the Gitea account owns it) and the
# one writable directory in run/. The difference between those two is what
# the "write into the release tree" check measures.
mkdir -p "$WORK/app" "$WORK/run"
cp "$HERE/probe.py" "$WORK/app/probe.py"

GITEA_PID="$(pgrep -f 'gitea web' | head -1 || true)"
[ -n "$GITEA_PID" ] || GITEA_PID=1

PYTHON="$(command -v python3 || true)"
[ -n "$PYTHON" ] || { echo "python3 is required" >&2; exit 1; }

echo "gitea      : $GITEA"
echo "data dir   : $DATA"
echo "gitea pid  : $GITEA_PID"
echo "python     : $PYTHON"
echo

echo "############ 1/2  control run — NOT sandboxed ############"
echo
if ! "$PYTHON" "$WORK/app/probe.py" "$DATA" "$GITEA_PID" "$WORK/run" --control; then
	echo
	echo "Stopping: the control run failed, so the real run would prove nothing." >&2
	exit 1
fi

echo
echo "############ 2/2  sandboxed run ############"
echo

# The same read-only set the platform grants a real app — kept in step with
# sandboxReadOnlyBinds in company/appsandbox.go — plus the one writable
# directory. Note what is absent: /proc, /tmp, and Gitea's data directory.
exec "$GITEA" deptapp-exec \
	--ro /usr --ro /lib --ro /lib64 --ro /bin --ro /sbin \
	--ro /etc/ssl --ro /etc/ca-certificates --ro /etc/resolv.conf \
	--ro /etc/passwd --ro /etc/group --ro /etc/nsswitch.conf --ro /etc/localtime \
	--ro /dev/urandom --ro /dev/null --ro /proc/self \
	--ro "$WORK/app" \
	--rw "$WORK/run" \
	--gitea-pid "$GITEA_PID" \
	--memory-mb 512 --processes 64 --open-files 4096 \
	-- "$PYTHON" "$WORK/app/probe.py" "$DATA" "$GITEA_PID" "$WORK/run"
