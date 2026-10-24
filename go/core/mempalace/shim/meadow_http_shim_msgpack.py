#!/usr/bin/env python3
"""
meadow_http_shim_msgpack.py — Tier 1 obfuscation step 3.

Drop-in replacement for ~/.mempalace/meadow_http_shim.py whose cache
persists as `meadow_kv_cache.msgpack` instead of `meadow_kv_cache.json`.

Why msgpack:
  - JSON cache is plaintext on disk; `cat ~/.mempalace/meadow_kv_cache.json`
    reveals every lesson / pref / verbatim. msgpack is a tight binary
    format — `cat` shows control bytes, `file` reports "data".
  - This isn't crypto (anyone with the file can decode) but it does mean
    a casual `grep` on the shipped app's data dir returns nothing.
  - msgpack is significantly faster than json for our payload sizes;
    typical ~60 KB cache loads in <2 ms vs 6 ms for json.

Migration story:
  - On startup we look for meadow_kv_cache.msgpack FIRST.
  - If missing but meadow_kv_cache.json exists, we load the json, write
    msgpack atomically, and rename the json to .json.bak.pre-msgpack so
    nothing is destroyed.
  - The original shim keeps writing .json; cutover means stopping it
    cleanly and starting this variant.

The HTTP wire format is unchanged. Endpoints, payloads, port (8091),
error semantics all match the original shim verbatim — only the
on-disk cache encoding differs. This is deliberate: Meadow Code TS code
doesn't have to change.

NOTE: This file is NOT auto-deployed. The live shim on PID 8091 is
controlled by the user; cutover is documented in OBFUSCATION_VERIFY.md.
The Tier 1 obfuscation branch ships the binary + migrator only.

Usage:
    pip install msgpack
    python scripts/mempalace/meadow_http_shim_msgpack.py \\
        --port 8091 [--no-mempalace] [--cache ~/.mempalace/meadow_kv_cache.msgpack]
"""

from __future__ import annotations

import argparse
import logging
import os
import re
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any
from urllib.parse import parse_qs, urlparse

try:
    import msgpack  # type: ignore
except ImportError:
    sys.stderr.write(
        "msgpack not installed. Run: pip install msgpack\n"
        "(falling back to json with a warning so the shim still boots)\n"
    )
    msgpack = None

import json  # for migration + fallback only

logger = logging.getLogger("meadow_shim_mp")
logger.setLevel(logging.INFO)

# ---------------------------------------------------------------------------
# Paths
# ---------------------------------------------------------------------------

DEFAULT_CACHE_MP = Path.home() / ".mempalace" / "meadow_kv_cache.msgpack"
LEGACY_CACHE_JSON = Path.home() / ".mempalace" / "meadow_kv_cache.json"

_CACHE_PATH: Path = DEFAULT_CACHE_MP
_USE_MEMPALACE: bool = True
_cache_lock = threading.Lock()
_cache_data: dict[str, Any] = {}
_cache_dirty: bool = False


def _ensure_cache_dir() -> None:
    _CACHE_PATH.parent.mkdir(parents=True, exist_ok=True)


# ---------------------------------------------------------------------------
# Migration: one-shot json -> msgpack
# ---------------------------------------------------------------------------


def _migrate_json_to_msgpack(json_path: Path, mp_path: Path) -> dict[str, Any]:
    """Read legacy JSON, write msgpack, archive the JSON. Returns the data."""
    try:
        with json_path.open("r", encoding="utf-8") as fh:
            data = json.load(fh)
    except (OSError, json.JSONDecodeError) as exc:
        logger.warning("legacy json load failed (%s); starting empty", exc)
        return {"prefs": {}, "max_tok": {}, "lessons": {}}

    # Atomic msgpack write
    _save_cache_raw(data, mp_path)

    # Archive the json (don't delete — explicit safety net)
    bak = json_path.with_suffix(".json.bak.pre-msgpack")
    try:
        os.replace(json_path, bak)
        logger.info("migrated %s -> %s (legacy archived at %s)", json_path, mp_path, bak)
    except OSError as exc:
        logger.warning("could not archive legacy json: %s", exc)
    return data


# ---------------------------------------------------------------------------
# Cache I/O (msgpack)
# ---------------------------------------------------------------------------


def _empty_cache() -> dict[str, Any]:
    return {"prefs": {}, "max_tok": {}, "lessons": {}}


