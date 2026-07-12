import importlib.util
import json
import pathlib
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


MODULE_PATH = pathlib.Path(__file__).with_name("btc09_exchange_smoke.py")
TIP_HASH = "ab" * 32
VALID_ADDRESS = "4kSmokeTestAddress"


def load_smoke_module():
    spec = importlib.util.spec_from_file_location("btc09_exchange_smoke", MODULE_PATH)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class FixtureHandler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, _format, *_args):
        return

    def do_GET(self):
        fixture = self.server.fixture
        self.server.client_ports.add(self.client_address[1])
        self.server.last_user_agent = self.headers.get("User-Agent", "")
        if self.path == "/api/v1/tip":
            self._json(fixture["tip"])
            return
        if self.path == "/api/v1/block/" + TIP_HASH:
            self._json(fixture["block"])
            return
        if self.path.startswith("/api/v1/address/"):
            self.server.last_address_path = self.path
            self._json(fixture["address"])
            return
        self.send_error(404)

    def _json(self, value):
        body = json.dumps(value).encode("utf-8")
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


class ExchangeSmokeTest(unittest.TestCase):
    def setUp(self):
        fixture = {
            "tip": {
                "schema_version": 1,
                "network": "btc09-mainnet",
                "tip": {"hash": TIP_HASH, "height": 42},
            },
            "block": {
                "schema_version": 1,
                "network": "btc09-mainnet",
                "found": True,
                "block": {"hash": TIP_HASH, "height": 42, "canonical": True},
                "tip": {"hash": TIP_HASH, "height": 42},
            },
            "address": {
                "schema_version": 1,
                "network": "btc09-mainnet",
                "address": VALID_ADDRESS,
                "complete": True,
                "tip": {"hash": TIP_HASH, "height": 42},
                "outputs": [{"txid": "cd" * 32, "vout": 0}],
            },
        }
        self.server = ThreadingHTTPServer(("127.0.0.1", 0), FixtureHandler)
        self.server.fixture = fixture
        self.server.last_address_path = ""
        self.server.last_user_agent = ""
        self.server.client_ports = set()
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.base_url = "http://127.0.0.1:" + str(self.server.server_port)

    def tearDown(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=2)

    def smoke(self):
        self.assertTrue(MODULE_PATH.exists(), "implementation module missing")
        return load_smoke_module()

    def test_check_exchange_api_pins_optional_address_scan(self):
        result = self.smoke().check_exchange_api(self.base_url, VALID_ADDRESS, 2.0)
        self.assertTrue(result["ok"])
        self.assertEqual(result["network"], "btc09-mainnet")
        self.assertEqual(result["height"], 42)
        self.assertEqual(result["address_outputs"], 1)
        self.assertIn("expected_tip_hash=" + TIP_HASH, self.server.last_address_path)
        self.assertIn("expected_tip_height=42", self.server.last_address_path)

    def test_check_exchange_api_uses_project_user_agent(self):
        self.smoke().check_exchange_api(self.base_url, None, 2.0)
        self.assertEqual(self.server.last_user_agent, "btc09-exchange-smoke/1")

    def test_check_exchange_api_reuses_one_http_connection(self):
        self.smoke().check_exchange_api(self.base_url, None, 2.0)
        self.assertEqual(len(self.server.client_ports), 1)

    def test_check_exchange_api_rejects_noncanonical_tip_hash(self):
        self.server.fixture["tip"]["tip"]["hash"] = TIP_HASH.upper()
        smoke = self.smoke()
        with self.assertRaisesRegex(smoke.CheckFailed, "tip hash"):
            smoke.check_exchange_api(self.base_url, None, 2.0)

    def test_check_exchange_api_rejects_tip_block_mismatch(self):
        self.server.fixture["block"]["block"]["height"] = 41
        smoke = self.smoke()
        with self.assertRaisesRegex(smoke.CheckFailed, "canonical tip"):
            smoke.check_exchange_api(self.base_url, None, 2.0)

    def test_check_exchange_api_rejects_unpinned_address_response(self):
        self.server.fixture["address"]["tip"]["height"] = 41
        smoke = self.smoke()
        with self.assertRaisesRegex(smoke.CheckFailed, "requested tip"):
            smoke.check_exchange_api(self.base_url, VALID_ADDRESS, 2.0)


if __name__ == "__main__":
    unittest.main()
