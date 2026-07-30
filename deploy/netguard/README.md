# BTC09 production VPS network guard

This package adds a narrow, reversible first-line filter to existing BTC09
DigitalOcean VPSs. It does not move services or change DNS.

The XDP program looks only at new TCP SYN packets for ports 22, 80, 443, and
9009. It applies generous per-source and global rates before Linux allocates
normal TCP state. Established sessions and unrelated traffic pass unchanged.
If native or generic XDP cannot load, the service applies equivalent nftables
meters instead.

The package also applies conservative Linux SYN/backlog settings and exposes
drop counters through `btc09-netguard-status`. A one-minute systemd timer
records the counters, SYN backlog, P2P connections, and conntrack use in the
journal. It raises the journal priority when the early-drop count increases.

The OpenSSH drop-in limits unauthenticated connections from one source and
shortens their login grace period. It does not change public keys, disable root
login, or alter established SSH sessions.

Build on Ubuntu 24.04:

```bash
apt-get update
apt-get install -y xdp-tools clang llvm libbpf-dev build-essential
clang -O2 -g -target bpf -D__TARGET_ARCH_x86 \
  -I/usr/include/x86_64-linux-gnu -Wall -Werror \
  -c btc09_xdp_guard.c -o btc09_xdp_guard.o
bpftool prog load btc09_xdp_guard.o \
  /sys/fs/bpf/btc09-netguard-verify type xdp
rm /sys/fs/bpf/btc09-netguard-verify
```

Install after recording the existing nftables, sysctl, and SSH configuration:

```bash
install -d -m 0755 /opt/btc09-netguard
install -m 0644 btc09_xdp_guard.o btc09-netguard.nft \
  /opt/btc09-netguard/
install -m 0755 btc09-netguard-start btc09-netguard-stop \
  btc09-netguard-status btc09-netguard-watch /usr/local/sbin/
install -m 0644 btc09-netguard.service btc09-netguard-watch.service \
  btc09-netguard-watch.timer /etc/systemd/system/
install -m 0644 99-btc09-netguard.conf /etc/sysctl.d/
install -m 0644 99-btc09-connection-guard.conf /etc/ssh/sshd_config.d/
sshd -t
nft -c -f /opt/btc09-netguard/btc09-netguard.nft
systemctl daemon-reload
sysctl --system
systemctl reload ssh
systemctl enable --now btc09-netguard btc09-netguard-watch.timer
btc09-netguard-status
```

Rollback:

```text
systemctl disable --now btc09-netguard-watch.timer btc09-netguard
rm -f /etc/sysctl.d/99-btc09-netguard.conf \
  /etc/ssh/sshd_config.d/99-btc09-connection-guard.conf
sshd -t
systemctl reload ssh
# Restore the values recorded in /root/btc09-netguard-backup-*/sysctl.before,
# or reboot so the remaining system configuration is reapplied cleanly.
```

This protects the VPS CPU, socket queues, and BTC09 process against SYN floods
that reach the host. It cannot recover bandwidth already saturated or traffic
already null-routed by the provider.
