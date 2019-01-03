# Copyright 2018 Datawire. All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

import ipaddress
import json
from socket import gethostbyname, gaierror
from subprocess import CalledProcessError
from typing import Dict, List, Iterable, cast

from telepresence.connect import SSH
from telepresence.proxy import RemoteInfo
from telepresence.runner import Runner
from telepresence.utilities import random_name
from telepresence.startup import KubeInfo


def covering_cidr(ips: List[str]) -> str:
    """
    Given list of IPs, return CIDR that covers them all.

    Presumes it's at least a /24.
    """

    def collapse(ns: Iterable[ipaddress.IPv4Network]
                 ) -> List[ipaddress.IPv4Network]:
        return list(ipaddress.collapse_addresses(ns))

    assert len(ips) > 0
    networks = collapse([
        ipaddress.IPv4Interface(ip + "/24").network for ip in ips
    ])
    # Increase network size until it combines everything:
    while len(networks) > 1:
        networks = collapse([networks[0].supernet()] + networks[1:])
    return networks[0].with_prefixlen


# Script to dump resolved IPs to stdout as JSON list:

_GET_IPS_PY = """
import socket, sys, json

result = []
for host in sys.argv[1:]:
    result.append(socket.gethostbyname(host))
sys.stdout.write(json.dumps(result))
sys.stdout.flush()
"""


def get_proxy_cidrs(
    runner: Runner, remote_info: RemoteInfo, hosts_or_ips: List[str]
) -> List[str]:
    """
    Figure out which IP ranges to route via sshuttle.

    1. Given the IP address of a service, figure out IP ranges used by
       Kubernetes services.
    2. Extract pod ranges from API.
    3. Any hostnames/IPs given by the user using --also-proxy.

    See https://github.com/kubernetes/kubernetes/issues/25533 for eventual
    long-term solution for service CIDR.
    """
    span = runner.span()

    # Run script to convert --also-proxy hostnames to IPs, doing name
    # resolution inside Kubernetes, so we get cloud-local IP addresses for
    # cloud resources:
    result = set(k8s_resolve(runner, remote_info, hosts_or_ips))
    assert isinstance(runner.kubectl, KubeInfo)
    context_cache = runner.cache.child(runner.kubectl.context)
    result.update(
        cast(
            Iterable[str],
            context_cache.lookup("podCIDRs", lambda: podCIDRs(runner))
        )
    )
    result.add(
        cast(
            str,
            context_cache.lookup("serviceCIDR", lambda: serviceCIDR(runner))
        )
    )

    span.end()
    return list(result)


def k8s_resolve(
    runner: Runner, remote_info: RemoteInfo, hosts_or_ips: List[str]
) -> List[str]:
    """
    Resolve a list of host and/or ip addresses inside the cluster
    using the context, namespace, and remote_info supplied. Note that
    if any hostname fails to resolve this will fail Telepresence.
    """
    # Separate hostnames from IPs and IP ranges
    hostnames = []
    ip_ranges = []

    assert isinstance(runner.kubectl, KubeInfo)
    ipcache = cast(
        Dict[str, str],
        runner.cache.child(runner.kubectl.context).child("ips")
    )

    for proxy_target in hosts_or_ips:
        try:
            addr = ipaddress.ip_network(proxy_target)
        except ValueError:
            pass
        else:
            ip_ranges.append(str(addr))
            continue

        if proxy_target in ipcache:
            ip_ranges.append(ipcache[proxy_target])
            continue

        hostnames.append(proxy_target)

    if hostnames:
        try:
            resolved_ips: List[str] = json.loads(
                runner.get_output(
                    runner.kubectl(
                        "exec", "--container=" + remote_info.container_name,
                        remote_info.pod_name, "--", "python3", "-c",
                        _GET_IPS_PY, *hostnames
                    )
                )
            )
        except CalledProcessError as e:
            runner.write(str(e))
            raise runner.fail(
                "We failed to do a DNS lookup inside Kubernetes for the "
                "hostname(s) you listed in "
                "--also-proxy ({}). Maybe you mistyped one of them?".format(
                    ", ".join(hosts_or_ips)
                )
            )
    else:
        resolved_ips = []

    for host, ip in zip(hostnames, resolved_ips):
        ipcache[host] = ip

    return resolved_ips + ip_ranges


