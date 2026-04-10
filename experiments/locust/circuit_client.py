"""
circuit_client.py — Python implementation of the Torr onion-routing client.

Mirrors the logic in client/client.go so Locust users can build and send
circuits without shelling out to the Go binary.

Cryptography matches the Go implementation exactly:
  - RSA-2048 OAEP-SHA256 for key exchange (/setup)
  - AES-256-GCM with a random 12-byte nonce prepended to the ciphertext

Public API
----------
  get_circuit(session, directory_url, hops)         -> dict
  setup_circuit(session, circuit, circuit_id, hops) -> (guard_key, relay_key, exit_key)
  build_onion(guard_key, relay_key, exit_key,
              exit_layer, hops)                      -> bytes
  send_onion(session, guard_url, circuit_id, payload) -> dict
  decrypt_response(guard_key, relay_key, exit_key,
                   payload, hops)                   -> dict
  execute_circuit(session, directory_url, circuit_id,
                  exit_layer, hops)                 -> (ExitResponse dict, setup_ms, request_ms)
"""

import base64
import json
import os
import time
import uuid

from cryptography.hazmat.primitives import hashes, serialization
from cryptography.hazmat.primitives.asymmetric import padding
from cryptography.hazmat.primitives.ciphers.aead import AESGCM

MAX_CIRCUIT_ATTEMPTS = 3
NONCE_SIZE = 12  # bytes — matches Go's gcmNonceSize


# ── AES-256-GCM helpers ───────────────────────────────────────────────────────

def aes_encrypt(key: bytes, plaintext: bytes) -> bytes:
    """Encrypt plaintext with AES-256-GCM. Returns nonce || ciphertext."""
    nonce = os.urandom(NONCE_SIZE)
    ct = AESGCM(key).encrypt(nonce, plaintext, None)
    return nonce + ct


def aes_decrypt(key: bytes, data: bytes) -> bytes:
    """Decrypt nonce || ciphertext produced by aes_encrypt."""
    if len(data) < NONCE_SIZE:
        raise ValueError("ciphertext too short")
    nonce, ct = data[:NONCE_SIZE], data[NONCE_SIZE:]
    return AESGCM(key).decrypt(nonce, ct, None)


# ── RSA OAEP helpers ──────────────────────────────────────────────────────────

def _load_public_key(pem: str):
    return serialization.load_pem_public_key(pem.encode())


def rsa_encrypt_key(public_key_pem: str, aes_key: bytes) -> bytes:
    """RSA-OAEP-SHA256 encrypt an AES key with the node's public key."""
    pub = _load_public_key(public_key_pem)
    return pub.encrypt(aes_key, padding.OAEP(
        mgf=padding.MGF1(algorithm=hashes.SHA256()),
        algorithm=hashes.SHA256(),
        label=None,
    ))


# ── Directory / circuit helpers ───────────────────────────────────────────────

def get_circuit(session, directory_url: str, hops: int) -> dict:
    """
    GET /circuit?hops=N from the directory server.
    Returns the raw parsed JSON dict {guard, relay, exit}.
    Raises on HTTP error or missing nodes.
    """
    resp = session.get(f"{directory_url}/circuit", params={"hops": hops}, timeout=10)
    resp.raise_for_status()
    circuit = resp.json()

    if not circuit.get("guard", {}).get("nodeId"):
        raise ValueError("directory returned circuit with missing guard")
    if hops == 3 and (
        not circuit.get("relay", {}).get("nodeId")
        or not circuit.get("exit", {}).get("nodeId")
    ):
        raise ValueError("directory returned incomplete 3-hop circuit")

    return circuit


# ── Key exchange (/setup) ─────────────────────────────────────────────────────

def _node_url(node: dict) -> str:
    return f"http://{node['host']}:{node['port']}"


def _send_setup_key(session, node_url: str, circuit_id: str, public_key_pem: str, aes_key: bytes):
    """RSA-encrypt aes_key and POST it to node_url/setup."""
    encrypted = rsa_encrypt_key(public_key_pem, aes_key)
    body = {
        "circuitId": circuit_id,
        "encryptedKey": base64.b64encode(encrypted).decode(),
    }
    resp = session.post(f"{node_url}/setup", json=body, timeout=10)
    if resp.status_code != 204:
        raise RuntimeError(
            f"POST {node_url}/setup returned {resp.status_code}: {resp.text[:200]}"
        )


def setup_circuit(session, circuit: dict, circuit_id: str, hops: int):
    """
    Generate fresh AES-256 session keys, RSA-encrypt each one, and POST to
    each node's /setup endpoint.

    Returns (guard_key, relay_key, exit_key).
    relay_key and exit_key are None for 1-hop circuits.
    """
    guard_key = os.urandom(32)
    relay_key = os.urandom(32) if hops == 3 else None
    exit_key  = os.urandom(32) if hops == 3 else None

    candidates = [(circuit["guard"], guard_key)]
    if hops == 3:
        candidates += [
            (circuit["relay"], relay_key),
            (circuit["exit"],  exit_key),
        ]

    for node, key in candidates:
        _send_setup_key(session, _node_url(node), circuit_id, node["publicKey"], key)

    return guard_key, relay_key, exit_key


