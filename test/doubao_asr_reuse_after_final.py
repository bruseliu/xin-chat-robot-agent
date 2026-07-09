#!/usr/bin/env python3
"""Smoke test: reuse one Doubao SAUC ASR WebSocket after a negative-sequence final.

It sends internal/vad/testdata/test2.wav twice on the same connection:
  full request seq=1
  first utterance:  200ms audio chunks, last chunk uses negative sequence
  second utterance: 200ms audio chunks, last chunk uses negative sequence

The script prints every response and any write/read failure. It intentionally
keeps the same WebSocket open between the two utterances.

Dependency:
  pip install websocket-client
"""

from __future__ import annotations

import argparse
import gzip
import json
import os
import ssl
import struct
import sys
import threading
import time
import uuid
import wave
from pathlib import Path
from typing import Any

try:
    import websocket
except ImportError as exc:
    raise SystemExit("missing dependency: pip install websocket-client") from exc


PROTOCOL_VERSION = 1
HEADER_SIZE_WORDS = 1

CLIENT_FULL_REQUEST = 1
CLIENT_AUDIO_ONLY_REQUEST = 2
SERVER_FULL_RESPONSE = 9
SERVER_ACK = 11
SERVER_ERROR = 15

NO_SEQUENCE = 0
POS_SEQUENCE = 1
NEG_SEQUENCE = 2
NEG_WITH_SEQUENCE = 3

NO_SERIALIZATION = 0
JSON_SERIALIZATION = 1

NO_COMPRESSION = 0
GZIP_COMPRESSION = 1

DEFAULT_WAV = Path("internal/vad/testdata/test2.wav")
DEFAULT_URL = "wss://openspeech.bytedance.com/api/v3/sauc/bigmodel_async"


def load_env(path: Path = Path(".env")) -> None:
    if not path.exists():
        return
    for raw in path.read_text().splitlines():
        line = raw.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        key, value = line.split("=", 1)
        key = key.strip()
        value = value.strip().strip('"').strip("'")
        os.environ.setdefault(key, value)


def header(message_type: int, flags: int, serialization: int, compression: int) -> bytes:
    return bytes([
        (PROTOCOL_VERSION << 4) | HEADER_SIZE_WORDS,
        (message_type << 4) | flags,
        (serialization << 4) | compression,
        0,
    ])


def gzip_bytes(data: bytes) -> bytes:
    return gzip.compress(data)


def build_full_request(seq: int, *, sample_rate: int, audio_format: str, model_name: str, language: str, enable_punc: bool) -> bytes:
    payload = {
        "user": {"uid": "reuse-after-final-smoke"},
        "audio": {
            "format": audio_format,
            "codec": "raw",
            "rate": sample_rate,
            "bits": 16,
            "channel": 1,
        },
        "request": {
            "model_name": model_name,
            "language": language,
            "enable_punc": enable_punc,
            "show_utterances": True,
            "enable_nonstream": False,
        },
    }
    raw = json.dumps(payload, ensure_ascii=False, separators=(",", ":")).encode("utf-8")
    compressed = gzip_bytes(raw)
    return b"".join([
        header(CLIENT_FULL_REQUEST, POS_SEQUENCE, JSON_SERIALIZATION, GZIP_COMPRESSION),
        struct.pack(">i", seq),
        struct.pack(">i", len(compressed)),
        compressed,
    ])


def build_audio_request(seq: int, pcm: bytes) -> bytes:
    flags = NEG_WITH_SEQUENCE if seq < 0 else POS_SEQUENCE
    compressed = gzip_bytes(pcm)
    return b"".join([
        header(CLIENT_AUDIO_ONLY_REQUEST, flags, NO_SERIALIZATION, GZIP_COMPRESSION),
        struct.pack(">i", seq),
        struct.pack(">i", len(compressed)),
        compressed,
    ])


def read_wav_pcm(path: Path) -> tuple[int, bytes]:
    with wave.open(str(path), "rb") as wf:
        channels = wf.getnchannels()
        sample_width = wf.getsampwidth()
        sample_rate = wf.getframerate()
        frames = wf.readframes(wf.getnframes())
    if sample_width != 2:
        raise SystemExit(f"{path} must be PCM16 WAV; sample_width={sample_width}")
    if channels == 1:
        return sample_rate, frames
    if channels == 2:
        # Downmix stereo PCM16LE to mono by taking left channel. This is only a smoke test.
        mono = bytearray()
        for i in range(0, len(frames), 4):
            mono.extend(frames[i : i + 2])
        return sample_rate, bytes(mono)
    raise SystemExit(f"{path} must be mono/stereo WAV; channels={channels}")


