"""Optional, private, bounded tree-feature snapshots. No numerical imports.

Writers and the idle janitor share a nonblocking process lock. All file operations
are relative to an opened private directory; resource input never supplies paths.
"""
import contextlib
import csv
import fcntl
import io
import os
import re
import stat
import sys
import time
import uuid

MAX_ROWS = 256
MAX_FILE_BYTES = 1024 * 1024
SNAPSHOT = re.compile(r"(?:xgboost|lightgbm)-[0-9a-f]{64}\.csv\Z")
TEMPORARY = re.compile(r"\.feature-[0-9a-f]{32}\.tmp\Z")


def configuration():
    root = os.environ.get("HPAPLUS_FEATURE_DUMP_DIR", "")
    if not root:
        return None
    if not os.path.isabs(root) or os.path.normpath(root) != root or root == "/":
        raise ValueError("invalid diagnostic configuration")
    limits = []
    for key, default, minimum, maximum in (
        ("MAX_FILES", 64, 1, 256),
        ("MAX_BYTES", 16 * MAX_FILE_BYTES, MAX_FILE_BYTES, 64 * MAX_FILE_BYTES),
        ("MAX_AGE_SECONDS", 3600, 60, 86400),
    ):
        raw = os.environ.get("HPAPLUS_FEATURE_DUMP_" + key, str(default))
        if not re.fullmatch(r"[0-9]{1,10}", raw):
            raise ValueError("invalid diagnostic configuration")
        value = int(raw)
        if not minimum <= value <= maximum:
            raise ValueError("invalid diagnostic configuration")
        limits.append(value)
    return (root, *limits)


def private(info, directory=False):
    kind = stat.S_ISDIR if directory else stat.S_ISREG
    if (not kind(info.st_mode) or info.st_uid != os.getuid()
            or info.st_mode & 0o077 or (not directory and info.st_nlink != 1)):
        raise ValueError("unsafe diagnostic storage")


@contextlib.contextmanager
def locked(root, create):
    if create:
        try:
            os.mkdir(root, 0o700)
        except FileExistsError:
            pass
    # The administrator owns ancestor directories; the final component must not
    # be a symlink. dir_fd prevents a concurrent rename from redirecting writes.
    directory = os.open(root, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        private(os.fstat(directory), directory=True)
        lock = os.open(".lock", os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW | os.O_NONBLOCK,
                       0o600, dir_fd=directory)
        try:
            private(os.fstat(lock))
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            yield directory
        finally:
            os.close(lock)
    finally:
        os.close(directory)


def retained(directory, max_age, now):
    names = os.listdir(directory)
    if len(names) > 4096:
        raise ValueError("diagnostic directory is not dedicated")
    snapshots = []
    for name in names:
        if not (SNAPSHOT.fullmatch(name) or TEMPORARY.fullmatch(name)):
            continue  # Never recursively remove or touch unrelated files.
        info = os.stat(name, dir_fd=directory, follow_symlinks=False)
        private(info)
        if (TEMPORARY.fullmatch(name) or now - info.st_mtime >= max_age
                or info.st_mtime > now or info.st_size > MAX_FILE_BYTES):
            os.unlink(name, dir_fd=directory)
        else:
            snapshots.append((info.st_mtime, name, info.st_size))
    return sorted(snapshots)


def trim(directory, snapshots, max_files, max_bytes):
    total = sum(item[2] for item in snapshots)
    while len(snapshots) > max_files or total > max_bytes:
        _, name, size = snapshots.pop(0)
        os.unlink(name, dir_fd=directory)
        total -= size


def encode_rows(header, rows):
    output = io.StringIO()
    csv.writer(output).writerow(header)
    size = len(output.getvalue().encode("utf-8"))
    if size > MAX_FILE_BYTES:
        return None
    for row in rows[-MAX_ROWS:]:
        line = io.StringIO()
        csv.writer(line).writerow(row)
        value = line.getvalue()
        size += len(value.encode("utf-8"))
        if size > MAX_FILE_BYTES:
            break
        output.write(value)
    return output.getvalue().encode("utf-8")


def dump_features(family, scope, header, rows):
    """Best effort: diagnostics cannot change the forecast result or expose paths."""
    try:
        config = configuration()
        if config is None or family not in ("xgboost", "lightgbm"):
            return
        if not isinstance(scope, str) or not re.fullmatch(r"[0-9a-f]{64}", scope):
            return  # Missing identity must never collapse into a shared filename.
        data = encode_rows(header, rows)
        if data is None:
            return
        root, max_files, max_bytes, max_age = config
        name = family + "-" + scope + ".csv"
        with locked(root, create=True) as directory:
            snapshots = retained(directory, max_age, time.time())
            others = [item for item in snapshots if item[1] != name]
            trim(directory, others, max_files - 1, max_bytes - len(data))
            temporary = ".feature-" + uuid.uuid4().hex + ".tmp"
            try:
                fd = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW,
                             0o600, dir_fd=directory)
                with os.fdopen(fd, "wb") as handle:
                    handle.write(data)
                os.replace(temporary, name, src_dir_fd=directory, dst_dir_fd=directory)
            finally:
                try:
                    os.unlink(temporary, dir_fd=directory)
                except FileNotFoundError:
                    pass
    except BlockingIOError:
        pass  # Skip a diagnostic instead of delaying a forecast on another writer.
    except (OSError, ValueError, TypeError, OverflowError):
        print("Feature diagnostic unavailable", file=sys.stderr)


def cleanup():
    config = configuration()
    if config is None:
        return
    root, max_files, max_bytes, max_age = config
    try:
        with locked(root, create=False) as directory:
            snapshots = retained(directory, max_age, time.time())
            trim(directory, snapshots, max_files, max_bytes)
    except (FileNotFoundError, BlockingIOError):
        pass


if __name__ == "__main__":
    try:
        cleanup()
    except (OSError, ValueError):
        print("Feature diagnostic cleanup unavailable", file=sys.stderr)
        sys.exit(1)
