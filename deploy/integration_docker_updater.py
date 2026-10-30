"""Real Docker/Compose/PostgreSQL transaction acceptance. Run only in CI.

Uses an isolated loopback registry, disposable Compose project, real pg_dump /
pg_restore and bind mounts. The application fixture avoids a second full build;
normal Go/frontend CI validates the production application separately.
"""
import importlib.util
import json
from pathlib import Path
import subprocess
import tempfile
import time
import unittest

spec = importlib.util.spec_from_file_location("updater", Path(__file__).with_name("docker_updater.py"))
updater = importlib.util.module_from_spec(spec)
spec.loader.exec_module(updater)
IMAGE = "127.0.0.1:5000/naxia/fixture"


def run(*args, cwd=None):
    return subprocess.check_output(args, cwd=cwd)


class RealDockerTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.temp = tempfile.TemporaryDirectory()
        cls.build = Path(cls.temp.name)
        (cls.build / "app.py").write_text('''import os,json,signal,sys
from http.server import BaseHTTPRequestHandler,HTTPServer
signal.signal(signal.SIGTERM,lambda *_:sys.exit(0))
class H(BaseHTTPRequestHandler):
 def do_GET(self):
  values={'/api/health':json.dumps({'ok':True,'version':os.environ['FIXTURE_VERSION']}),'/api/auth/status':'{"initialized":true}','/api/update/settings':'{}','/':'<!doctype html><script src="/app.js"></script>','/app.js':'console.log("ready")'}
  data=values.get(self.path)
  self.send_response(200 if data else 404);self.end_headers();self.wfile.write((data or '').encode())
 def log_message(self,*a):pass
HTTPServer(('0.0.0.0',8787),H).serve_forever()
''')
        (cls.build / "artex").write_text('#!/bin/sh\n[ "${1:-}" = "-h" ] && exit 0\nexec python3 /app/app.py\n')
        (cls.build / "Dockerfile").write_text('FROM python:3.12-slim\nARG VERSION\nENV FIXTURE_VERSION=$VERSION\nLABEL org.opencontainers.image.version=v$VERSION\nWORKDIR /app\nCOPY app.py artex /app/\nRUN chmod +x /app/artex\nENTRYPOINT ["/app/artex"]\n')
        run("docker", "run", "-d", "--name", "naxia-ci-updater-registry", "-p", "127.0.0.1:5000:5000", "registry:2")
        for v in ("1.0.2", "1.0.3"):
            run("docker", "build", "--build-arg", "VERSION="+v, "-t", IMAGE+":v"+v, str(cls.build))
            run("docker", "push", IMAGE+":v"+v)

    @classmethod
    def tearDownClass(cls):
        run("docker", "rm", "-f", "naxia-ci-updater-registry")
        cls.temp.cleanup()

    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        self.project = self.root / "project"
        self.project.mkdir()
        self.data = self.root / "data"
        self.data.mkdir()
        (self.data / "payload").write_bytes(b"original\x00\xff")
        key = self.root / "jwt.key"
        key.write_text("A"*32)
        name = "naxia-ci-" + self.root.name.lower().replace("_", "")
        (self.project / ".env").write_text("ARTEX_IMAGE="+IMAGE+"\nARTEX_TAG=v1.0.2\n")
        cfg = {"name":name,"services":{
            "postgres":{"image":"postgres:16-alpine","environment":{"POSTGRES_USER":"naxia","POSTGRES_PASSWORD":"ci-only","POSTGRES_DB":"naxia"},"volumes":["db:/var/lib/postgresql/data"]},
            "artex":{"image":"${ARTEX_IMAGE}:${ARTEX_TAG}","environment":{"ARTEX_PG_DSN":"postgres://naxia:ci-only@postgres/naxia"},"volumes":[str(self.data)+":/app/data",str(key)+":/app/jwt.key:ro"]}},"volumes":{"db":{}}}
        (self.project / "compose.yml").write_text(json.dumps(cfg))
        run("docker", "compose", "up", "-d", cwd=self.project)
        self.addCleanup(self.cleanup_stack)
        for _ in range(60):
            # The image briefly runs a Unix-socket-only init server before the
            # configured database exists. Wait for an actual TCP SQL query.
            p = subprocess.run(["docker","compose","exec","-T","postgres","sh","-c",
                'PGPASSWORD="$POSTGRES_PASSWORD" psql -h 127.0.0.1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -Atc "SELECT 1"'],
                cwd=self.project,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
            if p.returncode == 0: break
            time.sleep(1)
        else:
            self.fail("PostgreSQL did not become ready")
        self.w = updater.Worker(self.project, self.root / "state", IMAGE, health_seconds=2)
        self.addCleanup(self.w.process_lock.close)
        self.w.db("psql","-v","ON_ERROR_STOP=1","-c","CREATE TABLE settings(key text PRIMARY KEY,value text); CREATE TABLE proof(value text); INSERT INTO proof VALUES('original');")
        # auth fixture does not require a login, but still exercises generated headers.
        time.sleep(2)

    def cleanup_stack(self):
        run("docker","compose","down","-v","--remove-orphans",cwd=self.project)
        self.temp.cleanup()

    def test_success_recreates_pins_and_cleans(self):
        old = self.w.container()
        pg = self.w.compose("ps","-q","postgres")
        self.w.upgrade("v1.0.3")
        self.assertEqual(self.w.snapshot()["phase"], "complete", self.w.snapshot())
        new = self.w.container()
        self.assertNotEqual(old["Id"],new["Id"])
        self.assertEqual(pg,self.w.compose("ps","-q","postgres"))
        self.assertIn("v1.0.3@sha256:",(self.project/".env").read_text())
        self.assertFalse(list(self.w.state.glob("backup-*")))
        self.assertEqual((self.data/"payload").read_bytes(),b"original\x00\xff")
        self.assertEqual(self.w.db("psql","-At","-c","SELECT value FROM proof;").strip(),b"original")

    def test_unhealthy_upgrade_restores_entire_database_and_files(self):
        wait = self.w.wait_healthy
        def inject(tag,*args):
            if tag == "v1.0.3":
                wait(tag,*args)
                self.w.db("psql","-v","ON_ERROR_STOP=1","-c","UPDATE proof SET value='changed'; CREATE TABLE added_by_migration(id int);")
                (self.data/"payload").write_bytes(b"changed")
                (self.data/"new-file").write_text("new")
                raise RuntimeError("injected post-migration failure")
            return wait(tag,*args)
        self.w.wait_healthy = inject
        self.w.upgrade("v1.0.3")
        self.assertEqual(self.w.snapshot()["phase"],"failed",self.w.snapshot())
        self.assertTrue(self.w.snapshot()["ready"],self.w.snapshot())
        self.assertEqual(self.w.probe(self.w.container())["version"],"1.0.2")
        self.assertEqual((self.data/"payload").read_bytes(),b"original\x00\xff")
        self.assertFalse((self.data/"new-file").exists())
        self.assertEqual(self.w.db("psql","-At","-c","SELECT value FROM proof;").strip(),b"original")
        self.assertEqual(self.w.db("psql","-At","-c","SELECT to_regclass('added_by_migration') IS NULL;").strip(),b"t")
        self.assertTrue(list(self.w.state.glob("backup-*/complete.json")))


if __name__ == "__main__":
    unittest.main(verbosity=2)
