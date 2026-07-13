from pathlib import Path
import unittest


ROOT = Path(__file__).resolve().parents[2]
HTTP = ROOT / "deploy" / "nginx" / "bitcoin09-open-miner-http.conf"
SERVER = ROOT / "deploy" / "nginx" / "bitcoin09-open-miner-server.conf"
INSTALLER = ROOT / "deploy" / "scripts" / "install-open-miner.sh"


class OpenMinerDeploymentContract(unittest.TestCase):
    def test_http_limits_are_keyed_by_restored_client_ip(self) -> None:
        text = HTTP.read_text(encoding="ascii")
        self.assertIn("limit_req_zone $binary_remote_addr zone=btc09_miner_work", text)
        self.assertIn("limit_req_zone $binary_remote_addr zone=btc09_miner_submit", text)
        self.assertIn("limit_conn_zone $binary_remote_addr zone=btc09_miner_connections", text)

    def test_only_exact_post_routes_reach_loopback_coordinator(self) -> None:
        text = SERVER.read_text(encoding="ascii")
        self.assertEqual(text.count("proxy_pass http://127.0.0.1:9010;"), 2)
        self.assertIn("location = /api/v1/work", text)
        self.assertIn("location = /api/v1/submit", text)
        self.assertEqual(text.count("limit_except POST"), 2)
        self.assertNotIn("location /api/v1/", text)
        self.assertNotIn("server_name", text)
        self.assertNotIn("0.0.0.0:9010", text)
        self.assertNotIn("proxy_pass http://$", text)

    def test_proxy_has_bounded_requests_connections_and_timeouts(self) -> None:
        text = SERVER.read_text(encoding="ascii")
        for required in (
            "client_max_body_size 4k;",
            "limit_conn btc09_miner_connections 4;",
            "proxy_request_buffering on;",
            "proxy_connect_timeout 2s;",
            "proxy_send_timeout 10s;",
            "proxy_read_timeout 35s;",
            "proxy_hide_header X-Content-Type-Options;",
            "proxy_set_header X-Real-IP $remote_addr;",
        ):
            self.assertIn(required, text)

    def test_installer_validates_and_rolls_back_before_reload(self) -> None:
        text = INSTALLER.read_text(encoding="ascii")
        for required in (
            "nginx -t",
            "systemctl reload nginx",
            "curl --fail --silent --show-error",
            "127.0.0.1:9010/api/v1/work",
            "for _attempt in {1..10}",
            "restore_nginx",
            "trap",
        ):
            self.assertIn(required, text)


if __name__ == "__main__":
    unittest.main()
