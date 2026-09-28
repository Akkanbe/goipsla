# net-snmp for the P7 staging checks: snmpd as the AgentX master for
# goipslad, snmptrapd to receive its traps, and the snmp* client tools.
FROM debian:trixie-slim
RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends snmpd snmptrapd snmp \
 && rm -rf /var/lib/apt/lists/*
# Resolve names with the repository's MIBs (mounted at /mibs). Debian's slim
# image has no IETF MIBs (they are in the non-free snmp-mibs-downloader), so
# name the modules to load instead of adding to net-snmp's default list,
# which would warn about every missing one.
ENV MIBDIRS=/mibs:/usr/share/snmp/mibs \
    MIBS=SNMPv2-MIB:IF-MIB:CISCO-RTTMON-MIB:CISCO-RTTMON-ICMP-MIB
