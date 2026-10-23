#!/usr/bin/env python3
"""Exercise deployment orchestration without SSH, Java, or a running dbgold."""

import json
import hashlib
import os
from pathlib import Path
import shutil
import subprocess
import tarfile
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[2]
BASH = shutil.which("bash")
PYTHON = shutil.which("python3")


FAKE_COMMAND = r'''#!{python}
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys

name = Path(sys.argv[0]).name
args = sys.argv[1:]
with open(os.environ["DEPLOY_TEST_LOG"], "a") as log:
    log.write(json.dumps({{"command": name, "args": args}}) + "\n")
if name == "go":
    output = Path(args[args.index("-o") + 1])
    output.write_text("fake backend\n")
    output.chmod(0o755)
elif name == "npm":
    Path("dist").mkdir(exist_ok=True)
    Path("dist/index.html").write_text("<html>test</html>\n")
elif name == "javac":
    if os.environ.get("FAIL_JAVAC"):
        sys.exit(1)
elif name == "jar":
    output = Path(args[args.index("--file") + 1])
    output.parent.mkdir(parents=True, exist_ok=True)
    output.write_text("fresh bridge\n")
elif name == "sshpass":
    if args[0] == "-p":
        args = args[2:]
    elif args[0] == "-e":
        args = args[1:]
    os.execvp(args[0], args)
elif name == "ssh":
    # ssh options all precede user@host; the remainder is the remote command.
    for index, arg in enumerate(args):
        if "@" in arg:
            args = args[index + 1:]
            break
    command = " ".join(args)
    payload = sys.stdin.buffer.read()
    is_upload = "cat >" in command and "bundle.tar.gz" in command
    with open(os.environ["DEPLOY_TEST_LOG"], "a") as log:
        log.write(json.dumps({{"command": "remote", "text": command,
                               "stdin": "<archive>" if is_upload else payload.decode(),
                               "upload": is_upload}}) + "\n")
    if is_upload:
        if os.environ.get("FAIL_UPLOAD"):
            sys.exit(1)
        Path(os.environ["DEPLOY_TEST_CAPTURE"], "bundle.tar.gz").write_bytes(payload)
    if "preflight" in command:
        sys.exit(1 if os.environ.get("FAIL_PREFLIGHT") else 0)
    if "activate" in command:
        sys.exit(1 if os.environ.get("FAIL_ACTIVATE") else 0)
    if any(token in command for token in ["pgrep", "nohup", "kill "]):
        raise SystemExit("test refused to run process control over mock SSH")
    result = subprocess.run(command, input=payload, shell=True)
    sys.exit(result.returncode)
elif name == "scp":
    if os.environ.get("FAIL_UPLOAD"):
        sys.exit(1)
    source, destination = args[-2:]
    if source.endswith((".tar", ".tar.gz", ".tgz")):
        shutil.copy2(source, Path(os.environ["DEPLOY_TEST_CAPTURE"]) / Path(source).name)
    destination = destination.split(":", 1)[1]
    # scp may include shell quotes for a path containing spaces.
    import shlex
    destination = shlex.split(destination)[0]
    target = Path(destination)
    if target.is_dir():
        target = target / Path(source).name
    shutil.copy2(source, target)
else:
    raise SystemExit("unexpected mock command: " + name)
'''