def chunk_pcm(pcm: bytes, sample_rate: int, chunk_ms: int) -> list[bytes]:
    if len(pcm) < 2:
        raise SystemExit("test wav has no audio")
    bytes_per_ms = sample_rate * 2 / 1000
    chunk_size = int(bytes_per_ms * chunk_ms)
    chunk_size -= chunk_size % 2
    if chunk_size <= 0:
        raise SystemExit(f"invalid chunk_ms={chunk_ms}")
    return [pcm[i : i + chunk_size] for i in range(0, len(pcm), chunk_size)]


def send_audio_chunks(ws: websocket.WebSocket, label: str, chunks: list[bytes], start_seq: int, chunk_ms: int) -> tuple[int, bool]:
    if not chunks:
        raise SystemExit("no audio chunks to send")
    seq = start_seq
    for idx, chunk in enumerate(chunks):
        is_last = idx == len(chunks) - 1
        wire_seq = -seq if is_last else seq
        chunk_label = f"{label} chunk={idx + 1}/{len(chunks)} seq={wire_seq}"
        if not send_packet(ws, chunk_label, build_audio_request(wire_seq, chunk)):
            return seq + 1, False
        seq += 1
        if not is_last:
            time.sleep(chunk_ms / 1000)
    return seq, True


def parse_packet(packet: bytes) -> dict[str, Any]:
    if len(packet) < 4:
        return {"error": f"short packet len={len(packet)}", "raw_hex": packet.hex()}
    header_size = (packet[0] & 0x0F) * 4
    message_type = packet[1] >> 4
    flags = packet[1] & 0x0F
    serialization = packet[2] >> 4
    compression = packet[2] & 0x0F
    offset = header_size
    seq = None
    if flags & 0x01:
        if len(packet) < offset + 4:
            return {"error": "missing sequence", "raw_hex": packet.hex()}
        seq = struct.unpack(">i", packet[offset : offset + 4])[0]
        offset += 4

    out: dict[str, Any] = {
        "message_type": message_type,
        "flags": flags,
        "sequence": seq,
        "is_last_package": bool((seq is not None and seq < 0) or (flags & 0x02)),
        "serialization": serialization,
        "compression": compression,
    }

    if message_type == SERVER_ERROR:
        if len(packet) >= offset + 8:
            code = struct.unpack(">i", packet[offset : offset + 4])[0]
            offset += 4
            size = struct.unpack(">i", packet[offset : offset + 4])[0]
            offset += 4
            payload = packet[offset : offset + size]
            if compression == GZIP_COMPRESSION:
                payload = gzip.decompress(payload)
            out["error_code"] = code
            out["error_payload"] = payload.decode("utf-8", errors="replace")
        return out

    if len(packet) < offset + 4:
        return out
    size = struct.unpack(">i", packet[offset : offset + 4])[0]
    offset += 4
    payload = packet[offset : offset + size]
    if compression == GZIP_COMPRESSION and payload:
        payload = gzip.decompress(payload)
    if serialization == JSON_SERIALIZATION and payload:
        try:
            out["payload"] = json.loads(payload.decode("utf-8"))
        except Exception as exc:  # noqa: BLE001 - smoke test prints parse failures.
            out["payload_decode_error"] = str(exc)
            out["payload_text"] = payload.decode("utf-8", errors="replace")
    elif payload:
        out["payload_hex"] = payload[:128].hex()
    return out


def recv_loop(ws: websocket.WebSocket, stop: threading.Event, packets: list[dict[str, Any]]) -> None:
    ws.settimeout(0.5)
    while not stop.is_set():
        try:
            data = ws.recv()
        except websocket.WebSocketTimeoutException:
            continue
        except Exception as exc:  # noqa: BLE001 - smoke test prints raw behavior.
            if not stop.is_set():
                print(f"[recv] error: {type(exc).__name__}: {exc}")
            return
        if isinstance(data, str):
            print(f"[recv] text: {data}")
            continue
        parsed = parse_packet(data)
        packets.append(parsed)
        print(f"[recv] packet: {json.dumps(parsed, ensure_ascii=False)}")


def wait_new_last_package(packets: list[dict[str, Any]], start_count: int, timeout: float) -> bool:
    deadline = time.time() + timeout
    while time.time() < deadline:
        if sum(1 for item in packets if item.get("is_last_package")) > start_count:
            return True
        time.sleep(0.05)
    print(f"[wait] no new last_package within {timeout}s")
    return False


