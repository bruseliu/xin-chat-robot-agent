#!/usr/bin/env python3
"""Real-time voice client for xin-chat-robot-agent.

Reads microphone audio, sends to the server via WebSocket,
prints ASR/LLM intermediate results, and plays TTS audio.
"""

import asyncio
import base64
import json
import os
import signal
import sys
import struct
import threading
import time
from collections import deque

import pyaudio
import websockets

try:
    import cv2
except ImportError:
    cv2 = None

SERVER_URL = os.getenv("VOICE_SERVER_URL", "ws://127.0.0.1:8080/v1/chat/voice")
USER_ID = os.getenv("VOICE_USER_ID", "test-user")
SESSION_ID = os.getenv("VOICE_SESSION_ID", "test-session-1")
MIC_SAMPLE_RATE = 16000
MIC_CHANNELS = 1
MIC_FORMAT = pyaudio.paInt16
MIC_CHUNK_MS = 200
MIC_CHUNK_SAMPLES = MIC_SAMPLE_RATE * MIC_CHUNK_MS // 1000


def env_bool(name: str, default: bool = False) -> bool:
    value = os.getenv(name)
    if value is None:
        return default
    return value.strip().lower() in {"1", "true", "yes", "on"}


VIDEO_ENABLED = env_bool("VIDEO_ENABLED")
VIDEO_DEVICE_INDEX = int(os.getenv("VIDEO_DEVICE_INDEX", "0"))
VIDEO_SEND_INTERVAL_SECONDS = float(os.getenv("VIDEO_SEND_INTERVAL_SECONDS", "1"))
VIDEO_CAPTURE_INTERVAL_SECONDS = float(os.getenv("VIDEO_CAPTURE_INTERVAL_SECONDS", "0.2"))
VIDEO_MAX_WIDTH = int(os.getenv("VIDEO_MAX_WIDTH", "640"))
VIDEO_MAX_HEIGHT = int(os.getenv("VIDEO_MAX_HEIGHT", "480"))
VIDEO_JPEG_QUALITY = int(os.getenv("VIDEO_JPEG_QUALITY", "70"))

# Muting the mic while TTS plays makes barge-in impossible — the server never
# receives the utterance that would interrupt. Keep False for headphones (or any
# echo-free input) and set True for speakers, where the mic would otherwise feed
# the assistant's own voice back and let it interrupt itself. Pair with
# BARGE_IN_ENABLED on the server.
MIC_MUTE_DURING_TTS = False

pa = pyaudio.PyAudio()


class CameraCapture:
    """Keeps only the newest JPEG in memory; frames are never written to disk."""

    def __init__(self, device_index: int):
        if cv2 is None:
            raise RuntimeError("摄像头功能需要安装 opencv-python：.venv/bin/pip install -r requirements.txt")
        self._capture = cv2.VideoCapture(device_index)
        if not self._capture.isOpened():
            self._capture.release()
            raise RuntimeError(f"无法打开摄像头设备 {device_index}")
        self._capture.set(cv2.CAP_PROP_FRAME_WIDTH, VIDEO_MAX_WIDTH)
        self._capture.set(cv2.CAP_PROP_FRAME_HEIGHT, VIDEO_MAX_HEIGHT)
        self._lock = threading.Lock()
        self._latest = None
        self._version = 0
        self._reported_capture = False
        self._reported_read_failure = False
        self._running = threading.Event()
        self._thread = None

    def start(self):
        self._running.set()
        self._thread = threading.Thread(target=self._capture_loop, name="camera-capture", daemon=True)
        self._thread.start()

    def _capture_loop(self):
        try:
            while self._running.is_set():
                ok, frame = self._capture.read()
                if not ok:
                    if not self._reported_read_failure:
                        print(f"[camera] Cannot read a frame from device {VIDEO_DEVICE_INDEX}")
                        self._reported_read_failure = True
                    time.sleep(0.1)
                    continue
                height, width = frame.shape[:2]
                scale = min(1.0, VIDEO_MAX_WIDTH / width, VIDEO_MAX_HEIGHT / height)
                if scale < 1.0:
                    frame = cv2.resize(frame, (round(width * scale), round(height * scale)))
                ok, encoded = cv2.imencode(".jpg", frame, [cv2.IMWRITE_JPEG_QUALITY, VIDEO_JPEG_QUALITY])
                if not ok:
                    continue
                with self._lock:
                    self._latest = encoded.tobytes()
                    self._version += 1
                if not self._reported_capture:
                    print(f"[camera] Capturing {frame.shape[1]}x{frame.shape[0]} JPEG frames")
                    self._reported_capture = True
                time.sleep(max(VIDEO_CAPTURE_INTERVAL_SECONDS, 0.02))
        finally:
            self._capture.release()

    def latest(self):
        with self._lock:
            if self._latest is None:
                return None, self._version
            return self._latest, self._version

    def close(self):
        self._running.clear()
        if self._thread:
            self._thread.join(timeout=2)
            self._thread = None
        with self._lock:
            self._latest = None


