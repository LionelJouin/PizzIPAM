#!/usr/bin/env bash
# Convert an IPv4/IPv6 address to its integer form (for spec.baseInt).
#   ./scripts/ip2int.sh 192.168.0.128   -> 3232235648
set -euo pipefail
python3 -c "import ipaddress,sys; print(int(ipaddress.ip_address(sys.argv[1])))" "$1"
