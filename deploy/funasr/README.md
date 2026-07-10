# Private FunASR Deployment

The selected model is `FunAudioLLM/Fun-ASR-Nano-2512`. It provides the
real-time WebSocket protocol used by this service and fits the available RTX
5080 16 GB as a single-GPU deployment.

Keep the ASR service private. The application connects to it through
`ws://127.0.0.1:10095` by default.

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
FUNASR_HOME=/opt/Fun-ASR \
FUNASR_PYTHON=/opt/Fun-ASR/.venv/bin/python \
bash deploy/funasr/run-funasr-nano.sh
```

The script uses one GPU and one tensor-parallel shard, enables the Chinese
model, and disables speaker diarization. It also applies
`bind-loopback.patch` to the upstream service when necessary, because the
current upstream script otherwise listens on every network interface. The
private service therefore binds to `127.0.0.1` by default.

The script sets `VLLM_USE_FLASHINFER_SAMPLER=0` by default. This avoids the
FlashInfer JIT path, which needs `nvcc`; override it only on hosts with a full
CUDA toolkit and a reason to use that sampler.

## Application Settings

```env
VAD_PROVIDER=passthrough
ASR_PROVIDER=funasr
FUNASR_URL=ws://127.0.0.1:10095
FUNASR_SAMPLE_RATE=16000
FUNASR_TIMEOUT_SECONDS=30
```

The FunASR service accepts PCM16LE mono 16 kHz frames. It handles endpointing
itself, so the application forwards both speech and silence. Each locked
FunASR sentence triggers one text turn while the WebSocket stays open.

For manual local WebSocket tests from a shell that configures an HTTP proxy,
set `NO_PROXY=127.0.0.1,localhost` (and `no_proxy` if needed) so the local ASR
connection is not routed through that proxy.