class TTSPlayer:
    """Plays PCM audio on one long-lived output stream and playback thread.

    The stream is opened once and reused for the whole session. Opening and
    closing it per utterance wedges ALSA after a few dozen cycles, and because
    every method here would then have to join the playback thread, a wedged
    device blocked the WebSocket reader and the client stopped rendering
    anything at all while the server kept working. Nothing in this class blocks
    on the audio device, so the event loop can never stall on playback.
    """

    def __init__(self):
        self._stream = None
        self._sample_rate = None
        self._queue = deque()
        self._lock = threading.Lock()
        self._wake = threading.Event()
        self._thread = None
        self._running = True
        # Only frames belonging to a live utterance are queued, so an interrupted
        # turn cannot be heard even if a stray frame arrives late.
        self.accepting = False
        self.playing = False

    def _ensure_stream(self, sample_rate: int):
        if self._stream is not None and self._sample_rate == sample_rate:
            return
        if self._stream is not None:
            try:
                self._stream.stop_stream()
                self._stream.close()
            except Exception:
                pass
        self._sample_rate = sample_rate
        self._stream = pa.open(
            format=pyaudio.paInt16,
            channels=1,
            rate=sample_rate,
            output=True,
            frames_per_buffer=2048,
        )
        if self._thread is None:
            self._thread = threading.Thread(target=self._play_loop, daemon=True)
            self._thread.start()

    def _play_loop(self):
        while self._running:
            chunk = None
            with self._lock:
                if self._queue:
                    chunk = self._queue.popleft()
            if chunk is None:
                self._wake.wait(timeout=0.1)
                self._wake.clear()
                continue
            stream = self._stream
            if stream is None:
                continue
            try:
                stream.write(chunk)
            except Exception:
                # A device hiccup must not kill playback for the rest of the session.
                pass

    def start(self, sample_rate: int):
        """Begin a new utterance."""
        self._ensure_stream(sample_rate or 16000)
        with self._lock:
            self._queue.clear()
        self.accepting = True
        self.playing = True

    def feed(self, pcm: bytes):
        if not self.accepting:
            return
        with self._lock:
            self._queue.append(pcm)
        self._wake.set()

    def flush(self):
        """The server finished sending; let what is queued play out."""
        self.accepting = False
        self.playing = False

    def stop(self):
        """Barge-in: drop whatever has not been played yet."""
        self.accepting = False
        self.playing = False
        with self._lock:
            self._queue.clear()

    def close(self):
        self._running = False
        self.accepting = False
        self.playing = False
        self._wake.set()
        if self._thread:
            self._thread.join(timeout=2)
            self._thread = None
        if self._stream:
            try:
                self._stream.stop_stream()
                self._stream.close()
            except Exception:
                pass
            self._stream = None


async def send_mic_audio(ws: websockets.WebSocketClientProtocol, stop_event: asyncio.Event, player: TTSPlayer, send_lock: asyncio.Lock):
    """Read microphone and send PCM frames to server."""
    stream = pa.open(
        format=MIC_FORMAT,
        channels=MIC_CHANNELS,
        rate=MIC_SAMPLE_RATE,
        input=True,
        frames_per_buffer=MIC_CHUNK_SAMPLES,
    )
    print(f"[mic] Recording at {MIC_SAMPLE_RATE}Hz, {MIC_CHUNK_MS}ms chunks. Speak now...")
    try:
        while not stop_event.is_set():
            data = await asyncio.get_event_loop().run_in_executor(
                None, lambda: stream.read(MIC_CHUNK_SAMPLES, exception_on_overflow=False)
            )
            if MIC_MUTE_DURING_TTS and player.playing:
                continue
            async with send_lock:
                await ws.send(data)
    except (asyncio.CancelledError, websockets.exceptions.ConnectionClosed):
        pass
    finally:
        stream.stop_stream()
        stream.close()


