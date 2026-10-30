import importlib.util
import json
import os
from pathlib import Path
import tempfile
import types
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("updater", Path(__file__).with_name("docker_updater.py"))
updater = importlib.util.module_from_spec(spec)
spec.loader.exec_module(updater)


class FakeWorker(updater.Worker):
    """Exercise the transaction with real backup/restore files, fake Docker only."""
    def __init__(self, root):
        self.root = Path(root)
        project = self.root / "project"
        project.mkdir()
        (project / ".env").write_text("POSTGRES_PASSWORD=secret\nARTEX_IMAGE=ghcr.io/jishengkele/naxia\nARTEX_TAG=v1.0.2\n")
        self.data = self.root / "data"
        self.data.mkdir()
        (self.data / "payload").write_bytes(b"old\x00\xff")
        self.key = self.root / "jwt.key"
        self.key.write_bytes(b"signing-key")
        self.events = []
        self.current = "old"
        self.running = True
        self.fail_new = False
        self.fail_restore = False
        self.fail_backup = False
        self.database = b"original-database"
        self.postgres_id = "unchanged-postgres"
        super().__init__(project, self.root / "state", "ghcr.io/jishengkele/naxia", health_seconds=0)

    def container(self):
        pin = "ghcr.io/jishengkele/naxia:v1.0.2" if self.current == "old" else "ghcr.io/jishengkele/naxia:v1.0.3@sha256:new"
        return {"Id": self.current, "Image": self.current, "Config": {"Image": pin}, "RestartCount": 0,
                "State": {"Running": self.running}, "Mounts": [
                    {"RW": True, "Type": "bind", "Source": str(self.data), "Destination": "/app/data"},
                    {"RW": False, "Type": "bind", "Source": str(self.key), "Destination": "/app/jwt.key"}]}

    def probe(self, c):
        return {"version": "1.0.2" if c["Image"] == "old" else "1.0.3", "initialized": True}

    def auth_headers(self, key):
        return {}

    def config(self):
        pg = {"POSTGRES_USER": "u", "POSTGRES_PASSWORD": "secret", "POSTGRES_DB": "naxia"}
        pin = "ghcr.io/jishengkele/naxia:v1.0.3@sha256:new" if "ARTEX_TAG=v1.0.3@" in (self.project / ".env").read_text() else "ghcr.io/jishengkele/naxia:v1.0.2"
        return {"services": {"artex": {"image": pin, "environment": {"ARTEX_PG_DSN": "postgres://u:secret@postgres/naxia"}},
                             "postgres": {"image": "postgres:16-alpine", "environment": pg}}}

    def compose(self, *args, **kwargs):
        self.events.append(args)
        if args[0] == "config": return json.dumps(self.config()).encode()
        if args[0] == "stop": self.running = False
        if args[0] == "up":
            self.current = "new" if "v1.0.3@" in (self.project / ".env").read_text() else "old"
            self.running = True
        return b""

    def run(self, *args, **kwargs):
        self.events.append(args)
        if args[:3] == ("docker", "image", "inspect"):
            old = args[-1] == "old"
            return json.dumps([{"Id": "old" if old else "new", "RepoDigests": [self.image + "@sha256:new"],
                "RepoTags": [self.image + ":v1.0.2"], "Config": {"Labels": {"org.opencontainers.image.version": "v1.0.2" if old else "v1.0.3"}}}]).encode()
        return b""

    def check_space(self, mounts):
        if getattr(self, "no_space", False): raise RuntimeError("space")

    def wait_healthy(self, tag, before, mounts, image_id):
        if tag == "v1.0.3":
            (self.data / "payload").write_bytes(b"new runtime changed data")
            (self.data / "new-file").write_text("migration")
            self.database = b"new-migrated-database"
            if self.fail_new: raise RuntimeError("unhealthy new image")
        return self.container()

    def backup_mounts(self, mounts, root):
        if self.fail_backup: raise RuntimeError("backup failed")
        super().backup_mounts(mounts, root)

    def restore_database(self, dump):
        if self.fail_restore: raise RuntimeError("restore failed")
        self.database = dump.read_bytes()


class TransactionTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.w = FakeWorker(self.temp.name)
        self.addCleanup(self.w.process_lock.close)
        self.w.status.update(running=True, version="v1.0.3")
        self.env = (self.w.project / ".env").read_bytes()

    def subprocess(self, args, **kwargs):
        if "pg_dump" in " ".join(args):
            kwargs["stdout"].write(self.w.database)
        return types.SimpleNamespace(returncode=0, stdout=b"", stderr=b"")

    def upgrade(self):
        with patch.object(updater.subprocess, "run", side_effect=self.subprocess), patch.object(updater.os, "sync"):
            self.w.upgrade("v1.0.3")

    def test_success_pins_image_and_cleans_only_own_backup(self):
        unrelated = self.w.state / "backup-previous-failure"
        unrelated.mkdir()
        self.upgrade()
        self.assertEqual(self.w.snapshot()["phase"], "complete")
        self.assertEqual(self.w.current, "new")
        self.assertIn("ARTEX_TAG=v1.0.3@sha256:new", (self.w.project / ".env").read_text())
        self.assertEqual(list(self.w.state.glob("backup-*")), [unrelated])
        self.assertTrue(any(x[:3] == ("docker", "image", "rm") for x in self.w.events))
        self.assertFalse(any("prune" in x for x in self.w.events))
        self.assertFalse(any(x[0] in ("stop", "rm", "up") and "postgres" in x for x in self.w.events))

    def test_failure_restores_database_files_and_env_retains_backup(self):
        self.w.fail_new = True
        self.upgrade()
        self.assertEqual(self.w.snapshot()["phase"], "failed")
        self.assertEqual(self.w.current, "old")
        self.assertEqual(self.w.database, b"original-database")
        self.assertEqual((self.w.data / "payload").read_bytes(), b"old\x00\xff")
        self.assertFalse((self.w.data / "new-file").exists())
        self.assertEqual((self.w.project / ".env").read_bytes(), self.env)
        self.assertTrue(list(self.w.state.glob("backup-*/complete.json")))
        self.assertFalse(any(x[:3] == ("docker", "image", "rm") for x in self.w.events))

    def test_failed_restore_disables_retries(self):
        self.w.fail_new = self.w.fail_restore = True
        self.upgrade()
        self.assertFalse(self.w.snapshot()["ready"])
        self.assertTrue(self.w.snapshot()["recovery_required"])
        with self.assertRaises(ValueError): self.w.submit("v1.0.4")

    def test_corrupt_backup_is_rejected_before_existing_files_are_deleted(self):
        self.w.fail_new = True
        wait = self.w.wait_healthy
        def corrupt(tag, *args):
            if tag == "v1.0.3":
                backup = next(self.w.state.glob("backup-*/0/payload"))
                backup.write_bytes(b"corrupt backup")
            return wait(tag, *args)
        self.w.wait_healthy = corrupt
        self.upgrade()
        self.assertFalse(self.w.snapshot()["ready"])
        self.assertEqual((self.w.data / "payload").read_bytes(), b"new runtime changed data")
        self.assertTrue(self.w.snapshot()["recovery_required"])

    def test_space_failure_does_not_stop_application(self):
        self.w.no_space = True
        self.upgrade()
        self.assertTrue(self.w.running)
        self.assertFalse(any(x[0] == "stop" for x in self.w.events))
        self.assertEqual((self.w.project / ".env").read_bytes(), self.env)

    def test_backup_failure_restarts_old_without_restoring_partial_data(self):
        self.w.fail_backup = True
        self.upgrade()
        self.assertEqual(self.w.current, "old")
        self.assertTrue(self.w.running)
        self.assertEqual(self.w.database, b"original-database")
        self.assertFalse(list(self.w.state.glob("backup-*/complete.json")))

    def test_cleanup_failure_does_not_rollback_accepted_database(self):
        original = updater.shutil.rmtree
        def fail_backup(path, *args, **kwargs):
            if Path(path).name.startswith("backup-"): raise OSError("busy")
            return original(path, *args, **kwargs)
        with patch.object(updater.shutil, "rmtree", side_effect=fail_backup): self.upgrade()
        self.assertEqual(self.w.snapshot()["phase"], "complete")
        self.assertEqual(self.w.current, "new")
        self.assertEqual(self.w.database, b"new-migrated-database")
        self.assertIn("清理未完成", self.w.snapshot()["message"])

    def test_restarted_worker_does_not_silently_retry_interrupted_upgrade(self):
        self.w.emit("staged", "rebuilding")
        self.w.process_lock.close()
        replacement = updater.Worker(self.w.project, self.w.state, self.w.image)
        self.addCleanup(replacement.process_lock.close)
        self.assertFalse(replacement.snapshot()["ready"])
        self.assertEqual(replacement.snapshot()["phase"], "failed")

    def test_payload_validation_and_version_injection(self):
        for bad in ["latest", "v1.0.3;id", "v1.0.3\n", "../../root", "v01.0.0", None]:
            with self.subTest(bad=bad), self.assertRaises(ValueError): updater.version(bad)
        with self.assertRaises(ValueError): self.w.submit("v1.0.4")

    def test_env_pin_preserves_other_settings_and_removes_duplicates(self):
        raw = "# hello\nPOSTGRES_PASSWORD='p@ss'\nARTEX_TAG=latest\nexport ARTEX_TAG=v1.0.0\nARTEX_IMAGE=old/repo\nCOMPOSE_FILE=a.yml:b.yml\n"
        out = self.w.pin_env(raw, self.w.image, "v1.0.3", "sha256:new")
        self.assertIn("POSTGRES_PASSWORD='p@ss'", out)
        self.assertIn("COMPOSE_FILE=a.yml:b.yml", out)
        self.assertEqual(out.count("ARTEX_TAG="), 1)

    def test_external_database_is_rejected(self):
        cfg = self.w.config()
        cfg["services"]["artex"]["environment"]["ARTEX_PG_DSN"] = "postgres://u:secret@elsewhere/naxia"
        with self.assertRaises(RuntimeError): self.w.validate_database(cfg)

    def test_backup_preserves_symlinks_and_file_permissions(self):
        os.symlink("payload", self.w.data / "link")
        os.chmod(self.w.data / "payload", 0o640)
        backup = self.w.state / "manual"
        backup.mkdir()
        mounts = self.w.mounts(self.w.container())
        self.w.backup_mounts(mounts, backup)
        self.assertEqual(os.readlink(backup / "0" / "link"), "payload")
        self.assertEqual(self.w.inventory(self.w.data), self.w.inventory(backup / "0"))

    def test_space_includes_database_and_per_file_overhead(self):
        self.w.db = lambda *args, **kw: b"1073741824"
        with patch.object(updater.shutil, "disk_usage", return_value=types.SimpleNamespace(free=1024**3)):
            with self.assertRaises(RuntimeError): updater.Worker.check_space(self.w, self.w.mounts(self.w.container()))


if __name__ == "__main__":
    unittest.main()