# ── Onion construction ────────────────────────────────────────────────────────

def build_onion(
    guard_key: bytes,
    relay_key,
    exit_key,
    exit_layer: dict,
    circuit: dict,
    hops: int,
) -> bytes:
    """
    Wrap exit_layer in onion-encrypted layers.

    Layer structure (inside-out, matching Go's BuildOnionWithHops):
      1-hop : AES(guardKey, ExitLayer JSON)
      3-hop : AES(guardKey, {nextHop: relayAddr,
                   payload: AES(relayKey, {nextHop: exitAddr,
                                payload: AES(exitKey, ExitLayer JSON)})})
    """
    exit_json = json.dumps(exit_layer, separators=(",", ":")).encode()

    if hops == 1:
        return aes_encrypt(guard_key, exit_json)

    # Innermost: exit layer encrypted with exitKey
    exit_ct = aes_encrypt(exit_key, exit_json)

    relay_node = circuit["relay"]
    exit_node  = circuit["exit"]
    exit_addr  = f"{exit_node['host']}:{exit_node['port']}"
    relay_addr = f"{relay_node['host']}:{relay_node['port']}"

    # Middle: relay layer encrypted with relayKey
    relay_layer = json.dumps(
        {"nextHop": exit_addr, "payload": base64.b64encode(exit_ct).decode()},
        separators=(",", ":"),
    ).encode()
    relay_ct = aes_encrypt(relay_key, relay_layer)

    # Outer: guard layer encrypted with guardKey
    guard_layer = json.dumps(
        {"nextHop": relay_addr, "payload": base64.b64encode(relay_ct).decode()},
        separators=(",", ":"),
    ).encode()
    return aes_encrypt(guard_key, guard_layer)


# ── Send via guard ────────────────────────────────────────────────────────────

def send_onion(session, guard_url: str, circuit_id: str, payload: bytes) -> dict:
    """POST the onion payload to the guard's /onion endpoint."""
    body = {
        "circuitId": circuit_id,
        "payload": base64.b64encode(payload).decode(),
    }
    resp = session.post(f"{guard_url}/onion", json=body, timeout=30)
    if resp.status_code != 200:
        raise RuntimeError(
            f"POST {guard_url}/onion returned {resp.status_code}: {resp.text[:200]}"
        )
    return resp.json()


# ── Response decryption ───────────────────────────────────────────────────────

def decrypt_response(
    guard_key: bytes,
    relay_key,
    exit_key,
    payload_b64: str,
    hops: int,
) -> dict:
    """
    Peel onion layers from the circuit response.

    Return path (each node re-encrypts with its own key):
      guard wraps relay wraps exit wraps ExitResponse JSON

    Client peels: guardKey → relayKey → exitKey → ExitResponse dict.
    """
    data = base64.b64decode(payload_b64)

    if hops == 1:
        plain = aes_decrypt(guard_key, data)
        return json.loads(plain)

    relay_encrypted = aes_decrypt(guard_key, data)
    exit_encrypted  = aes_decrypt(relay_key, relay_encrypted)
    exit_json       = aes_decrypt(exit_key, exit_encrypted)
    return json.loads(exit_json)


# ── High-level: one full circuit attempt ──────────────────────────────────────

def _try_circuit(session, directory_url: str, circuit_id: str, exit_layer: dict, hops: int):
    circuit = get_circuit(session, directory_url, hops)

    t0 = time.monotonic()
    guard_key, relay_key, exit_key = setup_circuit(session, circuit, circuit_id, hops)
    setup_ms = (time.monotonic() - t0) * 1000

    payload = build_onion(guard_key, relay_key, exit_key, exit_layer, circuit, hops)
    guard_url = _node_url(circuit["guard"])

    t1 = time.monotonic()
    onion_resp = send_onion(session, guard_url, circuit_id, payload)
    request_ms = (time.monotonic() - t1) * 1000

    exit_resp = decrypt_response(
        guard_key, relay_key, exit_key, onion_resp["payload"], hops
    )
    return exit_resp, setup_ms, request_ms


def execute_circuit(session, directory_url: str, circuit_id: str, exit_layer: dict, hops: int):
    """
    Run the full circuit flow with up to MAX_CIRCUIT_ATTEMPTS retries on
    setup/send failures (mirrors ExecuteRequestWithHops in client/client.go).

    Returns (exit_response dict, setup_ms, request_ms).
    Raises on all attempts exhausted or non-retriable error.
    """
    last_err = None
    for attempt in range(1, MAX_CIRCUIT_ATTEMPTS + 1):
        try:
            return _try_circuit(session, directory_url, circuit_id, exit_layer, hops)
        except Exception as exc:
            last_err = exc
            if attempt < MAX_CIRCUIT_ATTEMPTS:
                continue
    raise RuntimeError(
        f"all {MAX_CIRCUIT_ATTEMPTS} circuit attempts failed: {last_err}"
    ) from last_err


def new_circuit_id() -> str:
    return str(uuid.uuid4())
