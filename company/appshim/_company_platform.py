# Installed into every app's virtualenv by the platform (company/appshim.go),
# so an app that opens SQLite at a path of its own still gets the persistent
# database instead of failing against its read-only code directory.
import os

_DB_PATH = os.environ.get("DB_PATH")
# Set for the app only: the platform's own runners share this venv and must
# open exactly the paths they ask for.
_APP_DIR = os.environ.get("COMPANY_APP_DIR")


def _inside(path, root):
    return path == root or path.startswith(root.rstrip(os.sep) + os.sep)


def _target(path):
    if path in ("", ":memory:"):
        return path
    real = os.path.realpath(path)
    if _inside(real, _DATA_DIR):
        return path
    parent = os.path.dirname(real)
    if _inside(real, _CODE_DIR) or not os.path.isdir(parent) or not os.access(parent, os.W_OK):
        return _DB_PATH
    return path


def _uri_target(uri):
    from urllib.parse import quote, unquote

    if not uri.startswith("file:"):
        return uri
    path, sep, query = uri[len("file:"):].partition("?")
    if "mode=memory" in query:
        return uri
    if path.startswith("//"):  # file://host/path
        path = "/" + path[2:].partition("/")[2]
    path = unquote(path)
    new = _target(path)
    if new == path:
        return uri
    return "file:" + quote(new) + sep + query


def _wrap(connect):
    announced = set()

    def wrapper(database, *args, **kwargs):
        try:
            uri = kwargs.get("uri", args[6] if len(args) > 6 else False)
            original = os.fsdecode(database)
            new = _uri_target(original) if uri else _target(original)
            if new != original:
                if original not in announced:
                    announced.add(original)
                    print(f"[platform] SQLite database {original!r} is stored at {_DB_PATH}", flush=True)
                database = new
        except Exception:  # never break an app over a path it could have opened itself
            pass
        return connect(database, *args, **kwargs)

    wrapper.__wrapped__ = connect
    return wrapper


# state: what an app keeps in memory that it wants back after a stop. The
# platform stops apps — over a limit, under memory pressure, for a deploy —
# and SIGTERM lets uvicorn finish its requests but nothing in the process
# outlives it. Anything put here is written to the data directory when the
# process exits normally (which a SIGTERM stop is) and every 30 seconds
# in between, and is loaded before the app's own code runs:
#
#     import _company_platform as platform
#     platform.state["cache"] = compute()      # survives a stop
#     platform.state.get("cache")              # there again after a start
#
# Values must be JSON: this is a saved dictionary, not a memory image.
state = {}
_STATE_PATH = None


def _save_state():
    if _STATE_PATH is None or (not state and not os.path.exists(_STATE_PATH)):
        return  # nothing kept, nothing to write
    try:
        import json
        tmp = _STATE_PATH + ".tmp"
        with open(tmp, "w", encoding="utf-8") as f:
            json.dump(state, f, ensure_ascii=False)
        os.replace(tmp, _STATE_PATH)
    except Exception as e:  # never take an app down over its keepsakes
        print(f"[platform] state was not saved: {e}", flush=True)


def _load_state():
    try:
        import json
        with open(_STATE_PATH, encoding="utf-8") as f:
            loaded = json.load(f)
        if isinstance(loaded, dict):
            state.update(loaded)
            print(f"[platform] state restored from {_STATE_PATH} ({len(loaded)} keys)", flush=True)
    except FileNotFoundError:
        pass
    except Exception as e:
        print(f"[platform] state was not restored: {e}", flush=True)


def _autosave():
    import threading
    import time

    def loop():
        while True:
            time.sleep(30)
            _save_state()

    threading.Thread(target=loop, name="platform-state-autosave", daemon=True).start()


# TCP: under bubblewrap the app has no network, only the broker's socket, so
# a library dialing a host itself (LDAP to the company directory, a database)
# is sent there instead: names resolve to placeholder addresses that remember
# the name, and a connect becomes CONNECT host:port, which the broker allows
# or refuses by the app's outbound rules (company/broker_tcp.go).
_BROKER = os.environ.get("BROKER_SOCKET")
_FAKE_NET = "198.18.0.0"  # RFC 2544 benchmarking range: never a real host here


