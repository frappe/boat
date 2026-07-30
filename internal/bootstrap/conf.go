package bootstrap

// sysctlConf is the kernel-tuning drop-in, verbatim from bootstrap-server.py. The
// forwarding + proxy_ndp lines are load-bearing for the routed-tap VM networking
// (each VM is a tap the host routes eth0<->tap, which IS forwarding); the rest are
// CIS 3.3 network-hardening controls a routing host still wants. rp_filter is
// deliberately omitted — strict reverse-path filtering can drop the asymmetric
// traffic of the routed-tap topology.
const sysctlConf = `# --- VM networking (required; deliberate CIS 3.3.1 deviation, v4 + v6) ---
net.ipv6.conf.all.forwarding = 1
net.ipv6.conf.default.forwarding = 1
net.ipv6.conf.all.proxy_ndp = 1
net.ipv4.ip_forward = 1

# --- CIS 3.3 network hardening (compatible with a routing host) ---
net.ipv4.conf.all.accept_redirects = 0
net.ipv4.conf.default.accept_redirects = 0
net.ipv4.conf.all.secure_redirects = 0
net.ipv4.conf.default.secure_redirects = 0
net.ipv6.conf.all.accept_redirects = 0
net.ipv6.conf.default.accept_redirects = 0
net.ipv4.conf.all.send_redirects = 0
net.ipv4.conf.default.send_redirects = 0
net.ipv4.conf.all.accept_source_route = 0
net.ipv4.conf.default.accept_source_route = 0
net.ipv6.conf.all.accept_source_route = 0
net.ipv6.conf.default.accept_source_route = 0
net.ipv4.conf.all.log_martians = 1
net.ipv4.conf.default.log_martians = 1
net.ipv4.icmp_ignore_bogus_error_responses = 1
net.ipv4.icmp_echo_ignore_broadcasts = 1
net.ipv4.tcp_syncookies = 1
net.ipv6.conf.all.accept_ra = 0
net.ipv6.conf.default.accept_ra = 0
`
