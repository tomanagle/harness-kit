#!/usr/bin/env python3
"""Restore only the disposable Compose fixture to a fresh temporary database."""
import json
import os
from pathlib import Path
import subprocess
import tempfile
import uuid

ROOT = Path(__file__).resolve().parent
COMPOSE = ["docker", "compose", "-f", str(ROOT / "compose.yml")]
if os.environ.get("COMPOSE_PROJECT_NAME"):
    COMPOSE += ["-p", os.environ["COMPOSE_PROJECT_NAME"]]
PG = COMPOSE + ["exec", "-T", "postgres"]
TARGET = "hk_restore_" + uuid.uuid4().hex


def execute(args, **options):
    return subprocess.run(args, check=True, **options)


def sql(database, query):
    return execute(PG + ["psql", "-U", "harness_owner", "-d", database,
                         "-XAt", "-v", "ON_ERROR_STOP=1", "-c", query],
                   capture_output=True, text=True).stdout.strip()


def fingerprint(database):
    tables = sql(database, "SELECT tablename FROM pg_tables WHERE schemaname='public' ORDER BY tablename").splitlines()
    result = {}
    for name in tables:
        # Identifiers came from the fixture catalog, and are quoted independently.
        table = '"' + name.replace('"', '""') + '"'
        result[name] = sql(database, f"SELECT count(*)::text || ':' || coalesce(md5(string_agg(row_to_json(t)::text, E'\\n' ORDER BY row_to_json(t)::text)), '') FROM {table} t")
    return result


with tempfile.TemporaryDirectory(prefix="harness-kit-restore-") as temporary:
    dump = Path(temporary) / "fixture.dump"
    # Quiesce fixture workloads before invoking; fingerprint and dump use separate snapshots.
    before = fingerprint("harnesskit")
    with dump.open("wb") as output:
        execute(PG + ["pg_dump", "-U", "harness_owner", "-Fc", "harnesskit"], stdout=output)
    execute(PG + ["createdb", "-U", "harness_owner", "-T", "template0", TARGET])
    try:
        with dump.open("rb") as source:
            execute(PG + ["pg_restore", "-U", "harness_owner", "--no-owner", "--exit-on-error", "-d", TARGET], stdin=source)
        restored = fingerprint(TARGET)
        if before != restored:
            raise RuntimeError("Restored data differs; stop fixture writes and repeat")
        print(json.dumps({"restore": "passed", "tables": len(restored), "source_untouched": True}))
    finally:
        execute(PG + ["dropdb", "-U", "harness_owner", "--if-exists", TARGET])
