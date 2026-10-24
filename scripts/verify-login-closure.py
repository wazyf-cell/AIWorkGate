#!/usr/bin/env python3
"""End-to-end acceptance for the login closure (handoff section 4.3).

For every installed plugin it drives the same host API the UI uses:
  login start per declared method -> actionable entry? -> poll
  /api/accounts -> account present?
  /api/accounts/identity/details -> real identity?
  /v1/chat/completions -> real upstream content?

Exit code 1 when a declared login method yields no actionable entry, or when a
plugin that has an account cannot answer a short chat. Print with --md FILE to
also write a markdown report.

Scaffolding only: it never edits plugin state beyond opening/cancelling a login
session and (optionally) creating one throwaway client key.
"""
import argparse
import json
import os
import subprocess
import time
import urllib.error
import urllib.request

BASE = os.environ.get("AWG_BASE", "http://127.0.0.1:7865")
REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
CAP_METHOD = {
    "login_api_key": "api_key",
    "login_oauth": "oauth",
    "login_ide_plugin": "ide_plugin",
    "login_paste_code": "paste_code",
    "login_local_state": "local_state",
    "login_cookie": "cookie",
}


def session_cookie():
    out = subprocess.run(
        ["go", "run", "./tools/devsession", "cookie"],
        cwd=REPO, capture_output=True, text=True, timeout=180,
    )
    if out.returncode != 0:
        raise SystemExit("devsession failed: " + (out.stderr or out.stdout))
    return out.stdout.strip()


def call(cookie, method, path, body=None, key=None, timeout=90):
    data = None if body is None else json.dumps(body).encode("utf-8")
    req = urllib.request.Request(BASE + path, data=data, method=method)
    if cookie:
        req.add_header("Cookie", cookie)
    if key:
        req.add_header("Authorization", "Bearer " + key)
    if data is not None:
        req.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            raw = resp.read().decode("utf-8", "replace")
            return resp.status, (json.loads(raw) if raw.strip() else {})
    except urllib.error.HTTPError as exc:
        raw = exc.read().decode("utf-8", "replace")
        try:
            parsed = json.loads(raw)
        except ValueError:
            parsed = {"raw": raw[:300]}
        return exc.code, parsed
    except Exception as exc:  # noqa: BLE001 - report, do not mask as success
        return 0, {"error": str(exc)}


def client_key(cookie):
    existing = os.path.join(REPO, ".devtmp", "keysecret.txt")
    if os.path.exists(existing):
        key = open(existing, encoding="utf-8").read().strip()
        status, _ = call(cookie, "GET", "/v1/models", key=key)
        if status == 200:
            return key
    status, body = call(cookie, "POST", "/api/keys",
                        {"name": "acceptance-%d" % int(time.time())})
    if status not in (200, 201) or not body.get("secret"):
        raise SystemExit("cannot create a client key: %s %s" % (status, body))
    os.makedirs(os.path.join(REPO, ".devtmp"), exist_ok=True)
    with open(existing, "w", encoding="utf-8") as fh:
        fh.write(body["secret"])
    return body["secret"]


def methods_of(caps):
    return [CAP_METHOD[c] for c in caps if c in CAP_METHOD]


