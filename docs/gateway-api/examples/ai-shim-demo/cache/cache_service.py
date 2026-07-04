#!/usr/bin/env python3
"""Semantic cache sidecar for the AI shim (tech-preview).

POST /lookup {model, tenant, system_hash, prompt} -> 200 {response} | 404 miss
POST /store  {model, tenant, system_hash, prompt, response} -> {"ok": true}

Two-stage match, both strictly scoped to a (model, tenant, system_hash)
namespace -- a lookup can NEVER return another tenant's response:
  1. exact  : sha256(prompt) direct key hit (fast, deterministic)
  2. semantic: all-MiniLM-L6-v2 embedding -> Redis RediSearch HNSW cosine KNN,
               accepted only when cosine similarity >= CACHE_THRESHOLD (0.95)

Fail-open: if Redis or the model is unavailable, /lookup returns 404 (miss) so
the shim just proceeds to the model server.
"""
import hashlib
import os

import numpy as np
import redis
from fastapi import FastAPI, Response
from pydantic import BaseModel
from redis.commands.search.field import TagField, TextField, VectorField
from redis.commands.search.indexDefinition import IndexDefinition, IndexType
from redis.commands.search.query import Query
from sentence_transformers import SentenceTransformer

REDIS_URL = os.environ.get("REDIS_URL", "redis://localhost:6379")
THRESHOLD = float(os.environ.get("CACHE_THRESHOLD", "0.95"))
INDEX = "shimcache"
PREFIX = "cache:"
DIM = 384

app = FastAPI()
_model = SentenceTransformer("all-MiniLM-L6-v2")
_r = redis.Redis.from_url(REDIS_URL, decode_responses=False)


def _ensure_index():
    try:
        _r.ft(INDEX).info()
    except redis.exceptions.ResponseError:
        _r.ft(INDEX).create_index(
            (
                TagField("ns"),
                TextField("phash"),
                VectorField("embedding", "HNSW",
                            {"TYPE": "FLOAT32", "DIM": DIM,
                             "DISTANCE_METRIC": "COSINE"}),
            ),
            definition=IndexDefinition(prefix=[PREFIX], index_type=IndexType.HASH),
        )


@app.on_event("startup")
def _startup():
    try:
        _ensure_index()
    except Exception as e:   # noqa: BLE001 - never let index setup crash boot
        print(f"[cache] index init deferred: {e}", flush=True)


def _sha(s: str) -> str:
    return hashlib.sha256((s or "").encode("utf-8")).hexdigest()


def _ns_hash(model: str, tenant: str, system_hash: str) -> str:
    # Namespace = model + tenant + system_hash. Hashed so it is a safe RediSearch
    # TAG (no separator/escaping issues) AND so tenants can never collide.
    return _sha(f"{model}\x1f{tenant}\x1f{system_hash}")


def _embed(text: str) -> bytes:
    v = _model.encode(text, normalize_embeddings=True)
    return np.asarray(v, dtype=np.float32).tobytes()


class LookupReq(BaseModel):
    model: str
    tenant: str
    system_hash: str
    prompt: str


class StoreReq(LookupReq):
    response: str


@app.post("/lookup")
def lookup(req: LookupReq, response: Response):
    ns = _ns_hash(req.model, req.tenant, req.system_hash)
    phash = _sha(req.prompt)
    key = f"{PREFIX}{ns}:{phash}"
    try:
        # 1) exact match (already tenant-scoped by the key)
        cached = _r.hget(key, "response")
        if cached is not None:
            return {"response": cached.decode("utf-8"), "match": "exact"}

        # 2) semantic KNN within this namespace only
        _ensure_index()
        q = (Query(f"(@ns:{{{ns}}})=>[KNN 1 @embedding $vec AS score]")
             .sort_by("score").return_fields("response", "score")
             .dialect(2).paging(0, 1))
        res = _r.ft(INDEX).search(q, query_params={"vec": _embed(req.prompt)})
        if res.docs:
            doc = res.docs[0]
            sim = 1.0 - float(doc.score)          # cosine distance -> similarity
            if sim >= THRESHOLD:
                body = doc.response
                if isinstance(body, bytes):
                    body = body.decode("utf-8")
                return {"response": body, "match": "semantic",
                        "similarity": round(sim, 4)}
    except Exception as e:   # noqa: BLE001 - fail open to a miss
        print(f"[cache] lookup error (fail-open miss): {e}", flush=True)

    response.status_code = 404
    return {"match": "miss"}


@app.post("/store")
def store(req: StoreReq):
    ns = _ns_hash(req.model, req.tenant, req.system_hash)
    phash = _sha(req.prompt)
    key = f"{PREFIX}{ns}:{phash}"
    try:
        _ensure_index()
        _r.hset(key, mapping={
            "ns": ns,
            "phash": phash,
            "response": req.response.encode("utf-8"),
            "embedding": _embed(req.prompt),
        })
        return {"ok": True}
    except Exception as e:   # noqa: BLE001
        print(f"[cache] store error: {e}", flush=True)
        return {"ok": False, "error": str(e)}


@app.get("/healthz")
def healthz():
    try:
        _r.ping()
        return {"ok": True, "threshold": THRESHOLD}
    except Exception:   # noqa: BLE001
        return Response('{"ok":false}', status_code=503,
                        media_type="application/json")
