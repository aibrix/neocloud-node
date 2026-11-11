# Run Qwen with vLLM or SGLang on RunPod

These examples run `Qwen/Qwen2.5-1.5B-Instruct` on one NVIDIA A40 using an
existing neocloud Helm installation. They use explicit engine image versions:

| Example | Image | API port |
| --- | --- | --- |
| [vLLM](qwen-vllm.yaml) | `vllm/vllm-openai:v0.32.0` | 8000/TCP |
| [SGLang](qwen-sglang.yaml) | `lmsysorg/sglang:v0.5.23` | 8000/TCP |

Pinned releases: [vLLM v0.32.0](https://github.com/vllm-project/vllm/releases/tag/v0.32.0)
and [SGLang v0.5.23](https://github.com/sgl-project/sglang/releases/tag/v0.5.23).

## Prerequisites

- One Helm release in `aibrix-system`, configured with `provider: runpod` and
  virtual node name `neocloud-node`.
- A user-managed RunPod credential Secret referenced by the Helm values.
  Credentials belong to the controller; neither example needs the RunPod key.
  If the Secret was created after controller startup, restart the controller
  to load it. See the [Helm guide](../../charts/neocloud-node/README.md).
- An available A40 in RunPod Secure Cloud and sufficient account balance.
  Applying a sample creates a paid remote instance. Run the examples one at a
  time when the virtual node advertises one GPU.

Both examples deliberately omit inference API authentication for a short smoke
test. Their exposed endpoint is unauthenticated; delete the test Pod afterward.
They use FP16, a 2,048-token context, and disable CUDA graphs to simplify startup.

The provider toleration allows these explicitly selected workloads onto the
RunPod virtual node. The second toleration is a DOKS-specific workaround for
[issue #73](https://github.com/aibrix/neocloud-node/issues/73). On other clusters,
remove that toleration. Do not alter DOKS-managed readiness rules or fake node
conditions to clear the taint.

## Start one engine

From the repository root, select one file:

```bash
SAMPLE=examples/inference/qwen-vllm.yaml
POD=qwen-vllm
# Alternatively:
# SAMPLE=examples/inference/qwen-sglang.yaml
# POD=qwen-sglang

kubectl apply -f "$SAMPLE"
kubectl -n aibrix-system get pod "$POD" -w
```

Stop the watch with Ctrl-C. `Running` / `Ready` currently reflects provider
status, not model readiness. Image pull and model initialization may take several
minutes. Check the HTTP API below before sending inference requests.

Find the remote instance ID:

```bash
kubectl -n aibrix-system get cloudpodbindings \
  -l "cloud.aibrix.ai/pod-name=$POD" \
  -o jsonpath='{range .items[*]}{.status.remoteID}{"\n"}{end}'
```

Open that instance in the RunPod console and locate its public TCP mapping for
container port **8000**. Use the public IP and the **mapped external port**;
the external port need not be 8000. Both samples request Secure Cloud.

```bash
BASE_URL=http://<public-ip>:<mapped-port>
curl --fail --max-time 15 "$BASE_URL/v1/models"
```

A successful response listing `Qwen/Qwen2.5-1.5B-Instruct` indicates that the API
is ready. Retry after a short delay if the connection is not ready yet.

## Send a request

The same OpenAI-compatible request works for both engines:

```bash
curl --fail --max-time 120 "$BASE_URL/v1/chat/completions" \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "Qwen/Qwen2.5-1.5B-Instruct",
    "messages": [{"role": "user", "content": "What is 2 + 2? Answer with only the number."}],
    "temperature": 0,
    "max_tokens": 64
  }'
```

Check that the response contains `choices[0].message.content` with the answer
`4`. For a second request, try `请用中文用一句话解释什么是 GPU。`.

This path uses the provider's public TCP endpoint. The current adapter does not
support `kubectl logs`, `exec`, or `port-forward`, and these samples do not claim
Kubernetes Service connectivity or probe-based readiness. Inspect startup logs
in RunPod if `/v1/models` never becomes reachable.

## Clean up before trying the other engine

```bash
kubectl delete -f "$SAMPLE" --wait=true --timeout=120s
kubectl -n aibrix-system get cloudpodbindings \
  -l "cloud.aibrix.ai/pod-name=$POD"
```

Confirm that the binding disappears **and** the corresponding instance is gone
in RunPod. A stopped instance can still incur storage charges; it should be
deleted. If cleanup fails, resolve or delete that specific remote instance in
RunPod before starting another test. Keep the Helm release and provider Secret
for future tests.
