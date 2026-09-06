#!/bin/sh
# What this host can and cannot do, before anything is deployed to it.
#
#   ./preflight.sh
#
# Needs no arguments and no privileges. Answers the four questions that
# decide whether this platform can run here at all:
#
#   - can apps be isolated, and by which mechanism
#   - can dependencies be installed
#   - which kernel, since Landlock's network support arrived in 6.7
#
# See docs/company/app-platform.md.

say() { printf '%-34s %s\n' "$1" "$2"; }

# Everything below asks the Linux kernel about Linux features. Run anywhere
# else and the answers are not merely useless, they are wrong — a Darwin
# kernel numbered 25.x sails past a "6.7 or newer" test.
if [ "$(uname -s)" != Linux ]; then
	echo "This host is $(uname -s), not Linux."
	echo "The sandbox exists only on Linux, so there is nothing here to check."
	exit 2
fi

echo "=== host ==="
say "kernel" "$(uname -r)"
say "distribution" "$(. /etc/os-release 2>/dev/null && echo "$PRETTY_NAME" || echo unknown)"
say "architecture" "$(uname -m)"
echo

echo "=== isolation: bubblewrap (preferred) ==="
if command -v bwrap >/dev/null 2>&1; then
	say "bwrap" "$(command -v bwrap)"
	say "setuid" "$(ls -l "$(command -v bwrap)" | cut -c1-10)"
	if bwrap --unshare-all --ro-bind /usr /usr -- /bin/true 2>/dev/null; then
		say "sandbox creation" "WORKS — bubblewrap will be used"
	else
		say "sandbox creation" "FAILS: $(bwrap --unshare-all --ro-bind /usr /usr -- /bin/true 2>&1 | head -1)"
	fi
else
	say "bwrap" "not installed"
fi
# The specific reason bubblewrap usually fails: unprivileged user namespaces.
if ! command -v unshare >/dev/null 2>&1; then
	# Absent is not the same as blocked, and saying so would send someone
	# looking for a sysctl that is not the problem.
	say "unprivileged user namespaces" "cannot test — unshare(1) not installed"
elif unshare -Ur true 2>/dev/null; then
	say "unprivileged user namespaces" "allowed"
else
	say "unprivileged user namespaces" "BLOCKED: $(unshare -Ur true 2>&1 | head -1)"
	for knob in kernel.unprivileged_userns_clone user.max_user_namespaces \
		kernel.apparmor_restrict_unprivileged_userns; do
		value="$(sysctl -n "$knob" 2>/dev/null || true)"
		[ -n "$value" ] && say "  $knob" "$value"
	done
fi
echo

echo "=== isolation: Landlock + seccomp (fallback) ==="
# Landlock's own ABI has no shell interface, so the kernel version is the
# proxy: filesystem restrictions arrived in 5.13, network ones in 6.7.
kernel="$(uname -r | cut -d- -f1)"
major="${kernel%%.*}"
rest="${kernel#*.}"
minor="${rest%%.*}"
if [ "$major" -gt 6 ] || { [ "$major" -eq 6 ] && [ "$minor" -ge 7 ]; }; then
	say "landlock" "filesystem + network (ABI 4+)"
elif [ "$major" -gt 5 ] || { [ "$major" -eq 5 ] && [ "$minor" -ge 13 ]; }; then
	say "landlock" "filesystem only — network relies on seccomp alone"
else
	say "landlock" "UNAVAILABLE (needs kernel 5.13+)"
fi
say "landlock lsm enabled" "$(cat /sys/kernel/security/lsm 2>/dev/null | tr ',' '\n' | grep -c landlock 2>/dev/null | sed 's/^0$/no/;s/^[1-9].*/yes/')"
echo

echo "=== dependencies ==="
say "python3" "$(command -v python3 || echo MISSING)"
say "python3 version" "$(python3 -V 2>&1 || true)"
say "venv module" "$(python3 -c 'import venv' 2>/dev/null && echo present || echo MISSING)"
tmp="$(mktemp -d)"
if python3 -m pip download --no-deps --dest "$tmp" requests >/dev/null 2>&1; then
	say "PyPI reachable" "yes"
else
	say "PyPI reachable" "NO — an internal mirror or a wheelhouse is required"
fi
rm -rf "$tmp"
echo

echo "=== disk ==="
say "writable home" "$(test -w "$HOME" && echo yes || echo NO)"
say "free space on \$HOME" "$(df -h "$HOME" 2>/dev/null | awk 'NR==2 {print $4}')"