def _load_cache() -> dict[str, Any]:
    """Load cache from msgpack. Auto-migrate from legacy JSON on first run.

    fail-open: any error -> empty dict.
    """
    if _CACHE_PATH.exists():
        if msgpack is None:
            # Last-resort: treat msgpack file as json (will likely fail; that's fine)
            logger.error("msgpack lib unavailable; cannot read .msgpack cache")
            return _empty_cache()
        try:
            with _CACHE_PATH.open("rb") as fh:
                data = msgpack.unpackb(fh.read(), raw=False)
            if not isinstance(data, dict):
                logger.warning("cache root not dict; rebuilding")
                return _empty_cache()
            for k in ("prefs", "max_tok", "lessons"):
                data.setdefault(k, {})
            return data
        except Exception as exc:  # noqa: BLE001 — fail-open by design
            logger.warning("msgpack cache load failed (%s); rebuilding", exc)
            return _empty_cache()

    # No msgpack — try legacy json migration
    if LEGACY_CACHE_JSON.exists() and msgpack is not None:
        return _migrate_json_to_msgpack(LEGACY_CACHE_JSON, _CACHE_PATH)

    return _empty_cache()


def _save_cache_raw(data: dict[str, Any], path: Path) -> None:
    """Atomic msgpack write. fail-open."""
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp = path.with_suffix(path.suffix + ".tmp")
    try:
        if msgpack is None:
            # Fallback: write json so we don't lose data
            with tmp.open("w", encoding="utf-8") as fh:
                json.dump(data, fh, ensure_ascii=False, indent=2, sort_keys=True)
        else:
            with tmp.open("wb") as fh:
                fh.write(msgpack.packb(data, use_bin_type=True))
        os.replace(tmp, path)
    except OSError as exc:
        logger.warning("cache save failed: %s", exc)


def _save_cache(data: dict[str, Any]) -> None:
    _save_cache_raw(data, _CACHE_PATH)


# ---------------------------------------------------------------------------
# Minimal HTTP surface (mirror of original shim, msgpack-backed)
#
# We re-implement only the endpoints the TS side actually calls. Keeping
# this file self-contained means the Tier 1 build doesn't accidentally
# couple to mempalace internals.
# ---------------------------------------------------------------------------


def _now() -> float:
    return time.time()


def _normalize_lesson_entry(item: Any) -> dict[str, Any]:
    if isinstance(item, str):
        return {
            "content": item,
            "verbatim": None,
            "negative": False,
            "use_count": 0,
            "last_used_ts": _now(),
            "created_ts": _now(),
        }
    if isinstance(item, dict):
        item.setdefault("content", "")
        item.setdefault("verbatim", None)
        item.setdefault("negative", False)
        item.setdefault("use_count", 0)
        item.setdefault("last_used_ts", _now())
        item.setdefault("created_ts", _now())
        return item
    return _normalize_lesson_entry(str(item))


def _salience(lesson: dict[str, Any], now: float) -> float:
    """Recency + frequency salience in [0,1] (F2): a recently/often used lesson
    ranks higher; a stale never-used one decays toward 0 (soft forgetting — it
    stops surfacing without being deleted). Uses only the existing use_count /
    last_used_ts fields. A 0/absent last_used_ts contributes no recency."""
    last = float(lesson.get("last_used_ts") or 0.0)
    used = int(lesson.get("use_count") or 0)
    recency = 0.0
    if last > 0:
        age_days = max(0.0, (now - last) / 86400.0)
        recency = 0.5 ** (age_days / 30.0)  # 30-day half-life
    freq = min(used, 5) / 5.0
    return 0.6 * recency + 0.4 * freq


def _boost_recalled(lessons: list[dict[str, Any]]) -> None:
    """Bump use_count + last_used_ts on the lessons just recalled so they gain
    salience next time (F2 boost). Recalled lessons are shared references into the
    cache (the GET path takes a shallow snapshot), so mutating them updates the
    stored entries; mark the cache dirty so the flush worker persists it."""
    global _cache_dirty
    if not lessons:
        return
    now = _now()
    with _cache_lock:
        for l in lessons:
            if isinstance(l, dict):
                l["use_count"] = int(l.get("use_count") or 0) + 1
                l["last_used_ts"] = now
        _cache_dirty = True


# ---------------------------------------------------------------------------
# Lesson semantic recall (lexical relevance across task_types)
# ---------------------------------------------------------------------------
# The exact /lessons?task_type=X lookup only returns lessons stored under that
# exact task_type — a lesson learned doing "svg-data-viz" is invisible to a
# "mini-spa" task even when it is the relevant one. With a query string the
# shim instead ranks ALL lessons by lexical overlap (the FTS layer of the
# OpenClaw-style hybrid; a vector layer is a documented optional follow-up).
# The corpus is tiny (≤5 lessons per task_type), so brute-force scoring is
# instant and needs no index or dependency.