async def send_camera_frames(ws: websockets.WebSocketClientProtocol, camera: CameraCapture, stop_event: asyncio.Event, send_lock: asyncio.Lock):
    """Upload one current JPEG per interval; the server retains only the latest."""
    last_version = 0
    sent_frames = 0
    try:
        while not stop_event.is_set():
            frame, version = camera.latest()
            if frame is not None and version != last_version:
                payload = {"type": "video_frame", "mime_type": "image/jpeg", "data": base64.b64encode(frame).decode("ascii")}
                async with send_lock:
                    await ws.send(json.dumps(payload, separators=(",", ":")))
                last_version = version
                sent_frames += 1
                if sent_frames == 1:
                    print(f"[camera] Uploading JPEG frames every {VIDEO_SEND_INTERVAL_SECONDS:.1f}s")
            await asyncio.sleep(max(VIDEO_SEND_INTERVAL_SECONDS, 0.1))
    except (asyncio.CancelledError, websockets.exceptions.ConnectionClosed):
        pass
    except Exception as exc:
        print(f"[camera] Upload failed: {exc}")


async def recv_messages(ws: websockets.WebSocketClientProtocol, player: TTSPlayer, stop_event: asyncio.Event):
    """Receive and handle server messages."""
    last_partial = ""
    try:
        async for message in ws:
            if stop_event.is_set():
                break
            if isinstance(message, bytes):
                player.feed(message)
                continue

            msg = json.loads(message)
            msg_type = msg.get("type", "")

            if msg_type == "partial":
                text = msg.get("transcript", {}).get("text", "")
                if text and text != last_partial:
                    sys.stdout.write(f"\r\033[K  [ASR partial] {text}")
                    sys.stdout.flush()
                    last_partial = text

            elif msg_type == "sentence":
                text = msg.get("transcript", {}).get("text", "")
                if last_partial:
                    sys.stdout.write("\r\033[K")
                    sys.stdout.flush()
                    last_partial = ""
                # The server first emits the recognized sentence, then emits a
                # final turn-complete frame with the same transcript after LLM/TTS.
                if not msg.get("final", False):
                    print(f"  [ASR final] {text}")

            elif msg_type == "llm_delta":
                delta = msg.get("response", {}).get("text", "")
                if delta:
                    sys.stdout.write(delta)
                    sys.stdout.flush()

            elif msg_type == "tts_start":
                sr = msg.get("sample_rate", 16000)
                print(f"\n  [TTS start] sample_rate={sr}")
                player.start(sr)

            elif msg_type == "tts_end":
                player.flush()
                print("  [TTS end]")

            elif msg_type == "interrupt":
                # The server cancelled the turn. Drop what is queued rather than
                # letting it play out — playing it is the opposite of interrupting.
                player.stop()
                print(f"\n  [interrupt] {msg.get('error', '')}")

            elif msg_type == "tts_error":
                print(f"\n  [TTS error] {msg.get('error', '')}")
                player.stop()

            elif "error" in msg:
                print(f"\n  [error] {msg.get('error', '')} {msg.get('detail', '')}")

    except (asyncio.CancelledError, websockets.exceptions.ConnectionClosed):
        pass


async def main():
    player = TTSPlayer()
    stop_event = asyncio.Event()
    camera = None

    url = f"{SERVER_URL}?user_id={USER_ID}&session_id={SESSION_ID}&sample_rate={MIC_SAMPLE_RATE}"
    print(f"[client] Connecting to {url}")

    try:
        if VIDEO_ENABLED:
            camera = CameraCapture(VIDEO_DEVICE_INDEX)
            camera.start()
            print(f"[camera] Enabled: device={VIDEO_DEVICE_INDEX}, interval={VIDEO_SEND_INTERVAL_SECONDS:.1f}s, max={VIDEO_MAX_WIDTH}x{VIDEO_MAX_HEIGHT}")

        async with websockets.connect(url, max_size=10 * 1024 * 1024, ping_interval=30, ping_timeout=60, proxy=None) as ws:
            print("[client] Connected. Press Ctrl+C to stop.\n")
            send_lock = asyncio.Lock()
            tasks = [
                asyncio.create_task(send_mic_audio(ws, stop_event, player, send_lock)),
                asyncio.create_task(recv_messages(ws, player, stop_event)),
            ]
            if camera is not None:
                tasks.append(asyncio.create_task(send_camera_frames(ws, camera, stop_event, send_lock)))

            loop = asyncio.get_event_loop()
            loop.add_signal_handler(signal.SIGINT, lambda: stop_event.set())
            await stop_event.wait()

            print("\n[client] Stopping...")
            async with send_lock:
                await ws.send(json.dumps({"type": "final"}))
            await asyncio.sleep(1)

            for task in tasks:
                task.cancel()
            await asyncio.gather(*tasks, return_exceptions=True)

    except ConnectionRefusedError:
        print(f"[error] Cannot connect to {url}. Is the server running?")
        sys.exit(1)
    except Exception as e:
        print(f"[error] {e}")
        sys.exit(1)
    finally:
        if camera is not None:
            camera.close()
        player.close()
        pa.terminate()
        print("[client] Done.")


if __name__ == "__main__":
    asyncio.run(main())