class DeployFixture(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="dbgold deploy test ")
        self.addCleanup(self.temporary.cleanup)
        self.base = Path(self.temporary.name).resolve()
        self.repo = self.base / "repo"
        self.repo.mkdir()
        self.bin = self.base / "bin"
        self.bin.mkdir()
        self.remote = self.base / "remote service"
        self.remote.mkdir()
        self.log = self.base / "commands.jsonl"
        self.log.touch()
        self.captures = self.base / "captures"
        self.captures.mkdir()
        for relative in ["deploy.sh", "deploy/remote-deploy.sh", "jdbcbridge/java/build.sh"]:
            source = ROOT / relative
            target = self.repo / relative
            target.parent.mkdir(parents=True, exist_ok=True)
            if source.is_file():
                shutil.copy2(source, target)
        (self.repo / "frontend").mkdir()
        vendor = self.repo / "third_party/oscar"
        vendor.mkdir(parents=True)
        (vendor / "oscarJDBC8.jar").write_text("vendor JDBC driver\n")
        (vendor / "README.md").write_text("vendor driver license notice\n")
        (vendor / "LICENSE").write_text("vendor driver license terms\n")
        bridge = self.repo / "jdbcbridge/java/build/dbgold-oscar-bridge.jar"
        bridge.parent.mkdir(parents=True)
        bridge.write_text("old bridge\n")
        for name in ["go", "npm", "javac", "jar", "sshpass", "ssh", "scp"]:
            command = self.bin / name
            command.write_text(FAKE_COMMAND.format(python=PYTHON))
            command.chmod(0o755)
        self.env = dict(os.environ, PATH=str(self.bin) + os.pathsep + os.environ["PATH"],
                        GO_BIN=str(self.bin / "go"), DEPLOY_TEST_LOG=str(self.log),
                        DEPLOY_TEST_CAPTURE=str(self.captures))

    def events(self):
        return [json.loads(line) for line in self.log.read_text().splitlines()]

    def run_deploy(self, extra_args=(), **variables):
        return subprocess.run([BASH, str(self.repo / "deploy.sh"), "-h", "example.invalid",
                               "-u", "test", "-p", "unused", "-d", str(self.remote), *extra_args],
                              cwd=self.repo, env=dict(self.env, **variables), text=True,
                              stdout=subprocess.PIPE, stderr=subprocess.STDOUT)

    def remote_calls(self):
        return [entry for entry in self.events() if entry["command"] == "remote"]

    def assert_no_activation(self):
        self.assertFalse(any("activate" in event["text"] or "activate" in event["stdin"]
                             for event in self.remote_calls()), self.log.read_text())


class LocalDeploymentTests(DeployFixture):
    def test_bridge_is_rebuilt_and_both_jars_are_uploaded_before_activation(self):
        result = self.run_deploy()
        self.assertEqual(result.returncode, 0, result.stdout)
        commands = [event["command"] for event in self.events()]
        self.assertIn("javac", commands)
        self.assertIn("jar", commands)
        archives = list(self.captures.iterdir())
        self.assertEqual(len(archives), 1, self.log.read_text())
        with tarfile.open(archives[0]) as archive:
            files = {member.name.removeprefix("./"): member for member in archive.getmembers()}
            for required in ["dbgold", "dist/index.html", "lib/dbgold-oscar-bridge.jar",
                             "lib/oscarJDBC8.jar"]:
                self.assertIn(required, files)
            self.assertEqual(archive.extractfile(files["lib/dbgold-oscar-bridge.jar"]).read(),
                             b"fresh bridge\n")
            self.assertIn("lib/OSCAR-JDBC-NOTICE.md", files)
            self.assertIn("lib/OSCAR-JDBC-LICENSE", files)
            license_body = archive.extractfile(files["lib/OSCAR-JDBC-LICENSE"]).read()
            self.assertEqual(license_body, (self.repo / "third_party/oscar/LICENSE").read_bytes())
            manifest = archive.extractfile(files["SHA256SUMS"]).read().decode()
            manifest_hashes = {filename.removeprefix("./"): digest
                               for digest, filename in (line.split(None, 1) for line in manifest.splitlines())}
            self.assertEqual(manifest_hashes["lib/OSCAR-JDBC-LICENSE"], hashlib.sha256(license_body).hexdigest())
        calls = self.remote_calls()
        preflight = next(i for i, call in enumerate(calls)
                         if "preflight" in call["text"] or "preflight" in call["stdin"])
        activation = next(i for i, call in enumerate(calls)
                          if "activate" in call["text"] or "activate" in call["stdin"])
        self.assertLess(preflight, activation)
        # A second deployment must rebuild even though the first JAR is present.
        result = self.run_deploy()
        self.assertEqual(result.returncode, 0, result.stdout)
        self.assertEqual(sum(event["command"] == "jar" for event in self.events()), 2)

    def test_missing_jdk_is_rejected_before_build_or_ssh(self):
        (self.bin / "javac").unlink()
        for command in ["bash", "dirname"]:
            (self.bin / command).symlink_to(shutil.which(command))
        result = self.run_deploy(PATH=str(self.bin))
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertIn("javac", result.stdout)
        self.assertEqual(self.events(), [])

    def test_missing_jdbc_driver_is_rejected_before_ssh(self):
        (self.repo / "third_party/oscar/oscarJDBC8.jar").unlink()
        result = self.run_deploy()
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertEqual(self.remote_calls(), [])

    def test_missing_jdbc_license_does_not_block_deployment_or_enter_manifest(self):
        (self.repo / "third_party/oscar/LICENSE").unlink()
        result = self.run_deploy()
        self.assertEqual(result.returncode, 0, result.stdout)
        self.assertIn("✅ 部署完成", result.stdout)
        with tarfile.open(self.captures / "bundle.tar.gz") as archive:
            files = {member.name.removeprefix("./"): member for member in archive.getmembers()}
            self.assertNotIn("lib/OSCAR-JDBC-LICENSE", files)
            self.assertNotIn("OSCAR-JDBC-LICENSE", archive.extractfile(files["SHA256SUMS"]).read().decode())

    def test_bridge_compilation_failure_does_not_touch_remote(self):
        result = self.run_deploy(FAIL_JAVAC="1")
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertEqual(self.remote_calls(), [])

    def test_upload_failure_does_not_activate(self):
        result = self.run_deploy(FAIL_UPLOAD="1")
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assert_no_activation()
        self.assertFalse(any("preflight" in event["text"] or "preflight" in event["stdin"]
                             for event in self.remote_calls()))

    def test_remote_preflight_failure_leaves_old_service_running(self):
        result = self.run_deploy(FAIL_PREFLIGHT="1")
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assert_no_activation()

    def test_explicit_java_path_reaches_both_remote_phases(self):
        java = "/opt/custom java/bin/java"
        result = self.run_deploy(extra_args=["-j", java])
        self.assertEqual(result.returncode, 0, result.stdout)
        phases = [call for call in self.remote_calls()
                  if "preflight" in call["text"] or "activate" in call["text"]]
        self.assertEqual(len(phases), 2)
        self.assertTrue(all(java in call["text"] for call in phases), phases)

    def test_activation_failure_is_not_reported_as_success(self):
        result = self.run_deploy(FAIL_ACTIVATE="1")
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertNotIn("✅ 部署完成", result.stdout)


