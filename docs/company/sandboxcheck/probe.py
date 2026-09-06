"""Everything a hostile department app would try, attempted in one go.

Run this inside the sandbox (see run.sh). A line marked ok is the sandbox
doing its job; a single FAIL means the isolation is not what the design
claims, and apps must not run on that host until it is.

The "must be ALLOWED" section is not filler. Without it, "every check was
denied" is indistinguishable from "Python never started properly", and the
second one looks like success.

Usage: probe.py <gitea-data-dir> <gitea-pid> <writable-dir> [--control]

--control expects the opposite outcome for every check, and is how run.sh
proves this file still detects an unprotected process. A probe that passes
outside the sandbox is testing nothing.
"""

import ctypes
import os
import resource
import socket
import subprocess
import sys

data_dir = sys.argv[1] if len(sys.argv) > 1 else "/nonexistent"
gitea_pid = int(sys.argv[2]) if len(sys.argv) > 2 else 1
workdir = sys.argv[3] if len(sys.argv) > 3 else "/tmp"
control = "--control" in sys.argv

failures = []
skipped = []


def check(name, fn, expect="deny", discriminating=True):
    """Run one attempt and report whether the outcome was the required one.

    discriminating=False marks a check that ordinary file permissions already
    refuse, sandbox or not — /etc/shadow is root-only on any Linux. Those are
    worth attempting as defence in depth, but they cannot tell a sandboxed
    process from an unsandboxed one, so the control run must not expect them
    to succeed or it would fail for the wrong reason.
    """
    try:
        fn()
        outcome, allowed = "ALLOWED", True
    except NotImplementedError as e:
        # The host does not have this to test — reported, never counted as a
        # pass, because "absent" is not "blocked".
        print("  --   %-42s SKIPPED (%s)" % (name, e))
        skipped.append(name)
        return
    except Exception as e:
        outcome, allowed = "DENIED (%s)" % type(e).__name__, False

    want_allowed = (expect == "allow")
    if control:
        if not discriminating:
            print("  --   %-42s %s (not part of the control)" % (name, outcome))
            return
        # Outside the sandbox every "must be denied" check should get
        # through. Anything still denied here is a check that cannot
        # distinguish the two cases, so it proves nothing inside.
        want_allowed = True
    ok = allowed == want_allowed
    print("  %-4s %-42s %s" % ("ok" if ok else "FAIL", name, outcome))
    if not ok:
        failures.append(name)


def under(*parts):
    return os.path.join(data_dir, *parts)


print("=== Gitea's own data — the whole reason this sandbox exists ===")
print("    (a readable session file is an immediate admin login)")
check("open gitea.db", lambda: open(under("gitea.db"), "rb").read(1))
check("list data/sessions/", lambda: os.listdir(under("sessions")))
check("list every department's repos", lambda: os.listdir(under("gitea-repositories")))
check("open custom/conf/app.ini", lambda: open(under("..", "custom", "conf", "app.ini")).read(1))
# Both of these are refused by ordinary permissions on any Linux, so they are
# defence in depth rather than evidence — see `discriminating` above.
check("open /etc/shadow", lambda: open("/etc/shadow").read(1), discriminating=False)
check("write into /usr", lambda: open("/usr/pwned", "w"), discriminating=False)
# This one *is* evidence: the Gitea account owns the release tree, so it can
# write there unless the sandbox mounts or binds it read-only. An app that
# can rewrite its own code can persist a backdoor into the next restart.
check("write into the release tree",
      lambda: open(os.path.join(workdir, os.pardir, "app", "backdoor.py"), "w"))

print("\n=== Reaching the network ===")


def tcp_connect():
    s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    s.settimeout(3)
    s.connect(("1.1.1.1", 80))


def raw_socket():
    if not hasattr(socket, "AF_PACKET"):
        raise NotImplementedError("AF_PACKET is Linux-only")
    socket.socket(socket.AF_PACKET, socket.SOCK_RAW)


def run_curl():
    if subprocess.run(["sh", "-c", "command -v curl"], capture_output=True).returncode != 0:
        raise NotImplementedError("curl is not installed")
    subprocess.run(["curl", "-sS", "--max-time", "3", "https://example.com"],
                   check=True, capture_output=True)


check("TCP connect out", tcp_connect)
check("socket(AF_INET6)", lambda: socket.socket(socket.AF_INET6, socket.SOCK_STREAM))
# AF_PACKET/SOCK_RAW needs CAP_NET_RAW, which an unprivileged account never
# has — denied sandbox or not, so it cannot be evidence. Kept as defence in
# depth in case the process ever acquires the capability.
check("socket(AF_PACKET) raw", raw_socket, discriminating=False)
check("socket(AF_NETLINK)", lambda: socket.socket(socket.AF_NETLINK, socket.SOCK_RAW, 0)
      if hasattr(socket, "AF_NETLINK") else (_ for _ in ()).throw(NotImplementedError("Linux-only")))
check("UDP socket", lambda: socket.socket(socket.AF_INET, socket.SOCK_DGRAM))
check("DNS resolution", lambda: socket.getaddrinfo("example.com", 80))
check("subprocess curl", run_curl)

