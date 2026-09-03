#!/usr/bin/env python3
"""Language-neutral wrapper fixture consumer used by the Go interoperability test."""

import base64
import json
import sys


MAX_ENVELOPE_BYTES = 1 << 20


def main() -> None:
    document = json.load(sys.stdin)
    job = document["job"]
    lease = document["lease"]
    envelope = base64.b64decode(job["encrypted_input"], validate=True)
    if not envelope or len(envelope) > MAX_ENVELOPE_BYTES:
        raise ValueError("invalid encrypted envelope")
    if job["job_id"] != lease["job_id"]:
        raise ValueError("job and lease identity differ")
    completion = {
        "schema_version": 1,
        "job_id": job["job_id"],
        "lease_id": lease["lease_id"],
        "worker_id": lease["worker_id"],
        "account_id": lease.get("account_id", ""),
        "fence_epoch": lease.get("fence_epoch", 0),
        "encrypted_output": base64.b64encode(envelope).decode("ascii"),
    }
    json.dump(completion, sys.stdout, separators=(",", ":"))
    sys.stdout.write("\n")


if __name__ == "__main__":
    main()