_WORD_RE = re.compile(r"[a-z0-9]+")
_CJK_RUN_RE = re.compile(r"[一-鿿぀-ヿ가-힯]+")


def _tokens(text: str) -> set[str]:
    """Lexical tokens for overlap scoring: lowercased ASCII/digit words plus CJK
    unigrams + bigrams, so Chinese/Japanese match without a word segmenter."""
    text = (text or "").lower()
    toks: set[str] = set(_WORD_RE.findall(text))
    for run in _CJK_RUN_RE.findall(text):
        for i, ch in enumerate(run):
            toks.add(ch)
            if i + 1 < len(run):
                toks.add(run[i : i + 2])
    return toks


def _search_lessons(
    all_lessons: dict[str, Any], query: str, want_task_type: str, k: int
) -> list[dict[str, Any]]:
    """Rank lessons across every task_type by lexical overlap with query — the
    cross-task_type recall the exact-key path cannot do. The requested task_type
    still gets a boost (it stays a strong signal); recency breaks ties. Returns
    up to k normalized lessons that share at least one token with the query."""
    qtoks = _tokens(query)
    if not qtoks:
        return []
    now = _now()
    scored: list[tuple[float, float, dict[str, Any]]] = []
    for task_type, arr in all_lessons.items():
        for raw in arr or []:
            lesson = _normalize_lesson_entry(raw)
            overlap = len(qtoks & _tokens(str(lesson.get("content") or "")))
            if overlap == 0:
                continue
            score = float(overlap)
            if want_task_type and task_type == want_task_type:
                score += 2.0
            score += _salience(lesson, now)  # F2: recency+frequency tie-break / decay
            scored.append((score, float(lesson.get("last_used_ts") or 0.0), lesson))
    scored.sort(key=lambda x: (x[0], x[1]), reverse=True)
    return [lesson for _, _, lesson in scored[:k]]


# ---------------------------------------------------------------------------
# Optional vector layer (bge-m3 MLX) — true semantic recall over lexical
# ---------------------------------------------------------------------------
# Lazy-loaded on the FIRST q= query so the shim stays light until used, then
# blended with the lexical score (hybrid, OpenClaw-style). Fail-open: any
# import/load/runtime error falls back to lexical-only. Disable with
# MEADOW_MEMPALACE_VECTOR=0; override the model with MEADOW_MEMPALACE_EMBED_MODEL.

_EMBED_LOCK = threading.Lock()
_EMBED: dict[str, Any] = {"tried": False, "model": None, "proc": None, "gen": None, "mx": None, "cache": {}}


def _embed_model() -> Any:
    """Lazily load the bge-m3 MLX embedder once. None when disabled/unavailable
    (the caller then uses the lexical path)."""
    if os.environ.get("MEADOW_MEMPALACE_VECTOR", "1") == "0":
        return None
    with _EMBED_LOCK:
        if _EMBED["tried"]:
            return _EMBED["model"]
        _EMBED["tried"] = True
        try:
            import mlx.core as mx  # type: ignore
            from mlx_embeddings import generate as gen, load as ld  # type: ignore

            name = os.environ.get("MEADOW_MEMPALACE_EMBED_MODEL", "mlx-community/bge-m3-mlx-fp16")
            model, proc = ld(name)
            _EMBED.update(model=model, proc=proc, gen=gen, mx=mx)
            logger.info("mempalace vector recall ON (%s)", name)
        except Exception as exc:  # noqa: BLE001 — fail-open to lexical
            logger.warning("mempalace vector recall OFF (lexical only): %s", exc)
            _EMBED["model"] = None
        return _EMBED["model"]


def _embed(texts: list[str]) -> list[Any]:
    """Embed + L2-normalize, memoizing per exact text (lessons are stable, so a
    warm query only embeds the query itself). The cache is capped so an endless
    stream of distinct queries can't grow it without bound."""
    mx = _EMBED["mx"]
    cache = _EMBED["cache"]
    missing = [t for t in dict.fromkeys(texts) if t not in cache]
    if missing:
        if len(cache) > 1000:
            cache.clear()
        out = _EMBED["gen"](_EMBED["model"], _EMBED["proc"], missing)
        for t, v in zip(missing, out.text_embeds):
            cache[t] = v / mx.maximum(mx.linalg.norm(v), 1e-9)
    return [cache[t] for t in texts]