def _install_tcp_redirect():
    import errno
    import ipaddress
    import socket
    import threading

    real_getaddrinfo = socket.getaddrinfo
    real_connect = socket.socket.connect
    real_getpeername = socket.socket.getpeername
    real_close = socket.socket.close
    real_setsockopt = socket.socket.setsockopt
    real_getsockopt = socket.socket.getsockopt
    lock = threading.Lock()
    by_name, by_ip, peers = {}, {}, {}
    announced = set()

    def is_name(host):
        if not host or host.lower() in ("localhost", "localhost.localdomain"):
            return False
        try:
            ipaddress.ip_address(host.split("%")[0])
            return False
        except ValueError:
            return True

    def placeholder(name):
        name = name.lower().rstrip(".")
        with lock:
            ip = by_name.get(name)
            if ip is None:
                ip = str(ipaddress.IPv4Address(_FAKE_NET) + len(by_name) + 1)
                by_name[name], by_ip[ip] = ip, name
            return ip

    def getaddrinfo(host, port, family=0, type=0, proto=0, flags=0):
        name = host.decode() if isinstance(host, bytes) else host
        if isinstance(name, str) and is_name(name) and family in (0, socket.AF_INET):
            # AI_ADDRCONFIG would drop the answer: the sandbox has only loopback.
            return real_getaddrinfo(placeholder(name), port, socket.AF_INET, type, proto, flags & ~socket.AI_ADDRCONFIG)
        return real_getaddrinfo(host, port, family, type, proto, flags)

    def gethostbyname(host):
        return placeholder(host) if is_name(host) else real_gethostbyname(host)

    real_gethostbyname = socket.gethostbyname

    def destination(sock, address):
        if sock.family not in (socket.AF_INET, socket.AF_INET6) or sock.type != socket.SOCK_STREAM:
            return None
        host, port = address[0], address[1]
        if host in by_ip:
            return by_ip[host], port
        if is_name(host):
            return host, port
        if ipaddress.ip_address(host.split("%")[0]).is_loopback:
            return None
        return host, port

    def tunnel(sock, host, port):
        authority = f"[{host}]:{port}" if ":" in host else f"{host}:{port}"
        broker = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        try:
            broker.settimeout(sock.gettimeout() or 30)
            real_connect(broker, _BROKER)
            broker.sendall(f"CONNECT {authority} HTTP/1.1\r\nHost: {authority}\r\n\r\n".encode())
            head = b""
            while not head.endswith(b"\r\n\r\n"):  # a byte at a time: what follows is the server's
                byte = broker.recv(1)
                if not byte or len(head) > 8192:
                    raise ConnectionRefusedError(errno.ECONNREFUSED, f"[platform] the outbound broker closed the connection to {authority}")
                head += byte
            status = head.split(b"\r\n", 1)[0].split(b" ")
            if len(status) < 2 or status[1] != b"200":
                try:
                    body = broker.recv(4096).decode(errors="replace").strip()
                except OSError:
                    body = ""
                reason = body or b" ".join(status[1:]).decode(errors="replace")
                if authority not in announced:
                    announced.add(authority)
                    print(f"[platform] outbound TCP to {authority} refused: {reason}", flush=True)
                raise ConnectionRefusedError(errno.ECONNREFUSED, f"[platform] {reason}")
            os.dup2(broker.fileno(), sock.fileno(), inheritable=sock.get_inheritable())
            sock.settimeout(sock.gettimeout())  # the descriptor came with the broker's blocking mode
        finally:
            broker.close()

    def connect(self, address):
        target = destination(self, address)
        if target is None:
            return real_connect(self, address)
        tunnel(self, *target)
        with lock:
            peers[self.fileno()] = tuple(address)

    def connect_ex(self, address):
        try:
            connect(self, address)
            return 0
        except OSError as e:
            return e.errno or errno.ECONNREFUSED

    def getpeername(self):
        peer = real_getpeername(self)
        if isinstance(peer, (str, bytes)):  # a tunnel is a unix socket underneath
            with lock:
                return peers.get(self.fileno(), peer)
        return peer

    def tunneled(sock):
        with lock:
            return sock.fileno() in peers

    # TCP_NODELAY, keepalive tuning: asyncio and LDAP clients set them, and a
    # unix socket refuses them. They mean nothing on the app's side of a tunnel.
    def setsockopt(self, level, option, *value):
        try:
            return real_setsockopt(self, level, option, *value)
        except OSError:
            if not tunneled(self):
                raise

    def getsockopt(self, level, option, *buflen):
        if level == socket.IPPROTO_TCP and tunneled(self):
            return bytes(buflen[0]) if buflen else 0
        return real_getsockopt(self, level, option, *buflen)

    def close(self):
        with lock:
            peers.pop(self.fileno(), None)
        real_close(self)

    socket.getaddrinfo = getaddrinfo
    socket.gethostbyname = gethostbyname
    socket.socket.connect = connect
    socket.socket.connect_ex = connect_ex
    socket.socket.getpeername = getpeername
    socket.socket.close = close
    socket.socket.setsockopt = setsockopt
    socket.socket.getsockopt = getsockopt


if _BROKER and _APP_DIR and os.environ.get("BROKER_TCP") == "1":
    _install_tcp_redirect()


if _DB_PATH and _APP_DIR:
    import atexit
    import sqlite3
    import sqlite3.dbapi2

    _DATA_DIR = os.path.realpath(os.path.dirname(_DB_PATH))
    _CODE_DIR = os.path.realpath(_APP_DIR)
    sqlite3.connect = sqlite3.dbapi2.connect = _wrap(sqlite3.dbapi2.connect)
    _STATE_PATH = os.path.join(_DATA_DIR, "platform-state.json")
    _load_state()
    atexit.register(_save_state)  # after uvicorn's own graceful shutdown, on the way out
    _autosave()
