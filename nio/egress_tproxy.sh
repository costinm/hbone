#!/usr/bin/env bash

# Egress capture.
#

PROXY_GID="1337"

# Port for capturing mesh services ( istio compat, load balancing ).
EPORT=${EPORT:-15001}

function egress_routes() {
  ip -6 addr add ::6/128 dev lo

  # The marked packets are routed to lo
  ip -4 rule add fwmark ${EPORT} lookup ${EPORT}
  ip -4 route add local default dev lo table ${EPORT}

  ip -6 rule add fwmark ${EPORT} lookup ${EPORT}
  ip -6 route add local default dev lo table ${EPORT}
}

function egress_routes_clean() {
  ip -4 rule delete from all fwmark ${EPORT} lookup ${EPORT}
  ip -4 -f inet route del local default dev lo table ${EPORT}

  ip -6 rule delete from all fwmark ${EPORT} lookup ${EPORT}
  ip -6 -f inet route del local default dev lo table ${EPORT}
}

function egress_tables() {
  egress_routes $EPORT
  _egress_tables "iptables" "127.0.0.1/32"
  _egress_tables "ip6tables" "::1/128"
}

function _egress_tables() {
  local iptables=$1
  local LOCAL_RANGE=$2

  # This table determines what gets marked - marks will result in a route to lo, and redirected
  ${iptables} -t mangle -N DMESH_MANGLE_OUT
  ${iptables} -t mangle -A OUTPUT  -j DMESH_MANGLE_OUT


  # This will redirect to tproxy anything on lo that is marked by the other rules
  ${iptables} -t mangle -N DMESH_TPROXY
  ${iptables} -t mangle -A PREROUTING -j DMESH_TPROXY

  ${iptables} -t mangle -N DMESH_CAPTURE_EGRESS
  ${iptables} -t mangle -A DMESH_CAPTURE_EGRESS -j MARK --set-mark ${EPORT}
  ${iptables} -t mangle -A DMESH_CAPTURE_EGRESS -j ACCEPT

  ${iptables} -t mangle -N DMESH_CAPTURE_MESH
  ${iptables} -t mangle -A DMESH_CAPTURE_MESH -j MARK --set-mark ${MPORT}
  ${iptables} -t mangle -A DMESH_CAPTURE_MESH -j ACCEPT

  ${iptables} -t mangle -N DMESH_CAPTURE_POD
  ${iptables} -t mangle -A DMESH_CAPTURE_POD -j MARK --set-mark ${PPORT}
  ${iptables} -t mangle -A DMESH_CAPTURE_POD -j ACCEPT

  # Added to prerouting, will redirect to tproxy:
  #  - all TCP and UDP with the mark
  #  - received on loopback
  #  - but not having dest on the loopback
  #
  # All outbound traffic to be captured will be routed using a special table that
  # has default route set to loopback interface.
  ${iptables} -t mangle -A DMESH_TPROXY -i lo -d ${LOCAL_RANGE} -j RETURN

  ${iptables} -t mangle -A DMESH_TPROXY --match mark --mark ${EPORT} -p tcp -i lo ! -d ${LOCAL_RANGE} -j TPROXY --tproxy-mark ${EPORT}/0xffffffff --on-port ${EPORT}
  ${iptables} -t mangle -A DMESH_TPROXY --match mark --mark ${EPORT} -p udp -i lo ! -d ${LOCAL_RANGE} -j TPROXY --tproxy-mark ${EPORT}/0xffffffff --on-port ${EPORT}
  ${iptables} -t mangle -A DMESH_TPROXY --match mark --mark ${MPORT} -p tcp -i lo ! -d ${LOCAL_RANGE} -j TPROXY --tproxy-mark ${MPORT}/0xffffffff --on-port ${MPORT}
  ${iptables} -t mangle -A DMESH_TPROXY --match mark --mark ${MPORT} -p udp -i lo ! -d ${LOCAL_RANGE} -j TPROXY --tproxy-mark ${MPORT}/0xffffffff --on-port ${MPORT}
  ${iptables} -t mangle -A DMESH_TPROXY --match mark --mark ${PPORT} -p tcp -i lo ! -d ${LOCAL_RANGE} -j TPROXY --tproxy-mark ${PPORT}/0xffffffff --on-port ${PPORT}
  ${iptables} -t mangle -A DMESH_TPROXY --match mark --mark ${PPORT} -p udp -i lo ! -d ${LOCAL_RANGE} -j TPROXY --tproxy-mark ${PPORT}/0xffffffff --on-port ${PPORT}

}

# Empty the configurable chains
function egress_clean() {
  iptables -t mangle -F DMESH_MANGLE_OUT
  ip6tables -t mangle -F DMESH_MANGLE_OUT
}

function egress_reset() {
  egress_clean
   _egress_reset iptables
  _egress_reset ip6tables
}

