#!/usr/bin/env bash
set -euo pipefail

: "${FUNASR_HOME:?FUNASR_HOME must point to the Fun-ASR checkout}"

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
patch_file="$script_dir/segment-end.patch"

if git -C "$FUNASR_HOME" apply --reverse --check "$patch_file" >/dev/null 2>&1; then
  :
elif git -C "$FUNASR_HOME" apply --check "$patch_file" >/dev/null 2>&1; then
  git -C "$FUNASR_HOME" apply "$patch_file"
else
  printf "%s\n" "segment-end.patch does not match $FUNASR_HOME/serve_realtime_ws.py" >&2
  exit 1
fi

export FUNASR_BIND_HOST="${FUNASR_BIND_HOST:-127.0.0.1}"
export VLLM_USE_FLASHINFER_SAMPLER="${VLLM_USE_FLASHINFER_SAMPLER:-0}"

vad_extra_args=()
if [[ -n "${FUNASR_VAD_SILENCE_SCHEDULE:-}" ]]; then
  vad_extra_args+=(--vad-silence-schedule "$FUNASR_VAD_SILENCE_SCHEDULE")
fi
if [[ -n "${FUNASR_VAD_SPEECH_NOISE_THRES:-}" ]]; then
  vad_extra_args+=(--vad-speech-noise-thres "$FUNASR_VAD_SPEECH_NOISE_THRES")
fi

exec "${FUNASR_PYTHON:-python3}" "$FUNASR_HOME/serve_realtime_ws.py" \
  --port "${FUNASR_PORT:-10095}" \
  --language "${FUNASR_LANGUAGE:-中文}" \
  --model "${FUNASR_MODEL:-FunAudioLLM/Fun-ASR-Nano-2512}" \
  --disable-spk \
  --tensor-parallel-size "${FUNASR_TENSOR_PARALLEL_SIZE:-1}" \
  --gpu-memory-utilization "${FUNASR_GPU_MEMORY_UTILIZATION:-0.7}" \
  --decode-interval "${FUNASR_DECODE_INTERVAL_SECONDS:-0.8}" \
  --ws-ping-interval "${FUNASR_WS_PING_INTERVAL_SECONDS:-20}" \
  --ws-ping-timeout "${FUNASR_WS_PING_TIMEOUT_SECONDS:-60}" \
  "${vad_extra_args[@]+${vad_extra_args[@]}}"