print("\n=== Watching or attacking other processes ===")


def other_pid_environ():
    if not os.path.isdir("/proc"):
        raise NotImplementedError("/proc is Linux-only")
    # Any pid but our own; Gitea's will do, and it is the interesting one.
    return open("/proc/%d/environ" % gitea_pid, "rb").read(1)


def ptrace_gitea():
    """Attach to Gitea, which is how an app would read its session keys.

    PTRACE_SEIZE, not PTRACE_ATTACH: attaching sends SIGSTOP to the target,
    and in the control run — outside the sandbox, same account — that
    succeeds and freezes the live Gitea until this process exits. A
    verification tool must not take the service down to prove a point.
    SEIZE attaches without stopping, and we detach immediately either way.
    """
    try:
        libc = ctypes.CDLL("libc.so.6", use_errno=True)
    except OSError as e:
        raise NotImplementedError("no glibc to call ptrace through") from e
    PTRACE_SEIZE, PTRACE_DETACH = 0x4206, 17
    libc.ptrace.restype = ctypes.c_long
    libc.ptrace.argtypes = [ctypes.c_long] * 4
    if libc.ptrace(PTRACE_SEIZE, gitea_pid, 0, 0) == -1:
        raise OSError(ctypes.get_errno(), "ptrace refused")
    libc.ptrace(PTRACE_DETACH, gitea_pid, 0, 0)


def list_proc():
    if not os.path.isdir("/proc"):
        raise NotImplementedError("/proc is Linux-only")
    # Denying this is what replaces the PID namespace bubblewrap would have
    # given: an app that cannot enumerate /proc cannot find another process
    # to read or signal.
    return os.listdir("/proc")


check("list /proc", list_proc)
check("read Gitea's environ", other_pid_environ)
# Ubuntu enables Yama's ptrace_scope=1 by default, which already stops a
# process from tracing anything but its own children. Where that is on, this
# check cannot tell the sandbox apart from the kernel doing the work — so it
# is reported either way, but only counted as evidence where Yama is off.
try:
    with open("/proc/sys/kernel/yama/ptrace_scope") as f:
        yama = int(f.read().strip())
except (OSError, ValueError):
    yama = 0
check("ptrace on Gitea" + (" (Yama already blocks it)" if yama else ""),
      ptrace_gitea, discriminating=(yama == 0))
check("signal Gitea", lambda: os.kill(gitea_pid, 0))
check("kill(-1) — everything this user owns", lambda: os.kill(-1, 0))

print("\n=== Resource limits ===")


def exceed_memory():
    soft, _ = resource.getrlimit(resource.RLIMIT_DATA)
    if soft == resource.RLIM_INFINITY:
        raise NotImplementedError("no RLIMIT_DATA is set")
    # Ask for twice the cap in one go. Under a working limit this raises
    # MemoryError rather than pushing the host into swap.
    bytearray(soft * 2)


check("allocate past the memory limit", exceed_memory)

print("\n=== The app still has to work ===")
check("write in its own directory",
      lambda: open(os.path.join(workdir, "probe.tmp"), "w").write("x"), expect="allow")
check("read its own /proc/self",
      lambda: open("/proc/self/status").read(1) if os.path.isdir("/proc")
      else (_ for _ in ()).throw(NotImplementedError("Linux-only")), expect="allow")
check("create a unix socket (its own)",
      lambda: socket.socket(socket.AF_UNIX, socket.SOCK_STREAM), expect="allow")
check("import ssl", lambda: __import__("ssl"), expect="allow")
check("read /etc/passwd (pwd, getpass)", lambda: open("/etc/passwd").read(1), expect="allow")
check("resolve the current user", lambda: __import__("getpass").getuser(), expect="allow")

print("\n=== Gitea's environment must not have leaked in ===")
leaked = sorted(k for k in os.environ
                if k.startswith("GITEA") or any(word in k.upper()
                for word in ("SECRET", "PASSWD", "PASSWORD", "TOKEN")))
if leaked and not control:
    print("  FAIL leaked: %s" % ", ".join(leaked))
    failures.append("environment")
else:
    print("  ok   %d variables, none of them Gitea's: %s" % (len(os.environ), sorted(os.environ)))

print("\n" + "=" * 66)
if skipped:
    print("skipped (not present on this host, proves nothing): %s" % ", ".join(skipped))
if failures:
    if control:
        print("CONTROL RUN FAILED on %d check(s): %s" % (len(failures), ", ".join(failures)))
        print("These checks cannot tell a sandboxed process from an unsandboxed one,")
        print("so they prove nothing in the real run either. Fix the probe first.")
    else:
        print("FAILED %d check(s): %s" % (len(failures), ", ".join(failures)))
        print("The sandbox is NOT doing what the design claims.")
        print("Do not run department apps on this host until it is.")
    sys.exit(1)
print("CONTROL RUN OK: the probe detects an unprotected process." if control
      else "All checks passed: the sandbox holds.")
