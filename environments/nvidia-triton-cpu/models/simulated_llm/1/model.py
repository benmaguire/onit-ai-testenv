"""simulated_llm: an LLM-shaped CPU model for hosting tests.

Takes PROMPT (string) and MAX_TOKENS (int32), sleeps
base_latency_ms + per_token_ms * MAX_TOKENS (per batch: the slowest request),
and returns TEXT with MAX_TOKENS pseudo tokens and TOKENS (count). Latency
parameters come from config.pbtxt, so the model behaves like a serving engine
whose throughput depends on batching and load, without any ML dependencies.
"""
import hashlib
import json
import time

import numpy as np
import triton_python_backend_utils as pb_utils

WORDS = ("alpha bravo charlie delta echo foxtrot golf hotel india juliet kilo lima "
         "mike november oscar papa quebec romeo sierra tango uniform victor whiskey").split()


class TritonPythonModel:
    def initialize(self, args):
        params = json.loads(args["model_config"]).get("parameters", {})
        self.base_ms = float(params.get("base_latency_ms", {}).get("string_value", "40"))
        self.per_token_ms = float(params.get("per_token_ms", {}).get("string_value", "2"))

    def execute(self, requests):
        prompts, counts = [], []
        for request in requests:
            p = pb_utils.get_input_tensor_by_name(request, "PROMPT").as_numpy().reshape(-1)
            n = pb_utils.get_input_tensor_by_name(request, "MAX_TOKENS").as_numpy().reshape(-1)
            prompts.append(p[0].decode() if isinstance(p[0], bytes) else str(p[0]))
            counts.append(int(max(1, min(4096, n[0]))))
        # One forward "pass" for the whole batch, like a batched engine.
        time.sleep((self.base_ms + self.per_token_ms * max(counts)) / 1000.0)
        responses = []
        for prompt, n in zip(prompts, counts):
            seed = int(hashlib.sha256(prompt.encode()).hexdigest()[:8], 16)
            text = " ".join(WORDS[(seed + i) % len(WORDS)] for i in range(n))
            responses.append(pb_utils.InferenceResponse(output_tensors=[
                pb_utils.Tensor("TEXT", np.array([[text.encode()]], dtype=np.object_)),
                pb_utils.Tensor("TOKENS", np.array([[n]], dtype=np.int32)),
            ]))
        return responses