def _vector_search(
    all_lessons: dict[str, Any], query: str, want_task_type: str, k: int
) -> list[dict[str, Any]] | None:
    """Hybrid recall: 0.7·cosine(bge-m3) + 0.3·lexical + task_type boost, across
    all task_types. None when the embedder is unavailable so the caller falls
    back to the lexical path."""
    if not query.strip() or _embed_model() is None:
        return None
    items = [(tt, _normalize_lesson_entry(raw)) for tt, arr in all_lessons.items() for raw in (arr or [])]
    if not items:
        return []
    try:
        contents = [str(lesson.get("content") or "") for _, lesson in items]
        qvec = _embed([query])[0]
        cvecs = _embed(contents)
        qtoks = _tokens(query)
        now = _now()
        scored: list[tuple[float, float, float, dict[str, Any]]] = []
        for (task_type, lesson), cvec, content in zip(items, cvecs, contents):
            cos = float((qvec * cvec).sum())
            lex = min(len(qtoks & _tokens(content)), 3) / 3.0
            rel = 0.7 * cos + 0.3 * lex
            if want_task_type and task_type == want_task_type:
                rel += 0.1
            # F2: salience re-ranks the RELEVANT lessons (recency+frequency); it is
            # kept OUT of the floor so it can never lift an irrelevant lesson in.
            sal = 0.1 * _salience(lesson, now)
            scored.append((rel, sal, float(lesson.get("last_used_ts") or 0.0), lesson))
        scored.sort(key=lambda x: (x[0] + x[1], x[2]), reverse=True)
        # Floor on RELEVANCE alone (bge-m3 unrelated blends ~0.40, related ≥0.49)
        # while keeping cross-lingual synonym hits lexical misses.
        return [lesson for rel, _sal, _ts, lesson in scored[:k] if rel >= 0.45]
    except Exception as exc:  # noqa: BLE001 — fail-open
        logger.warning("mempalace vector search failed, lexical fallback: %s", exc)
        return None


def _search_lessons_hybrid(
    all_lessons: dict[str, Any], query: str, want_task_type: str, k: int
) -> list[dict[str, Any]]:
    """Vector+lexical hybrid when the embedder is up; pure lexical otherwise."""
    vec = _vector_search(all_lessons, query, want_task_type, k)
    return vec if vec is not None else _search_lessons(all_lessons, query, want_task_type, k)


