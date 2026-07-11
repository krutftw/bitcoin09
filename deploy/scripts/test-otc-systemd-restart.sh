#!/usr/bin/env bash
set -euo pipefail

SERVICE=btc09-otc-bot.service
ADAPTER=/opt/btc09/bitcoin09/deploy/scripts/otc-systemd-restart-adapter.py
PYTHON=/opt/btc09/venv/bin/python
ENVIRONMENT_CHECK=/opt/btc09/bitcoin09/deploy/scripts/check-otc-process-environment.py
OVERRIDE_DIR="/run/systemd/system/$SERVICE.d"
OVERRIDE="$OVERRIDE_DIR/99-otc-restart-safety-test.conf"
TEST_PARENT=/run/btc09-otc-restart-test

fail() {
  printf '%s\n' "OTC restart safety test failed: $1" >&2
  exit 1
}

[[ $# -eq 1 && $1 == --acknowledge-service-restart ]] || fail "explicit --acknowledge-service-restart is required"
[[ $(id -u) -eq 0 ]] || fail "must run as root"
for command in systemctl runuser install flock realpath stat mktemp chown chmod dirname; do
  command -v "$command" >/dev/null 2>&1 || fail "required command is unavailable"
done
[[ -x $PYTHON && -f $ADAPTER && -x $ENVIRONMENT_CHECK ]] || fail "deployed Python or safety adapter is missing"
[[ ! -e $OVERRIDE ]] || fail "restart test override already exists"
systemctl cat "$SERVICE" >/dev/null || fail "service is not installed"
"$PYTHON" "$ENVIRONMENT_CHECK" || fail "effective production environment is unsafe"

exec 9>/run/lock/btc09-otc-restart-test.lock
flock -n 9 || fail "another restart safety test is active"

was_active=0
if systemctl is-active --quiet "$SERVICE"; then
  was_active=1
fi
[[ ! -L $TEST_PARENT ]] || fail "restart test parent must not be a symbolic link"
if [[ -e $TEST_PARENT ]]; then
  [[ -d $TEST_PARENT ]] || fail "restart test parent is not a directory"
else
  install -d -o root -g btc09-otc -m 0710 -- "$TEST_PARENT"
fi
[[ $(realpath -ms -- "$TEST_PARENT") == "$(realpath -e -- "$TEST_PARENT")" ]] || fail "restart test parent contains a symbolic link"
bot_group_id=$(id -g btc09-otc)
[[ $(stat -c '%u:%g:%a' -- "$TEST_PARENT") == "0:$bot_group_id:710" ]] || fail "restart test parent ownership or mode is unsafe"
runuser -u btc09-otc -- test -x "$TEST_PARENT" || fail "service user cannot traverse restart test parent"
runuser -u btc09-otc -- test ! -w "$TEST_PARENT" || fail "service user can write restart test parent"
test_root=$(mktemp -d -- "$TEST_PARENT/run.XXXXXXXXXX")
[[ -d $test_root && ! -L $test_root ]] || fail "restart test fixture creation failed"
chown btc09-otc:btc09-otc "$test_root"
chmod 0700 "$test_root"
db="$test_root/isolated-test.db"
wallet="$test_root/wallet-regtest.json"
state_dir="$test_root/state"

restore() {
  status=$?
  systemctl stop "$SERVICE" >/dev/null 2>&1 || true
  rm -f -- "$OVERRIDE"
  rmdir -- "$OVERRIDE_DIR" >/dev/null 2>&1 || true
  systemctl daemon-reload >/dev/null 2>&1 || true
  if [[ -n ${test_root:-} && -d $test_root && ! -L $test_root && $(dirname -- "$test_root") == "$TEST_PARENT" ]]; then
    rm -rf --one-file-system -- "$test_root"
  else
    status=1
  fi
  if [[ $was_active -eq 1 ]]; then
    systemctl start "$SERVICE" >/dev/null 2>&1 || status=1
  fi
  exit "$status"
}
trap restore EXIT HUP INT TERM

runuser -u btc09-otc -- mkdir -m 0700 -- "$state_dir"
runuser -u btc09-otc -- "$PYTHON" "$ADAPTER" prepare --db "$db" --wallet "$wallet"
install -d -o root -g root -m 0755 -- "$OVERRIDE_DIR"
printf '%s\n' \
  '[Service]' \
  'ExecStart=' \
  "ExecStart=$PYTHON $ADAPTER serve --db $db --wallet $wallet --state-dir $state_dir" \
  'Environment=OTC_ACCEPTING_ORDERS=0' \
  "ReadWritePaths=$test_root" \
  > "$OVERRIDE"
chmod 0600 "$OVERRIDE"
systemctl daemon-reload
systemctl restart btc09-otc-bot.service

wait_ready() {
  expected_generation=$1
  for _attempt in $(seq 1 150); do
    if [[ -f $state_dir/ready.json ]] && "$PYTHON" - "$state_dir/ready.json" "$expected_generation" <<'PY'
import json
import sys
payload = json.load(open(sys.argv[1], encoding="ascii"))
raise SystemExit(0 if (
    payload.get("generation") == int(sys.argv[2])
    and payload.get("recovery_ready") is True
    and payload.get("accepting_orders") is False
) else 1)
PY
    then
      return 0
    fi
    sleep 0.1
  done
  return 1
}

wait_ready 1 || fail "first isolated generation did not become recovery-ready"
[[ $(<"$state_dir/intake-enabled") == 0 ]] || fail "intake became enabled"
old_main=$(systemctl show -p MainPID --value "$SERVICE")
old_child=$("$PYTHON" -c 'import json,sys; print(json.load(open(sys.argv[1]))["child_pid"])' "$state_dir/ready.json")
control_group=$(systemctl show -p ControlGroup --value "$SERVICE")
[[ $old_main =~ ^[1-9][0-9]*$ && $old_child =~ ^[1-9][0-9]*$ && $control_group == /* ]] || fail "invalid first-generation cgroup evidence"
grep -Fxq "$old_main" "/sys/fs/cgroup$control_group/cgroup.procs" || fail "old main PID is outside the service cgroup"
grep -Fxq "$old_child" "/sys/fs/cgroup$control_group/cgroup.procs" || fail "old child PID is outside the service cgroup"

runuser -u btc09-otc -- "$PYTHON" "$ADAPTER" inject --db "$db" --wallet "$wallet"
systemctl restart btc09-otc-bot.service
for _attempt in $(seq 1 150); do
  [[ ! -e /proc/$old_main && ! -e /proc/$old_child ]] && break
  sleep 0.1
done
[[ ! -e /proc/$old_main && ! -e /proc/$old_child ]] || fail "old main or long-lived child survived restart"
wait_ready 2 || fail "new generation did not complete prepared-row recovery"
[[ $(<"$state_dir/intake-enabled") == 0 ]] || fail "intake became enabled during recovery"
new_main=$(systemctl show -p MainPID --value "$SERVICE")
new_child=$("$PYTHON" -c 'import json,sys; print(json.load(open(sys.argv[1]))["child_pid"])' "$state_dir/ready.json")
[[ $new_main != "$old_main" && $new_child != "$old_child" ]] || fail "worker generation overlapped after restart"
runuser -u btc09-otc -- "$PYTHON" "$ADAPTER" verify --db "$db" --state-dir "$state_dir"

printf '%s\n' "OTC systemd restart safety test passed: old cgroup gone, intake disabled, prepared expected_txid recovered"
