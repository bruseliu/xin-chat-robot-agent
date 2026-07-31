#!/usr/bin/env python3
"""Real-time voice client for xin-chat-robot-agent.

Reads microphone audio, sends to the server via WebSocket,
prints ASR/LLM intermediate results, and plays TTS audio.
"""

import asyncio
import json
import signal
import sys
import struct
import threading
from collections import deque

import pyaudio
import websockets

SERVER_URL = "ws://192.168.2.99:8080/v1/chat/voice"
USER_ID = "test-user"
SESSION_ID = "test-session-1"
MIC_SAMPLE_RATE = 16000
MIC_CHANNELS = 1
MIC_FORMAT = pyaudio.paInt16
MIC_CHUNK_MS = 200
MIC_CHUNK_SAMPLES = MIC_SAMPLE_RATE * MIC_CHUNK_MS // 1000

pa = pyaudio.PyAudio()


class TTSPlayer:
    """Plays PCM audio in a background thread so WebSocket reads are never blocked."""

    def __init__(self):
        self._stream = None
        self._sample_rate = 16000
        self._queue = deque()
        self._lock = threading.Lock()
        self._has_data = threading.Event()
        self._finished = threading.Event()  # signals no more data coming
        self._done = threading.Event()      # signals playback thread exited
        self._thread = None
        self._interrupted = False
        self.playing = False

    def start(self, sample_rate: int):
        self._sample_rate = sample_rate or 16000
        if self._stream is not None:
            self.stop()
        self._stream = pa.open(
            format=pyaudio.paInt16,
            channels=1,
            rate=self._sample_rate,
            output=True,
            frames_per_buffer=2048,
        )
        self._interrupted = False
        self._queue.clear()
        self._has_data.clear()
        self._finished.clear()
        self._done.clear()
        self.playing = True
        self._thread = threading.Thread(target=self._play_loop, daemon=True)
        self._thread.start()

    def _play_loop(self):
        try:
            while not self._interrupted:
                if not self._queue:
                    if self._finished.is_set():
                        break
                    self._has_data.wait(timeout=0.1)
                    self._has_data.clear()
                    continue
                with self._lock:
                    chunk = self._queue.popleft()
                if self._stream and not self._stream.is_stopped():
                    self._stream.write(chunk)
            # Flush: write silence to push remaining audio through hardware buffer
            if not self._interrupted and self._stream and not self._stream.is_stopped():
                silence = b'\x00' * (self._sample_rate * 2 * 300 // 1000)
                self._stream.write(silence)
        finally:
            self._done.set()

    def feed(self, pcm: bytes):
        with self._lock:
            self._queue.append(pcm)
        self._has_data.set()

    def flush(self):
        """Signal no more data and wait for playback to complete."""
        self._finished.set()
        self._has_data.set()
        self._done.wait(timeout=300)
        self._close_stream()
        self.playing = False

    def stop(self):
        """Immediately interrupt playback (barge-in)."""
        self._interrupted = True
        self._finished.set()
        self._has_data.set()
        self._done.wait(timeout=2)
        self._close_stream()
        self.playing = False

    def _close_stream(self):
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


async def send_mic_audio(ws: websockets.WebSocketClientProtocol, stop_event: asyncio.Event, player: TTSPlayer):
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
            if not player.playing:
                await ws.send(data)
    except (asyncio.CancelledError, websockets.exceptions.ConnectionClosed):
        pass
    finally:
        stream.stop_stream()
        stream.close()


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
                await asyncio.get_event_loop().run_in_executor(None, player.flush)
                print("  [TTS end]")

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

    url = f"{SERVER_URL}?user_id={USER_ID}&session_id={SESSION_ID}&sample_rate={MIC_SAMPLE_RATE}"
    print(f"[client] Connecting to {url}")

    try:
        async with websockets.connect(url, max_size=10 * 1024 * 1024, ping_interval=30, ping_timeout=60) as ws:
            print("[client] Connected. Press Ctrl+C to stop.\n")

            send_task = asyncio.create_task(send_mic_audio(ws, stop_event, player))
            recv_task = asyncio.create_task(recv_messages(ws, player, stop_event))

            loop = asyncio.get_event_loop()
            loop.add_signal_handler(signal.SIGINT, lambda: stop_event.set())

            await stop_event.wait()

            print("\n[client] Stopping...")
            await ws.send(json.dumps({"type": "final"}))
            await asyncio.sleep(1)

            send_task.cancel()
            recv_task.cancel()
            await asyncio.gather(send_task, recv_task, return_exceptions=True)

    except ConnectionRefusedError:
        print(f"[error] Cannot connect to {url}. Is the server running?")
        sys.exit(1)
    except Exception as e:
        print(f"[error] {e}")
        sys.exit(1)
    finally:
        player.stop()
        pa.terminate()
        print("[client] Done.")


if __name__ == "__main__":
    asyncio.run(main())
