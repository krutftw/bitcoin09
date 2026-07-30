// SPDX-License-Identifier: GPL-2.0
//
// BTC09 production XDP SYN guard.
//
// This program deliberately has a narrow scope: it rate-limits new TCP SYN
// packets for BTC09's public services before the normal Linux network stack.
// Established traffic and all other protocols are passed unchanged.

#include <linux/bpf.h>
#include <linux/if_ether.h>
#include <linux/in.h>
#include <linux/ip.h>
#include <linux/ipv6.h>
#include <linux/tcp.h>
#include <bpf/bpf_endian.h>
#include <bpf/bpf_helpers.h>

#define WINDOW_NS 1000000000ULL

enum service_index {
	SERVICE_SSH = 0,
	SERVICE_HTTP = 1,
	SERVICE_HTTPS = 2,
	SERVICE_P2P = 3,
	SERVICE_COUNT = 4,
};

enum stat_index {
	STAT_SYN_SEEN = 0,
	STAT_SYN_ALLOWED = 1,
	STAT_DROP_SOURCE = 2,
	STAT_DROP_GLOBAL = 3,
	STAT_PARSE_PASS = 4,
	STAT_COUNT = 5,
};

struct source_key {
	__u8 family;
	__u8 pad1;
	__u16 port;
	__u8 address[16];
};

struct rate_state {
	__u64 window_start_ns;
	__u32 count;
	__u32 pad;
};

struct service_limit {
	__u32 per_source;
	__u32 global;
};

struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, 65536);
	__type(key, struct source_key);
	__type(value, struct rate_state);
} btc09_source_rates SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__uint(max_entries, SERVICE_COUNT);
	__type(key, __u32);
	__type(value, struct rate_state);
} btc09_global_rates SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
	__uint(max_entries, STAT_COUNT);
	__type(key, __u32);
	__type(value, __u64);
} btc09_stats SEC(".maps");

static __always_inline void count_stat(__u32 key)
{
	__u64 *value = bpf_map_lookup_elem(&btc09_stats, &key);

	if (value)
		*value += 1;
}

static __always_inline int service_for_port(__u16 port,
					    struct service_limit *limit)
{
	switch (port) {
	case 22:
		limit->per_source = 30;
		limit->global = 100;
		return SERVICE_SSH;
	case 80:
		limit->per_source = 500;
		limit->global = 2000;
		return SERVICE_HTTP;
	case 443:
		limit->per_source = 500;
		limit->global = 2000;
		return SERVICE_HTTPS;
	case 9009:
		limit->per_source = 20;
		limit->global = 2000;
		return SERVICE_P2P;
	default:
		return -1;
	}
}

static __always_inline int over_limit(struct rate_state *state, __u64 now,
				      __u32 limit)
{
	if (now - state->window_start_ns >= WINDOW_NS) {
		state->window_start_ns = now;
		state->count = 1;
		return 0;
	}

	// BPF's atomic add instruction does not return the old value. Reading the
	// shared counter after the add is sufficient here; both VPS NICs currently
	// expose a single RX queue, and the global threshold remains conservative.
	__sync_fetch_and_add(&state->count, 1);
	return state->count > limit;
}

static __always_inline int check_rate(struct source_key *source,
				      __u32 service,
				      const struct service_limit *limit)
{
	struct rate_state initial = {};
	struct rate_state *source_state;
	struct rate_state *global_state;
	__u64 now = bpf_ktime_get_ns();

	source_state = bpf_map_lookup_elem(&btc09_source_rates, source);
	if (!source_state) {
		initial.window_start_ns = now;
		initial.count = 1;
		bpf_map_update_elem(&btc09_source_rates, source, &initial,
				    BPF_NOEXIST);
	} else if (over_limit(source_state, now, limit->per_source)) {
		count_stat(STAT_DROP_SOURCE);
		return XDP_DROP;
	}

	global_state = bpf_map_lookup_elem(&btc09_global_rates, &service);
	if (global_state &&
	    over_limit(global_state, now, limit->global)) {
		count_stat(STAT_DROP_GLOBAL);
		return XDP_DROP;
	}

	count_stat(STAT_SYN_ALLOWED);
	return XDP_PASS;
}

static __always_inline int inspect_tcp(void *data_end, struct tcphdr *tcp,
				       struct source_key *source)
{
	struct service_limit limit = {};
	__u16 destination;
	int service;

	if ((void *)(tcp + 1) > data_end) {
		count_stat(STAT_PARSE_PASS);
		return XDP_PASS;
	}

	// Only initial SYNs are rate-limited. SYN-ACKs and established traffic pass.
	if (!tcp->syn || tcp->ack)
		return XDP_PASS;

	destination = bpf_ntohs(tcp->dest);
	service = service_for_port(destination, &limit);
	if (service < 0)
		return XDP_PASS;

	source->port = destination;
	count_stat(STAT_SYN_SEEN);
	return check_rate(source, (__u32)service, &limit);
}

SEC("xdp")
int btc09_xdp_guard(struct xdp_md *ctx)
{
	void *data = (void *)(long)ctx->data;
	void *data_end = (void *)(long)ctx->data_end;
	struct ethhdr *ethernet = data;
	struct source_key source = {};
	__u16 protocol;

	if ((void *)(ethernet + 1) > data_end) {
		count_stat(STAT_PARSE_PASS);
		return XDP_PASS;
	}

	protocol = bpf_ntohs(ethernet->h_proto);

	if (protocol == ETH_P_IP) {
		struct iphdr *ipv4 = (void *)(ethernet + 1);
		struct tcphdr *tcp;
		__u32 header_length;

		if ((void *)(ipv4 + 1) > data_end) {
			count_stat(STAT_PARSE_PASS);
			return XDP_PASS;
		}
		if (ipv4->protocol != IPPROTO_TCP)
			return XDP_PASS;

		header_length = (__u32)ipv4->ihl * 4;
		if (header_length < sizeof(*ipv4) ||
		    (void *)ipv4 + header_length > data_end) {
			count_stat(STAT_PARSE_PASS);
			return XDP_PASS;
		}

		source.family = 4;
		__builtin_memcpy(source.address, &ipv4->saddr,
				 sizeof(ipv4->saddr));
		tcp = (void *)ipv4 + header_length;
		return inspect_tcp(data_end, tcp, &source);
	}

	if (protocol == ETH_P_IPV6) {
		struct ipv6hdr *ipv6 = (void *)(ethernet + 1);
		struct tcphdr *tcp;

		if ((void *)(ipv6 + 1) > data_end) {
			count_stat(STAT_PARSE_PASS);
			return XDP_PASS;
		}
		// Extension-header traffic is passed for kernel validation.
		if (ipv6->nexthdr != IPPROTO_TCP)
			return XDP_PASS;

		source.family = 6;
		__builtin_memcpy(source.address, &ipv6->saddr,
				 sizeof(ipv6->saddr));
		tcp = (void *)(ipv6 + 1);
		return inspect_tcp(data_end, tcp, &source);
	}

	return XDP_PASS;
}

char LICENSE[] SEC("license") = "GPL";
