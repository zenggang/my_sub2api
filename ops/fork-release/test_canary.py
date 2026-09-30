import pathlib
import re
import unittest


HERE = pathlib.Path(__file__).resolve().parent


class CanaryContractTests(unittest.TestCase):
    def test_native_model_matrix_uses_supported_sol_generation(self):
        script = (HERE / "canary.sh").read_text()
        models = re.search(r"^for model in (.+); do$", script, re.MULTILINE).group(1).split()
        self.assertEqual(models, [
            "gpt-5.5", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-6.1-sol",
        ])
        self.assertIn('"$HERE/fork-remote.sh" --check', script)
        self.assertIn('"$HERE/smoke-gateway.py" 18081 "$model" "$key_id"', script)

    def test_response_model_and_tool_completion_checks_remain_strict(self):
        smoke = (HERE / "smoke-gateway.py").read_text()
        self.assertIn("terminal.get('response',{}).get('model')!=model", smoke)
        self.assertIn("raise RuntimeError('downstream model name changed')", smoke)
        self.assertIn("terminal.get('type')!='response.completed'", smoke)
        self.assertIn("calls[0].get('namespace')!='compat_probe'", smoke)
        self.assertIn("raise RuntimeError('missing final answer')", smoke)