class RemoteDeploymentTests(DeployFixture):
    def run_helper(self, script, **variables):
        return subprocess.run([BASH, "-c", 'set -euo pipefail\nsource "$DEPLOY_HELPER"\n' + script],
                              env=dict(self.env, DEPLOY_HELPER=str(ROOT / "deploy/remote-deploy.sh"),
                                       TEST_DEPLOY_DIR=str(self.remote), **variables),
                              text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)

    def make_java(self, relative):
        path = self.base / relative
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text("#!/bin/sh\nexit 0\n")
        path.chmod(0o755)
        return path

    def make_stage(self):
        stage = self.remote / ".deploy.fixture"
        stage.mkdir()
        for relative in ["dbgold", "dist/index.html", "lib/dbgold-oscar-bridge.jar",
                         "lib/oscarJDBC8.jar", "lib/OSCAR-JDBC-NOTICE.md", "lib/OSCAR-JDBC-LICENSE"]:
            path = stage / relative
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text("test artifact " + relative + "\n")
        (stage / "dbgold").write_text("""#!/bin/sh
if [ "${1:-}" = healthcheck ]; then
  printf '%s\\n' "$*" >> "$DEPLOY_TEST_LOG"
  exit "${TEST_HEALTH_EXIT:-0}"
fi
printf '%s\\n' "$PORT" "$STATIC_DIR" "$OSCAR_JAVA_BIN" "$OSCAR_BRIDGE_JAR" "$OSCAR_JDBC_JAR" > "$DEPLOY_TEST_ENV"
""")
        (stage / "dbgold").chmod(0o755)
        self.write_checksums(stage)
        return stage

    def write_checksums(self, stage):
        (stage / "SHA256SUMS").write_text("".join(
            hashlib.sha256(path.read_bytes()).hexdigest() + "  " + str(path.relative_to(stage)) + "\n"
            for path in sorted(stage.rglob("*")) if path.is_file() and path.name != "SHA256SUMS"))

    def make_proc(self, address="0A01A8C0", inode="654321", state="0A"):
        proc = self.base / "proc"
        (proc / "12345/fd").mkdir(parents=True)
        (proc / "12345/net").mkdir()
        (proc / "12345/fd/3").symlink_to("socket:[654321]")
        (proc / "12345/net/tcp").write_text(
            "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt uid timeout inode\n"
            f"   0: {address}:{18089:04X} 00000000:0000 {state} 00000000:00000000 "
            f"00:00000000 00000000 1000 0 {inode} 1 0000000000000000 100 0 0 10 0\n")
        return proc

    @staticmethod
    def linux_preflight_mocks():
        return '''
uname() { case "$1" in -s) echo Linux ;; -m) echo x86_64 ;; esac; }
timeout() { shift; "$@"; }
'''

    def test_explicit_java_overrides_runtime_and_path(self):
        explicit = self.make_java("custom jdk/bin/java")
        self.make_java("remote service/runtime/bin/java")
        self.make_java("bin/java")
        result = self.run_helper('select_java "$TEST_DEPLOY_DIR" "$EXPLICIT_JAVA"',
                                 EXPLICIT_JAVA=str(explicit))
        self.assertEqual(result.returncode, 0, result.stdout)
        self.assertEqual(result.stdout.strip(), str(explicit.resolve()))

    def test_embedded_runtime_precedes_path_java(self):
        runtime = self.make_java("remote service/runtime/bin/java")
        self.make_java("bin/java")
        result = self.run_helper('select_java "$TEST_DEPLOY_DIR" ""')
        self.assertEqual(result.returncode, 0, result.stdout)
        self.assertEqual(result.stdout.strip(), str(runtime.resolve()))

    def test_path_java_is_resolved_to_absolute_path(self):
        java = self.make_java("bin/java")
        result = self.run_helper('select_java "$TEST_DEPLOY_DIR" ""')
        self.assertEqual(result.returncode, 0, result.stdout)
        self.assertEqual(result.stdout.strip(), str(java.resolve()))

    def test_invalid_explicit_java_does_not_silently_fall_back(self):
        self.make_java("remote service/runtime/bin/java")
        result = self.run_helper('select_java "$TEST_DEPLOY_DIR" "$TEST_DEPLOY_DIR/missing-java"')
        self.assertNotEqual(result.returncode, 0, result.stdout)

    def test_no_remote_java_is_an_actionable_failure(self):
        result = self.run_helper('select_java "$TEST_DEPLOY_DIR" ""', PATH=str(self.bin))
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertIn("Java", result.stdout)

    def test_preflight_checks_both_jars_and_starts_bridge(self):
        stage = self.make_stage()
        java = self.make_java("bin/java")
        java.write_text('#!/bin/sh\nprintf "%s\\n" "$@" > "$JAVA_ARGUMENTS"\n')
        arguments = self.base / "java-arguments"
        result = self.run_helper(self.linux_preflight_mocks() +
                                 'preflight "$STAGE" ""', STAGE=str(stage), JAVA_ARGUMENTS=str(arguments))
        self.assertEqual(result.returncode, 0, result.stdout)
        self.assertEqual(arguments.read_text().splitlines(), [
            "-cp", str(stage / "lib/dbgold-oscar-bridge.jar") + ":" + str(stage / "lib/oscarJDBC8.jar"),
            "com.dbgold.oscar.BridgeMain"])

    def test_preflight_accepts_bundle_without_optional_license(self):
        stage = self.make_stage()
        (stage / "lib/OSCAR-JDBC-LICENSE").unlink()
        self.write_checksums(stage)
        self.make_java("bin/java")
        result = self.run_helper(self.linux_preflight_mocks() +
                                 'preflight "$STAGE" ""', STAGE=str(stage))
        self.assertEqual(result.returncode, 0, result.stdout)
        self.assertNotIn("OSCAR-JDBC-LICENSE", (stage / "SHA256SUMS").read_text())

    def test_corrupt_upload_fails_before_old_service_is_stopped(self):
        stage = self.make_stage()
        self.make_java("bin/java")
        (stage / "lib/dbgold-oscar-bridge.jar").write_text("corrupted upload\n")
        stop_marker = self.base / "old-service-stopped"
        result = self.run_helper(self.linux_preflight_mocks() + '''
stop_instance() { touch "$STOP_MARKER"; }
activate "$STAGE" "" 18089
''', STAGE=str(stage), STOP_MARKER=str(stop_marker))
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertFalse(stop_marker.exists(), result.stdout)
        self.assertFalse((self.remote / "dbgold").exists())

    def test_java_startup_failure_fails_before_old_service_is_stopped(self):
        stage = self.make_stage()
        java = self.make_java("bin/java")
        java.write_text("#!/bin/sh\necho 'driver class not found' >&2\nexit 1\n")
        stop_marker = self.base / "old-service-stopped"
        result = self.run_helper(self.linux_preflight_mocks() + '''
stop_instance() { touch "$STOP_MARKER"; }
activate "$STAGE" "" 18089
''', STAGE=str(stage), STOP_MARKER=str(stop_marker))
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertIn("driver class not found", result.stdout)
        self.assertFalse(stop_marker.exists(), result.stdout)

    def test_pid_matching_excludes_other_installations(self):
        result = self.run_helper('''
pgrep() { printf '%s\\n' 101 202 303; }
readlink() {
  case "$2" in
    /proc/101/exe) echo "$TEST_DEPLOY_DIR/dbgold" ;;
    /proc/202/exe) echo "/opt/another-db/dbgold" ;;
    /proc/303/exe) echo "$TEST_DEPLOY_DIR/dbgold (deleted)" ;;
    *) return 1 ;;
  esac
}
instance_pids "$TEST_DEPLOY_DIR"
''')
        self.assertEqual(result.returncode, 0, result.stdout)
        self.assertEqual(result.stdout.splitlines(), ["101", "303"])

    def test_stop_only_signals_instances_in_this_directory(self):
        signal_log = self.base / "signals"
        result = self.run_helper('''
pgrep() { [[ ! -f "$SIGNAL_LOG" ]] && printf '%s\\n' 101 202; return 0; }
readlink() {
  case "$2" in
    /proc/101/exe) echo "$TEST_DEPLOY_DIR/dbgold" ;;
    /proc/202/exe) echo "/opt/another-db/dbgold" ;;
    *) return 1 ;;
  esac
}
kill() { printf '%s\\n' "$*" >> "$SIGNAL_LOG"; }
stop_instance "$TEST_DEPLOY_DIR"
''', SIGNAL_LOG=str(signal_log))
        self.assertEqual(result.returncode, 0, result.stdout)
        self.assertEqual(signal_log.read_text().splitlines(), ["-TERM 101"])

    def test_stop_escalates_only_current_instance_after_grace_period(self):
        signal_log = self.base / "signals"
        stopped = self.base / "stopped"
        result = self.run_helper('''
pgrep() { [[ -f "$STOPPED" ]] || printf '%s\\n' 101 202; }
readlink() {
  case "$2" in
    /proc/101/exe) echo "$TEST_DEPLOY_DIR/dbgold" ;;
    /proc/202/exe) echo "/opt/another-db/dbgold" ;;
    *) return 1 ;;
  esac
}
kill() {
  printf '%s\\n' "$*" >> "$SIGNAL_LOG"
  [[ "$1" != -KILL ]] || touch "$STOPPED"
}
sleep() { SECONDS=$((SECONDS + 21)); }
stop_instance "$TEST_DEPLOY_DIR"
''', SIGNAL_LOG=str(signal_log), STOPPED=str(stopped))
        self.assertEqual(result.returncode, 0, result.stdout)
        self.assertEqual(signal_log.read_text().splitlines(), ["-TERM 101", "-KILL 101"])

    def test_listener_returns_actual_address_owned_by_new_pid(self):
        proc = self.make_proc()
        result = self.run_helper('instance_listen_host 12345 18089 "$PROC_ROOT"', PROC_ROOT=str(proc))
        self.assertEqual(result.returncode, 0, result.stdout)
        self.assertEqual(result.stdout.strip(), "192.168.1.10")

    def test_wildcard_listener_uses_loopback_for_healthcheck(self):
        proc = self.make_proc(address="00000000")
        result = self.run_helper('instance_listen_host 12345 18089 "$PROC_ROOT"', PROC_ROOT=str(proc))
        self.assertEqual(result.returncode, 0, result.stdout)
        self.assertEqual(result.stdout.strip(), "127.0.0.1")

    def test_listener_rejects_other_process_on_same_port(self):
        proc = self.make_proc(inode="999999")
        result = self.run_helper('instance_listen_host 12345 18089 "$PROC_ROOT"', PROC_ROOT=str(proc))
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertEqual(result.stdout, "")

    def test_listener_requires_socket_to_be_listening(self):
        proc = self.make_proc(state="01")
        result = self.run_helper('instance_listen_host 12345 18089 "$PROC_ROOT"', PROC_ROOT=str(proc))
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertEqual(result.stdout, "")

    def test_readiness_rejects_new_process_exit_and_prints_logs(self):
        (self.remote / "dbgold.out").write_text("startup diagnostic\n")
        result = self.run_helper('''
pid_alive() { return 1; }
pid_is_instance() { return 0; }
instance_listen_host() { echo 127.0.0.1; }
verify_ready "$TEST_DEPLOY_DIR" 12345 18089
''')
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertIn("12345", result.stdout)
        self.assertIn("startup diagnostic", result.stdout)

    def test_readiness_timeout_is_failure(self):
        stage = self.make_stage()
        shutil.copy2(stage / "dbgold", self.remote / "dbgold")
        (self.remote / "dbgold.out").write_text("readiness diagnostic\n")
        result = self.run_helper('''
pid_alive() { return 0; }
pid_is_instance() { return 0; }
instance_listen_host() { echo 127.0.0.1; }
sleep() { SECONDS=$((SECONDS + 31)); }
verify_ready "$TEST_DEPLOY_DIR" 12345 18089
''', TEST_HEALTH_EXIT="1")
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertIn("30", result.stdout)
        self.assertIn("readiness diagnostic", result.stdout)
        self.assertIn("healthcheck --url http://127.0.0.1:18089/api/health/ready --timeout 1s",
                      self.log.read_text())

    def test_readiness_accepts_same_healthy_new_instance(self):
        stage = self.make_stage()
        shutil.copy2(stage / "dbgold", self.remote / "dbgold")
        result = self.run_helper('''
pid_alive() { return 0; }
pid_is_instance() { return 0; }
instance_listen_host() { echo 127.0.0.1; }
verify_ready "$TEST_DEPLOY_DIR" 12345 18123
''')
        self.assertEqual(result.returncode, 0, result.stdout)
        self.assertIn("12345", result.stdout)
        self.assertIn("healthcheck --url http://127.0.0.1:18123/api/health/ready --timeout 1s",
                      self.log.read_text())

    def test_readiness_uses_process_listen_address_and_bypasses_proxy(self):
        stage = self.make_stage()
        shutil.copy2(stage / "dbgold", self.remote / "dbgold")
        (self.remote / "dbgold").write_text('''#!/bin/sh
printf '%s\\n' "$*" "$NO_PROXY" "$no_proxy" > "$HEALTHCHECK_ARGUMENTS"
''')
        arguments = self.base / "healthcheck-arguments"
        result = self.run_helper('''
pid_alive() { return 0; }
pid_is_instance() { return 0; }
instance_listen_host() { echo 192.168.1.10; }
verify_ready "$TEST_DEPLOY_DIR" 12345 18123
''', HEALTHCHECK_ARGUMENTS=str(arguments), NO_PROXY="stale.example", no_proxy="stale.example")
        self.assertEqual(result.returncode, 0, result.stdout)
        self.assertEqual(arguments.read_text().splitlines(), [
            "healthcheck --url http://192.168.1.10:18123/api/health/ready --timeout 1s", "*", "*"])

    def test_readiness_waits_for_nohup_to_exec_new_binary(self):
        stage = self.make_stage()
        shutil.copy2(stage / "dbgold", self.remote / "dbgold")
        result = self.run_helper('''
exec_finished=0
pid_alive() { return 0; }
pid_is_instance() { [[ "$exec_finished" == 1 ]]; }
instance_listen_host() { echo 127.0.0.1; }
sleep() { exec_finished=1; }
verify_ready "$TEST_DEPLOY_DIR" 12345 18123
''')
        self.assertEqual(result.returncode, 0, result.stdout)
        self.assertIn("12345", result.stdout)
        self.assertEqual(self.log.read_text().count("healthcheck --url"), 1)

    def test_readiness_requires_same_new_instance_after_healthcheck(self):
        stage = self.make_stage()
        shutil.copy2(stage / "dbgold", self.remote / "dbgold")
        (self.remote / "dbgold.out").write_text("exited during healthcheck\n")
        result = self.run_helper('''
alive_calls=0
pid_alive() { alive_calls=$((alive_calls + 1)); [[ "$alive_calls" == 1 ]]; }
pid_is_instance() { return 0; }
instance_listen_host() { echo 127.0.0.1; }
sleep() { :; }
verify_ready "$TEST_DEPLOY_DIR" 12345 18089
''')
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertNotIn("已就绪", result.stdout)

    def test_activate_overrides_stale_paths_and_preserves_runtime_data(self):
        self.assert_activation_preserves_data(with_license=True)

    def test_activate_without_license_keeps_existing_remote_license(self):
        self.assert_activation_preserves_data(with_license=False)

    def assert_activation_preserves_data(self, with_license):
        stage = self.make_stage()
        if not with_license:
            (stage / "lib/OSCAR-JDBC-LICENSE").unlink()
            self.write_checksums(stage)
        for relative in [".env", "dbgold.db", "uploads/user-file", "log/existing.log", "lib/other-driver.jar",
                         "lib/OSCAR-JDBC-LICENSE"]:
            path = self.remote / relative
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text("preserve " + relative + "\n")
        (self.remote / ".env").write_text("OSCAR_BRIDGE_JAR=/old/bridge.jar\nSTATIC_DIR=/old/dist\n")
        environment = self.base / "new-process-env"
        actions = self.base / "activation-actions"
        java = self.make_java("custom java/bin/java")
        result = self.run_helper('''
preflight() { SELECTED_JAVA="$SELECTED_JAVA_TEST"; echo preflight >> "$ACTIONS"; }
stop_instance() { echo stop >> "$ACTIONS"; }
verify_ready() { wait "$2"; echo ready >> "$ACTIONS"; }
activate "$STAGE" "" 18123
''', STAGE=str(stage), ACTIONS=str(actions), SELECTED_JAVA_TEST=str(java),
                                 DEPLOY_TEST_ENV=str(environment), OSCAR_JAVA_BIN="/old/java",
                                 OSCAR_BRIDGE_JAR="/old/bridge.jar", OSCAR_JDBC_JAR="/old/jdbc.jar",
                                 STATIC_DIR="/old/dist")
        self.assertEqual(result.returncode, 0, result.stdout)
        self.assertEqual(actions.read_text().splitlines(), ["preflight", "stop", "ready"])
        self.assertEqual(environment.read_text().splitlines(), [
            "18123", str(self.remote / "dist"), str(java),
            str(self.remote / "lib/dbgold-oscar-bridge.jar"), str(self.remote / "lib/oscarJDBC8.jar")])
        self.assertTrue((self.remote / "dbgold.pid").read_text().strip().isdigit())
        for relative in ["dbgold.db", "uploads/user-file", "log/existing.log", "lib/other-driver.jar"]:
            self.assertEqual((self.remote / relative).read_text(), "preserve " + relative + "\n")
        self.assertIn("/old/bridge.jar", (self.remote / ".env").read_text())
        expected_license = ("test artifact " if with_license else "preserve ") + "lib/OSCAR-JDBC-LICENSE\n"
        self.assertEqual((self.remote / "lib/OSCAR-JDBC-LICENSE").read_text(), expected_license)


if __name__ == "__main__":
    unittest.main(verbosity=2)