def podCIDRs(runner: Runner) -> List[str]:
    """
    Get pod IPs from nodes if possible, otherwise use pod IPs as heuristic:
    """
    cidrs = set()
    try:
        nodes = json.loads(
            runner.get_output(runner.kubectl("get", "nodes", "-o", "json"))
        )["items"]
    except CalledProcessError as e:
        runner.write("Failed to get nodes: {}".format(e))
        # Fallback to using pod IPs:
        pods = json.loads(
            runner.get_output(runner.kubectl("get", "pods", "-o", "json"))
        )["items"]
        pod_ips = []
        for pod in pods:
            try:
                pod_ips.append(pod["status"]["podIP"])
            except KeyError:
                # Apparently a problem on OpenShift
                pass
        if pod_ips:
            cidrs.add(covering_cidr(pod_ips))
    else:
        for node in nodes:
            pod_cidr = node["spec"].get("podCIDR")
            if pod_cidr is not None:
                cidrs.add(pod_cidr)
    return list(cidrs)


def serviceCIDR(runner: Runner) -> str:
    """
    Get service IP range, based on heuristic of constructing CIDR from
    existing Service IPs. We create more services if there are less
    than 8, to ensure some coverage of the IP range.
    """

    def get_service_ips() -> List[str]:
        services = json.loads(
            runner.get_output(runner.kubectl("get", "services", "-o", "json"))
        )["items"]
        # FIXME: Add test(s) here so we don't crash on, e.g., ExternalName
        return [
            svc["spec"]["clusterIP"] for svc in services
            if svc["spec"].get("clusterIP", "None") != "None"
        ]

    service_ips = get_service_ips()
    new_services: List[str] = []
    # Ensure we have at least 8 ClusterIP Services:
    while len(service_ips) + len(new_services) < 8:
        new_service = random_name()
        runner.check_call(
            runner.kubectl(
                "create", "service", "clusterip", new_service, "--tcp=3000"
            )
        )
        new_services.append(new_service)
    if new_services:
        service_ips = get_service_ips()
    # Store Service CIDR:
    service_cidr = covering_cidr(service_ips)
    # Delete new services:
    for new_service in new_services:
        runner.check_call(runner.kubectl("delete", "service", new_service))

    if runner.chatty:
        runner.show(
            "Guessing that Services IP range is {}. Services started after"
            " this point will be inaccessible if are outside this range;"
            " restart telepresence if you can't access a "
            "new Service.\n".format(service_cidr)
        )
    return service_cidr


def connect_sshuttle(
    runner: Runner, remote_info: RemoteInfo, hosts_or_ips: List[str], ssh: SSH
) -> None:
    """Connect to Kubernetes using sshuttle."""
    span = runner.span()
    sshuttle_method = "auto"
    if runner.platform == "linux":
        # sshuttle tproxy mode seems to have issues:
        sshuttle_method = "nat"
    runner.launch(
        "sshuttle",
        [
            "sshuttle-telepresence",
            "-v",
            "--dns",
            "--method",
            sshuttle_method,
            "-e",
            (
                "ssh -oStrictHostKeyChecking=no " +
                "-oUserKnownHostsFile=/dev/null -F /dev/null"
            ),
            # DNS proxy running on remote pod:
            "--to-ns",
            "127.0.0.1:9053",
            "-r",
            "telepresence@127.0.0.1:" + str(ssh.port),
        ] + get_proxy_cidrs(runner, remote_info, hosts_or_ips),
        keep_session=True,  # Avoid trouble with interactive sudo
    )

    # sshuttle will take a while to startup. We can detect it being up when
    # DNS resolution of services starts working. We use a specific single
    # segment so any search/domain statements in resolv.conf are applied,
    # which then allows the DNS proxy to detect the suffix domain and
    # filter it out.
    # On Macs, and perhaps elsewhere, there is OS-level caching of
    # NXDOMAIN, so bypass caching by sending new domain each time. Another,
    # less robust alternative, is to `killall -HUP mDNSResponder`.
    subspan = runner.span("sshuttle-wait")
    countdown = 3
    for idx in runner.loop_until(25, 0.1):
        # Construct a different name each time to avoid NXDOMAIN caching.
        name = "hellotelepresence-{}".format(idx)
        runner.write("Wait for vpn-tcp connection: {}".format(name))
        try:
            gethostbyname(name)
            countdown -= 1
            runner.write("Resolved {}. {} more...".format(name, countdown))
            if countdown == 0:
                break
        except gaierror:
            pass
        try:
            # The loop uses a single segment to try to capture suffix or
            # search path in the proxy. However, in some network setups,
            # single-segment names don't get resolved the normal way. To see
            # whether we're running into this, also try to resolve a name with
            # many dots. This won't resolve successfully but will show up in
            # the logs. See also:
            # https://github.com/telepresenceio/telepresence/issues/242
            gethostbyname("{}.a.sanity.check.telepresence.io".format(name))
        except gaierror:
            pass

    if countdown != 0:
        raise RuntimeError("vpn-tcp tunnel did not connect")

    subspan.end()
    span.end()