def actionable(body):
    if body.get("status") == "success":
        return "success"
    if body.get("url"):
        return "url"
    if body.get("fields"):
        return "fields"
    if body.get("code"):
        return "code"
    return ""


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--md")
    ap.add_argument("--only", default="",
                    help="comma-separated plugin ids to drive (default: every installed plugin)")
    ap.add_argument("--skip-chat", action="store_true")
    ap.add_argument("--submit-probe", action="store_true",
                    help="submit an obviously bogus code and require a non-success answer")
    ap.add_argument("--reset-probe", action="store_true",
                    help="rotate identity where it exists and require the identifiers to change")
    args = ap.parse_args()

    cookie = session_cookie()
    _, plugins = call(cookie, "GET", "/api/plugins")
    _, states = call(cookie, "GET", "/api/accounts/login-state")
    declared = {r["plugin_id"]: r.get("methods") or [] for r in states.get("plugins", [])}
    key = None if args.skip_chat else client_key(cookie)
    _, models = call(cookie, "GET", "/v1/models", key=key) if key else (0, {})
    by_plugin = {}
    for item in models.get("data", []):
        pid, _, rest = item["id"].partition("/")
        by_plugin.setdefault(pid, []).append(
            {"id": rest or item["id"], "free": bool(item.get("free"))})

    rows, failures = [], []
    only = {x.strip() for x in args.only.split(",") if x.strip()}
    for plugin in plugins:
        pid = plugin["manifest"]["id"]
        if only and pid not in only:
            continue
        caps = plugin["manifest"].get("capabilities") or []
        entries = []
        for method in methods_of(caps):
            status, body = call(cookie, "POST", "/api/accounts/login",
                                {"plugin_id": pid, "method": method}, timeout=90)
            kind = actionable(body) if status == 200 else ""
            where = body.get("url") or ""
            if body.get("fields"):
                where = ",".join(f.get("name", "?") for f in body["fields"])
            sid = body.get("session_id")
            if (args.submit_probe and status == 200 and sid and body.get("fields")
                    and method in ("paste_code", "api_key")):
                field = body["fields"][0].get("name") or "code"
                sstatus, sbody = call(
                    cookie, "POST", "/api/accounts/login/%s/submit" % sid,
                    {"plugin_id": pid, "values": {field: "agw-invalid-probe"}}, timeout=90,
                )
                sres = sbody.get("status") or ""
                rows.append((pid, "submit", field, sstatus, sres or "NONE",
                             str(sbody.get("error") or "")[:48]))
                if sstatus == 200 and sres == "success":
                    failures.append("%s/%s accepted a bogus %s as success" % (pid, method, field))
            if status == 200 and sid and kind != "success":
                call(cookie, "DELETE", "/api/accounts/login/%s?plugin=%s" % (sid, pid))
                # A dismissed dialog must never keep reporting pending (the UI
                # would keep polling) or success (it would claim a login the
                # operator abandoned). Plugins that drop the session answer 404.
                pstatus, pbody = call(
                    cookie, "GET", "/api/accounts/login/%s?plugin=%s" % (sid, pid))
                pafter = (pbody.get("status") or "") if isinstance(pbody, dict) else ""
                rows.append((pid, "cancel", method, pstatus, pafter or "GONE", ""))
                if pstatus == 200 and pafter in ("pending", "success"):
                    failures.append("%s/%s reports %s after cancel" % (pid, method, pafter))
            entries.append("%s=%d/%s" % (method, status, kind or "NONE"))
            if status != 200 or not kind:
                failures.append("%s/%s: %s %s" % (pid, method, status, body))
            rows.append((pid, "start", method, status, kind, where[:60]))

        _, accounts = call(cookie, "GET", "/api/accounts")
        mine = [a for a in accounts.get("accounts", []) if a.get("plugin_id") == pid]
        for acc in mine:
            detStatus, det = call(cookie, "POST", "/api/accounts/identity/details",
                                  {"plugin_id": pid, "account_id": acc["id"], "reveal": False})
            ident = det.get("identity") or {}
            has = bool(ident.get("machine_id") or ident.get("device_id") or ident.get("session_id"))
            rows.append((pid, "identity", acc["id"], detStatus, "yes" if has else "none", ""))
            if args.reset_probe and has:
                before = json.dumps(ident, sort_keys=True)
                rstatus, _ = call(cookie, "POST", "/api/accounts/identity/reset",
                                  {"plugin_id": pid, "account_id": acc["id"],
                                   "mode": "request", "confirm": True})
                _, det2 = call(cookie, "POST", "/api/accounts/identity/details",
                               {"plugin_id": pid, "account_id": acc["id"], "reveal": False})
                after = json.dumps(det2.get("identity") or {}, sort_keys=True)
                changed = before != after
                rows.append((pid, "identity-reset", acc["id"], rstatus,
                             "changed" if changed else "unchanged", ""))
                if rstatus != 200 or not changed:
                    failures.append("%s identity reset: %s changed=%s" % (pid, rstatus, changed))

        if mine and key and not args.skip_chat:
            cands = by_plugin.get(pid) or []
            # Free tiers meter per model: flash variants can be exhausted while
            # another free model still answers (opencode: big-pickle 200 while
            # every *-flash-free returned 429). Try free models first, then the
            # rest, and stop at the first non-empty answer.
            ordered = sorted(cands, key=lambda m: (0 if m["free"] else 1, m["id"]))
            if not ordered:
                rows.append((pid, "chat", "-", "-", "SKIP", "no models advertised"))
            else:
                attempt_note, ok = [], False
                model, status, text = None, None, ""
                for candidate in ordered[:4]:
                    model = candidate["id"]
                    status, body = call(
                        cookie, "POST", "/v1/chat/completions",
                        {"model": pid + "/" + model, "stream": False, "max_tokens": 512,
                         "messages": [{"role": "user", "content": "Reply with exactly: OK"}]},
                        key=key, timeout=90,
                    )
                    text = ""
                    if status == 200:
                        text = ((body.get("choices") or [{}])[0].get("message") or {}).get("content") or ""
                    ok = status == 200 and bool(text.strip())
                    attempt_note.append("%s:%s" % (model, status if ok else "EMPTY" if status == 200 else status))
                    if ok:
                        break
                rows.append((pid, "chat", model, status, "OK" if ok else "EMPTY",
                             text[:40] if ok else "tried " + ", ".join(attempt_note)))
                if not ok:
                    failures.append("%s chat: tried %s" % (pid, ", ".join(attempt_note)))
        print("%-10s accounts=%d %s" % (pid, len(mine), " ".join(entries) or "(no login_*)"))

    lines = ["| plugin | step | detail | HTTP | result | note |", "|---|---|---|---|---|---|"]
    lines += ["| %s | %s | %s | %s | %s | %s |" % r for r in rows]
    lines.append("")
    lines.append("FAILURES: %d" % len(failures))
    for item in failures:
        lines.append("- " + item)
    report = "\n".join(lines)
    print(report)
    if args.md:
        with open(args.md, "w", encoding="utf-8") as fh:
            fh.write("# 验收运行 %s\n\n%s\n" % (time.strftime("%Y-%m-%d %H:%M:%S"), report))
    raise SystemExit(1 if failures else 0)


if __name__ == "__main__":
    main()