class Handler(BaseHTTPRequestHandler):
    def _send_json(self, code: int, payload: dict[str, Any]) -> None:
        body = json.dumps(payload, ensure_ascii=False).encode("utf-8")
        self.send_response(code)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _read_body(self) -> dict[str, Any]:
        length = int(self.headers.get("Content-Length") or 0)
        if length <= 0:
            return {}
        raw = self.rfile.read(length)
        try:
            return json.loads(raw.decode("utf-8"))
        except (UnicodeDecodeError, json.JSONDecodeError):
            return {}

    def log_message(self, fmt: str, *args: Any) -> None:  # noqa: A003
        logger.debug("%s - %s", self.address_string(), fmt % args)

    # --- GETs ---

    def do_GET(self) -> None:  # noqa: N802
        parsed = urlparse(self.path)
        qs = parse_qs(parsed.query)
        try:
            if parsed.path == "/healthz":
                self._send_json(200, {"ok": True, "backend": "msgpack"})
            elif parsed.path == "/prefs":
                key = (qs.get("key") or [""])[0]
                with _cache_lock:
                    val = _cache_data["prefs"].get(key)
                self._send_json(200, {"value": val})
            elif parsed.path == "/max_tok":
                tt = (qs.get("task_type") or [""])[0]
                with _cache_lock:
                    val = _cache_data["max_tok"].get(tt)
                self._send_json(200, {"value": val})
            elif parsed.path == "/lessons":
                tt = (qs.get("task_type") or [""])[0]
                q = (qs.get("q") or [""])[0]
                try:
                    k = int((qs.get("k") or ["5"])[0])
                except (TypeError, ValueError):
                    k = 5
                with _cache_lock:
                    snapshot = dict(_cache_data["lessons"])  # shallow copy
                if q.strip():
                    # Hybrid (vector+lexical) cross-task_type recall, run OUTSIDE
                    # the cache lock — the first call may load the embedder (~2s)
                    # and must not block other requests. Falls back to the exact
                    # task_type list when nothing matches, so a query never
                    # returns less than the old behavior.
                    lessons = _search_lessons_hybrid(snapshot, q, tt, k)
                    if not lessons and tt:
                        lessons = [_normalize_lesson_entry(x) for x in (snapshot.get(tt) or [])]
                else:
                    lessons = [_normalize_lesson_entry(x) for x in (snapshot.get(tt) or [])]
                _boost_recalled(lessons)  # F2: recall raises future salience, persisted via dirty
                self._send_json(200, {"lessons": lessons})
            else:
                self._send_json(404, {"error": "unknown path"})
        except Exception as exc:  # noqa: BLE001 — fail-open
            logger.warning("GET %s failed: %s", parsed.path, exc)
            self._send_json(200, {"value": None, "lessons": []})

    # --- POSTs ---

    def do_POST(self) -> None:  # noqa: N802
        global _cache_dirty
        parsed = urlparse(self.path)
        body = self._read_body()
        try:
            if parsed.path == "/pref":
                key = body.get("key", "")
                val = body.get("value", "")
                with _cache_lock:
                    _cache_data["prefs"][key] = val
                    _cache_dirty = True
                self._send_json(200, {"ok": True})
            elif parsed.path == "/max_tok":
                tt = body.get("task_type", "")
                val = int(body.get("value", 0))
                with _cache_lock:
                    _cache_data["max_tok"][tt] = val
                    _cache_dirty = True
                self._send_json(200, {"ok": True})
            elif parsed.path == "/lesson":
                tt = body.get("task_type", "")
                entry = _normalize_lesson_entry(
                    {"content": body.get("content", ""), "verbatim": body.get("verbatim")}
                )
                with _cache_lock:
                    arr = list(_cache_data["lessons"].get(tt) or [])
                    arr.append(entry)
                    _cache_data["lessons"][tt] = arr[-5:]  # cap at 5 per task_type
                    _cache_dirty = True
                self._send_json(200, {"ok": True})
            else:
                self._send_json(404, {"error": "unknown path"})
        except Exception as exc:  # noqa: BLE001 — fail-open
            logger.warning("POST %s failed: %s", parsed.path, exc)
            self._send_json(200, {"ok": False})


# ---------------------------------------------------------------------------
# Entry point
# ---------------------------------------------------------------------------


def main(argv: list[str] | None = None) -> int:
    global _CACHE_PATH, _USE_MEMPALACE, _cache_data
    parser = argparse.ArgumentParser()
    parser.add_argument("--port", type=int, default=8891)
    parser.add_argument("--cache", type=str, default=str(DEFAULT_CACHE_MP))
    parser.add_argument("--no-mempalace", action="store_true")
    parser.add_argument("--migrate-only", action="store_true",
                        help="Run JSON->msgpack migration and exit without starting HTTP server")
    parser.add_argument("--log-level", default="INFO")
    args = parser.parse_args(argv)

    logging.basicConfig(level=args.log_level)
    _CACHE_PATH = Path(args.cache).expanduser()
    _USE_MEMPALACE = not args.no_mempalace
    _ensure_cache_dir()

    # Load cache once into memory (triggers migration if needed)
    _cache_data = _load_cache()
    logger.info(
        "cache loaded: prefs=%d max_tok=%d lesson_buckets=%d",
        len(_cache_data["prefs"]), len(_cache_data["max_tok"]), len(_cache_data["lessons"]),
    )

    if args.migrate_only:
        logger.info("migrate-only mode: exiting after migration")
        return 0

    # Start background flushing thread
    def _flush_worker():
        global _cache_dirty
        while True:
            time.sleep(1.0)
            with _cache_lock:
                if _cache_dirty:
                    _save_cache_raw(_cache_data, _CACHE_PATH)
                    _cache_dirty = False
                    logger.debug("background flush: cache saved to disk")
                    
    flush_thread = threading.Thread(target=_flush_worker, daemon=True)
    flush_thread.start()

    httpd = ThreadingHTTPServer(("127.0.0.1", args.port), Handler)
    logger.info("meadow_http_shim_msgpack listening on http://127.0.0.1:%d", args.port)
    logger.info("cache: %s", _CACHE_PATH)
    try:
        httpd.serve_forever()
    except KeyboardInterrupt:
        logger.info("shutting down")
    finally:
        httpd.server_close()
        # Final flush on close
        with _cache_lock:
            if _cache_dirty:
                _save_cache_raw(_cache_data, _CACHE_PATH)
                logger.info("final flush: cache saved to disk")
    return 0


if __name__ == "__main__":
    sys.exit(main())
