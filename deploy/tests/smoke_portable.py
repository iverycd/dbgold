"""Run against a freshly extracted native package on the target OS (Python 3)."""

import argparse
import contextlib
import json
import os
from pathlib import Path
import shutil
import signal
import socket
import sqlite3
import subprocess
import tempfile
import time
import urllib.error
import urllib.request


URL = "http://127.0.0.1:18089"
EXE = "dbgold.exe" if os.name == "nt" else "dbgold"


def request(path, data=None, headers=None):
    req = urllib.request.Request(URL + path, data=data, headers=headers or {})
    with urllib.request.urlopen(req, timeout=5) as response:
        return response.read()


@contextlib.contextmanager
def running(root, cwd):
    if os.name == "nt":
        command = [str(root / EXE), "portable", "--no-browser"]
    else:
        command = [str(root / "start.sh")]
    with tempfile.TemporaryFile() as output:
        proc = subprocess.Popen(
            command, cwd=cwd, stdout=output, stderr=subprocess.STDOUT,
            creationflags=subprocess.CREATE_NEW_PROCESS_GROUP if os.name == "nt" else 0,
        )
        try:
            deadline = time.monotonic() + 120
            while time.monotonic() < deadline:
                if proc.poll() is not None:
                    raise RuntimeError(f"portable process exited with code {proc.returncode}")
                try:
                    if json.loads(request("/api/health/ready"))["status"] == "ok":
                        break
                except (OSError, urllib.error.URLError):
                    pass
                time.sleep(0.2)
            else:
                raise RuntimeError("portable instance did not become ready")
            yield
        finally:
            if proc.poll() is None:
                proc.send_signal(signal.CTRL_BREAK_EVENT if os.name == "nt" else signal.SIGTERM)
                try:
                    proc.wait(timeout=30)
                except subprocess.TimeoutExpired:
                    proc.kill()
                    proc.wait()
                    raise


def login(root):
    config = dict(line.split("=", 1) for line in (root / "config/dbgold.env")
                  .read_text(encoding="utf-8").splitlines() if "=" in line and not line.startswith("#"))
    data = json.dumps({"username": "admin", "password": config["ADMIN_PASS"]}).encode()
    token = json.loads(request("/api/auth/login", data, {"Content-Type": "application/json"}))["token"]
    return {"Authorization": "Bearer " + token}


def smoke(package, work):
    # Use disposable copies exclusively. An installed directory can contain real credentials.
    for name in ("data", "config", "uploads", "logs", "backups", "tmp"):
        if any((package / name).iterdir()):
            raise ValueError("Supply a freshly extracted package with empty runtime directories")
    with socket.socket() as probe:
        probe.bind(("127.0.0.1", 18089))
    root = work / "第一次 解压"
    shutil.copytree(package, root, symlinks=True)
    cwd = work / "other-cwd"
    cwd.mkdir()
    java = root / "runtime/bin" / ("java.exe" if os.name == "nt" else "java")
    classpath = os.pathsep.join(str(root / "lib" / jar) for jar in ("dbgold-oscar-bridge.jar", "oscarJDBC8.jar"))
    subprocess.run([str(java), "-Djava.io.tmpdir=" + str(root / "tmp"), "-XX:-UsePerfData",
                    "-cp", classpath, "com.dbgold.oscar.BridgeMain"],
                   stdin=subprocess.DEVNULL, cwd=cwd, check=True, timeout=60)
    with running(root, cwd):
        headers = login(root)
        original_config = (root / "config/dbgold.env").read_bytes()
        assert b"dbgold" in request("/").lower()
        duplicate = subprocess.run([str(root / EXE), "portable", "--no-browser"], cwd=cwd,
                                   capture_output=True, timeout=15)
        assert duplicate.returncode != 0, "second instance unexpectedly started"
        conn = dict(name="portable-smoke", db_type="mysql", host="127.0.0.1", port=3306,
                    username="fixture", password="fixture")
        request("/api/connections", json.dumps(conn).encode(), {**headers, "Content-Type": "application/json"})
        body = (b'--dbgold\r\nContent-Disposition: form-data; name="file"; filename="fixture.sql"\r\n'
                b'Content-Type: application/octet-stream\r\n\r\nSELECT 1;\r\n--dbgold--\r\n')
        upload = json.loads(request("/api/tickets/upload", body,
                                    {"Content-Type": "multipart/form-data; boundary=dbgold"}))
        attachment = Path(upload["stored_path"])
        assert attachment.parent == Path("uploads") and (root / attachment).read_bytes() == b"SELECT 1;"
        assert b"JWT_SECRET=" not in request("/config/dbgold.env")
    # Add a completed history fixture to the stopped disposable database.
    with contextlib.closing(sqlite3.connect(root / "data/dbgold.db")) as conn:
        conn.execute("INSERT INTO data_migration_jobs(job_id,status) VALUES (?,?)", ("move-check", "done"))
        conn.commit()
    moved = root.rename(work / "已搬移 dbgold")
    upgrade = [str(package / EXE), "portable-upgrade", "--target", str(moved), "--confirm-no-running-tasks"]
    database = moved / "data/dbgold.db"
    saved = database.read_bytes()
    with socket.socket() as occupied:
        option = socket.SO_EXCLUSIVEADDRUSE if os.name == "nt" else socket.SO_REUSEADDR
        occupied.setsockopt(socket.SOL_SOCKET, option, 1)
        occupied.bind(("127.0.0.1", 18089))
        occupied.listen(1)
        failed = subprocess.run(upgrade, cwd=cwd, capture_output=True, timeout=240)
        assert failed.returncode != 0 and b"previous program and data restored" in failed.stderr, failed.stderr
    assert database.read_bytes() == saved
    subprocess.run(upgrade, cwd=cwd, timeout=240, check=True)
    with running(moved, cwd):
        assert original_config == (moved / "config/dbgold.env").read_bytes()
        assert json.loads(request("/api/auth/me", headers=headers))["username"] == "admin"
        headers = login(moved)
        assert any(c["name"] == "portable-smoke" for c in json.loads(request("/api/connections", headers=headers)))
        jobs = json.loads(request("/api/migration/data-migrate/jobs", headers=headers))["items"]
        assert any(job["job_id"] == "move-check" for job in jobs)
        assert (moved / attachment).read_bytes() == b"SELECT 1;"
    assert not list(cwd.iterdir()), "dbgold wrote files in the caller's working directory"


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("package", type=Path, help="freshly extracted dbgold directory")
    args = parser.parse_args()
    with tempfile.TemporaryDirectory() as work:
        smoke(args.package.resolve(), Path(work))
    print("Portable startup, upload, relocation and upgrade passed.")
