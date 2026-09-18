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


if _DB_PATH and _APP_DIR:
    import sqlite3
    import sqlite3.dbapi2

    _DATA_DIR = os.path.realpath(os.path.dirname(_DB_PATH))
    _CODE_DIR = os.path.realpath(_APP_DIR)
    sqlite3.connect = sqlite3.dbapi2.connect = _wrap(sqlite3.dbapi2.connect)