def send_packet(ws: websocket.WebSocket, label: str, packet: bytes) -> bool:
    try:
        ws.send_binary(packet)
        print(f"[{label}] sent {len(packet)} bytes")
        return True
    except Exception as exc:  # noqa: BLE001 - smoke test prints raw behavior.
        print(f"[{label}] send error: {type(exc).__name__}: {exc}")
        return False


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--wav", default=str(DEFAULT_WAV), help="WAV path; default internal/vad/testdata/test2.wav")
    parser.add_argument("--recv-timeout", type=float, default=8.0)
    parser.add_argument("--chunk-ms", type=int, default=200, help="audio packet duration in ms; default 200")
    parser.add_argument("--pause-after-first-final", type=float, default=1.0)
    parser.add_argument("--insecure", action="store_true", help="disable TLS certificate verification")
    args = parser.parse_args()

    load_env()
    url = os.getenv("DOUBAO_ASR_URL", DEFAULT_URL)
    app_key = os.getenv("DOUBAO_ASR_APP_KEY", "")
    access_key = os.getenv("DOUBAO_ASR_ACCESS_KEY", "")
    resource_id = os.getenv("DOUBAO_ASR_RESOURCE_ID", "volc.bigasr.sauc.duration")
    model_name = os.getenv("DOUBAO_ASR_MODEL_NAME", "bigmodel")
    audio_format = os.getenv("DOUBAO_ASR_FORMAT", "pcm")
    language = os.getenv("DOUBAO_ASR_LANGUAGE", "zh-CN")
    enable_punc = os.getenv("DOUBAO_ASR_ENABLE_PUNC", "true").lower() != "false"

    if not app_key or not access_key:
        raise SystemExit("DOUBAO_ASR_APP_KEY and DOUBAO_ASR_ACCESS_KEY are required; source .env or fill .env")

    wav_path = Path(args.wav)
    sample_rate, pcm = read_wav_pcm(wav_path)
    chunks = chunk_pcm(pcm, sample_rate, args.chunk_ms)
    print(f"wav={wav_path} sample_rate={sample_rate} audio_format={audio_format} pcm_bytes={len(pcm)} chunk_ms={args.chunk_ms} chunks={len(chunks)} last_chunk_bytes={len(chunks[-1])}")

    headers = [
        f"X-Api-App-Key: {app_key}",
        f"X-Api-Access-Key: {access_key}",
        f"X-Api-Resource-Id: {resource_id}",
        f"X-Api-Request-Id: {uuid.uuid4()}",
        "X-Api-Sequence: -1",
    ]
    sslopt = {"cert_reqs": ssl.CERT_NONE} if args.insecure else None
    ws = websocket.create_connection(url, header=headers, timeout=10, sslopt=sslopt)
    print(f"connected url={url}")
    stop_recv = threading.Event()
    packets: list[dict[str, Any]] = []
    receiver = threading.Thread(target=recv_loop, args=(ws, stop_recv, packets), daemon=True)
    receiver.start()
    try:
        if not send_packet(ws, "full seq=1", build_full_request(1, sample_rate=sample_rate, audio_format=audio_format, model_name=model_name, language=language, enable_punc=enable_punc)):
            return 1
        time.sleep(0.2)

        last_count = sum(1 for item in packets if item.get("is_last_package"))
        next_seq, sent_first = send_audio_chunks(ws, "first", chunks, 2, args.chunk_ms)
        if not sent_first:
            return 1
        first_final_seen = wait_new_last_package(packets, last_count, args.recv_timeout)
        print(f"first_final_seen={first_final_seen}; next_seq={next_seq}; keep websocket open, sleep {args.pause_after_first_final}s")
        time.sleep(args.pause_after_first_final)

        last_count = sum(1 for item in packets if item.get("is_last_package"))
        _, sent_second = send_audio_chunks(ws, "second", chunks, next_seq, args.chunk_ms)
        if not sent_second:
            return 2
        second_final_seen = wait_new_last_package(packets, last_count, args.recv_timeout)
        print(f"second_final_seen={second_final_seen}")
        return 0 if second_final_seen else 3
    finally:
        stop_recv.set()
        try:
            ws.close()
        except Exception:
            pass
        receiver.join(timeout=1)


if __name__ == "__main__":
    raise SystemExit(main())
