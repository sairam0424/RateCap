import os
import sys
import unittest

_SRC_DIR = os.path.join(
    os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "src"
)
if _SRC_DIR not in sys.path:
    sys.path.insert(0, _SRC_DIR)

from ratecap import estimate_llm_cost


class TestEstimateLLMCost(unittest.TestCase):
    def test_sums_input_and_max_tokens(self):
        self.assertEqual(estimate_llm_cost(500, 1000), 1500)

    def test_zero_input_tokens_still_counts_max_tokens(self):
        self.assertEqual(estimate_llm_cost(0, 1000), 1000)

    def test_negative_inputs_clamp_to_zero(self):
        self.assertEqual(estimate_llm_cost(-10, -5), 0)


if __name__ == "__main__":
    unittest.main()