function _egress_reset() {
  local iptables=$1

  # Flush the tables
  ${iptables} -t mangle -F DMESH_CAPTURE_POD
  ${iptables} -t mangle -F DMESH_CAPTURE_EGRESS
  ${iptables} -t mangle -F DMESH_CAPTURE_MESH
  ${iptables} -t mangle -F DMESH_TPROXY

  # Remove the tables from the top chains
  ${iptables} -t mangle  -D PREROUTING -j DMESH_TPROXY
  ${iptables} -t mangle -D OUTPUT  -j DMESH_MANGLE_OUT

  # Remove the tables - now empty and not used
  ${iptables} -t mangle -X DMESH_CAPTURE_POD
  ${iptables} -t mangle -X DMESH_CAPTURE_EGRESS
  ${iptables} -t mangle -X DMESH_CAPTURE_MESH
  ${iptables} -t mangle -X DMESH_TPROXY
  ${iptables} -t mangle -X DMESH_MANGLE_OUT
}

# Decides which packets will be captured.
function egress_capture() {
  iptables -t mangle -A DMESH_MANGLE_OUT  -o lo -s 127.0.0.6/32 -j RETURN
  ip6tables -t mangle -A DMESH_MANGLE_OUT  -o lo -s ::6/128 -j RETURN

  # Loopback not impacted
  iptables -t mangle -A DMESH_MANGLE_OUT -o lo -d 127.0.0.0/8 -j RETURN

    # Instead: pod and VIP range explicitly captured
    # Redirect app calls back to itself via Envoy when using the service VIP or endpoint
    # address, e.g. appN => Envoy (client) => Envoy (server) => appN.
    #iptables -t mangle -A DMESH_MANGLE_OUT -o lo ! -d 127.0.0.1/32 -j ISTIO_IN_REDIRECT
  if [ ${#ipv4_ranges_exclude[@]} -gt 0 ]; then
    for cidr in "${ipv4_ranges_exclude[@]}"; do
      iptables -t nat -A ISTIO_OUTPUT -d "${cidr}" -j RETURN
    done
  fi

  _egress_capture iptables
  _egress_capture ip6tables

  # For Service CIDR ranges - redirect to LB service port
  # For Pod CIDR ranges - redirect to pod port

  # Everything else is public IP - redirect to egress port.

}

function _egress_capture() {
  local iptables=$1

  ${iptables} -t mangle -A DMESH_MANGLE_OUT -m owner --gid-owner ${PROXY_GID} -j RETURN
  ${iptables} -t mangle -A DMESH_MANGLE_OUT -m owner --uid-owner 0 -j RETURN

  ${iptables} -t mangle -A DMESH_MANGLE_OUT  -p tcp --dport 22 -j RETURN

  if [ -n "${OUTBOUND_PORTS_EXCLUDE}" ]; then
    for port in ${OUTBOUND_PORTS_EXCLUDE}; do
      ${iptables} -t mangle -A DMESH_MANGLE_OUT -p tcp --dport "${port}" -j RETURN
    done
  fi

  ${iptables} -t mangle -A DMESH_MANGLE_OUT  -p tcp --dport 15007 -j RETURN
  ${iptables} -t mangle -A DMESH_MANGLE_OUT  -p tcp --dport 15008 -j RETURN
  ${iptables} -t mangle -A DMESH_MANGLE_OUT  -p tcp --dport 15009 -j RETURN
  ${iptables} -t mangle -A DMESH_MANGLE_OUT  -p tcp --dport 15010 -j RETURN

  ${iptables} -t mangle -A DMESH_MANGLE_OUT  -p tcp --dport 9999 -j DMESH_CAPTURE_EGRESS
  ${iptables} -t mangle -A DMESH_MANGLE_OUT  -p udp --dport 9999 -j DMESH_CAPTURE_EGRESS


  #iptables -t mangle -A DMESH_MANGLE_PRE -m socket --transparent -j MARK --set-mark ${EPORT}
}

function egress_capture6() {
  ip6tables -t mangle -A DMESH_MANGLE_OUT  -p tcp --dport 9999 -j MARK --set-mark ${EPORT}
  ip6tables -t mangle -A DMESH_MANGLE_OUT  -p udp --dport 9999 -j MARK --set-mark ${EPORT}
}

export TUNUSER=${TUNUSER:-istio-proxy}
export N=${N:-0}

# Capture using a TUN - gVisor or lwip.
function egress_tun() {
    ip tuntap add dev dmesh${N} mode tun user ${TUNUSER} group ${TUNUSER}
    ip addr add 10.11.${N}.1/24 dev dmesh${N}
    # IP6 address may confuse linux
    ip -6 addr add fd::1:${N}/64 dev dmesh${N}
    ip link set dmesh${N} up

    # Route various ranges to dmesh1 - the gate can't initiate its own
    # connections to those ranges. Service VIPs can also use this simpler model.
    # ip route add fd::/8 dev ${N}
    ip route add 10.10.${N}.0/24 dev dmesh${N}


    # Don't remember why this was required
    echo 2 > /proc/sys/net/ipv4/conf/dmesh${N}/rp_filter
    sysctl -w net.ipv4.ip_forward=1
}

function egress_tun_cleanup() {
  # App must be stopped
  ip tuntap del dev dmesh${N} mode tun

  # ip rule delete  fwmark 1{N}1 priority 10  lookup 1{N}1
  # ip route del default dev dmesh${N} table 1{N}1

  # ip rule del fwmark 1{N}0 lookup 1{N}0
  # ip rule del iif dmesh${N} lookup 1{N}0
  # ip route del local 0.0.0.0/0 dev lo table 1{N}0
}



if [[ "$1" != "" ]]; then
  $1
fi
