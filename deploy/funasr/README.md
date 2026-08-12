# Private FunASR Deployment

The selected model is `FunAudioLLM/Fun-ASR-Nano-2512`. It provides the
real-time WebSocket protocol used by this service and fits the available RTX
5080 16 GB as a single-GPU deployment.

Keep the ASR service private. The application connects to it through
`ws://127.0.0.1:10095` by default.

The deployment patch is based on Fun-ASR commit `b0b84dc`. It owns the loopback bind,
`SEGMENT_END`, external-VAD, and per-segment reset changes. The launcher recognizes
a clean baseline, the preceding external-VAD patch level, and the current reset-aware
patch level. It never modifies an external checkout unless `FUNASR_APPLY_PATCH=1` is
explicitly set; an unsupported source layout fails fast rather than silently starting
with a second VAD or retaining unbounded audio history.

## Install the Service

Clone the upstream service outside this repository, then create a Python
environment with the dependencies required by its current README and vLLM
guide.

```bash
git clone https://github.com/FunAudioLLM/Fun-ASR.git /opt/Fun-ASR
cd /opt/Fun-ASR
python3 -m venv .venv
. .venv/bin/activate
pip install --upgrade pip
pip install -r requirements.txt
pip install 'funasr>=1.3.3' 'vllm>=0.12.0'
```

## Start the Service

```bash
CUDA_VISIBLE_DEVICES=0 \
FUNASR_APPLY_PATCH=1 \
FUNASR_HOME=/opt/Fun-ASR \
FUNASR_PYTHON=/opt/Fun-ASR/.venv/bin/python \
bash deploy/funasr/run-funasr-nano.sh
```

The script uses one GPU and one tensor-parallel shard, enables the Chinese
model, disables speaker diarization, and binds the private endpoint to
`127.0.0.1`. On a clean matching baseline, `FUNASR_APPLY_PATCH=1` is a deliberate
opt-in to apply the deployment patch; omit it to require a pre-applied patch.

## Upgrade an Existing Checkout

After pulling this repository on a workstation that already has the previous
external-VAD patch, apply only the compatible incremental patch and then restart
the existing service:

```bash
FUNASR_HOME=/opt/Fun-ASR \
FUNASR_APPLY_PATCH=1 \
FUNASR_PATCH_ONLY=1 \
bash deploy/funasr/run-funasr-nano.sh

systemctl --user restart funasr-nano-external-vad.service
```

`FUNASR_PATCH_ONLY=1` exits after a successful patch decision; it never starts a
second foreground server. The launcher also accepts a clean upstream baseline
and applies the complete patch under the same explicit opt-in.

The script sets `VLLM_USE_FLASHINFER_SAMPLER=0` by default. This avoids the
FlashInfer JIT path, which needs `nvcc`; override it only on hosts with a full
CUDA toolkit and a reason to use that sampler.

## Application Settings

```env
VAD_PROVIDER=silero
ASR_PROVIDER=funasr
FUNASR_URL=ws://127.0.0.1:10095
FUNASR_SAMPLE_RATE=16000
FUNASR_TIMEOUT_SECONDS=30
```

The FunASR service accepts PCM16LE mono 16 kHz frames. In the selected hybrid
path, Silero VAD is the utterance-boundary authority: the application forwards
only speech frames, sends `SEGMENT_END` on every local `VADEnd`, and keeps the
same WebSocket open for the next utterance. FunASR force-decodes that segment,
returns only the newly locked sentence, then clears its per-segment audio and
sentence state before accepting the next utterance. This prevents long-lived
connections from accumulating audio history. `STOP` is sent only when the
whole microphone session ends.

For manual local WebSocket tests from a shell that configures an HTTP proxy,
set `NO_PROXY=127.0.0.1,localhost` (and `no_proxy` if needed) so the local ASR
connection is not routed through that proxy.
