"""add_sub: OUTPUT0 = INPUT0 + INPUT1, OUTPUT1 = INPUT0 - INPUT1 (Triton's canonical Python backend example)."""
import numpy as np
import triton_python_backend_utils as pb_utils


class TritonPythonModel:
    def execute(self, requests):
        responses = []
        for request in requests:
            a = pb_utils.get_input_tensor_by_name(request, "INPUT0").as_numpy()
            b = pb_utils.get_input_tensor_by_name(request, "INPUT1").as_numpy()
            responses.append(pb_utils.InferenceResponse(output_tensors=[
                pb_utils.Tensor("OUTPUT0", (a + b).astype(np.float32)),
                pb_utils.Tensor("OUTPUT1", (a - b).astype(np.float32)),
            ]))
        return responses
